package main

// 1.8.35 性能修复的回归测试。
//
// 背景（用户实测）：打开未缓存目录时，前台网格请求与后台预生成各自计数、
// 互不知情，峰值并发 = thumbConcurrency(默认 6) + preloadConcurrency(默认 3) = 9 个
// 解码任务同时抢 CPU；此时点开大图，要么在浏览器连接池里排队，要么在服务端
// 和后台抢 CPU —— 表现就是「点了要等好几秒」。
//
// 本文件守住四件事：
//   1. 后台只能用「前台用不到的槽」，总并发不再叠加；
//   2. 用户正在交互时后台主动静默（静默期）；
//   3. pause 是抢占式的：清空待办 + 取消在跑的后台任务（连带 kill ffmpeg）；
//   4. /api/raw 的 maxdim 有服务端硬上限。

import (
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withThumbConcurrency 在测试期内把缩略图并发改成 n，并重置信号量
// （thumbGenSem 只在首次调用时按当时的配置创建）。
func withThumbConcurrency(t *testing.T, n int) {
	t.Helper()
	settingsMu.Lock()
	old := settings
	settings.ThumbEnabled = true
	settings.ThumbConcurrency = n
	settingsMu.Unlock()

	thumbSemMu.Lock()
	oldSem := thumbGenSem
	thumbGenSem = nil
	thumbSemMu.Unlock()

	t.Cleanup(func() {
		settingsMu.Lock()
		settings = old
		settingsMu.Unlock()
		thumbSemMu.Lock()
		thumbGenSem = oldSem
		thumbSemMu.Unlock()
	})
}

// 后台不能占用前台正在用的槽：这是「并发不再叠加」的核心保证。
func TestBackgroundOnlyUsesIdleSlots(t *testing.T) {
	withThumbConcurrency(t, 2)
	sem := getThumbSem()
	if cap(sem) != 2 {
		t.Fatalf("信号量容量 = %d，期望 2", cap(sem))
	}
	// 前台占满
	sem <- struct{}{}
	sem <- struct{}{}
	if tryAcquireThumbSlot() {
		t.Fatal("前台已占满时后台仍抢到了槽 —— 前后台并发会重新叠加")
	}

	// 等不到槽必须放弃而不是无限等：把等待上限压到 50ms
	oldWait, oldPoll := bgMaxWait, bgPollInterval
	bgMaxWait, bgPollInterval = 50*time.Millisecond, 10*time.Millisecond
	defer func() { bgMaxWait, bgPollInterval = oldWait, oldPoll }()

	oldQuiet := thumbQuietPeriod
	thumbQuietPeriod = 0 // 排除静默期干扰，本用例专测「抢槽」
	defer func() { thumbQuietPeriod = oldQuiet }()

	ctx := currentBackgroundCtx()
	if waitBackgroundTurn(ctx, ctx) {
		t.Fatal("前台占满时 waitBackgroundTurn 不该返回 true")
	}

	// 释放一个槽：后台**仍然**不抢 —— 这一个空槽是给前台预留的
	releaseThumbSlot()
	if tryAcquireThumbSlot() {
		t.Fatal("只剩一个空余槽时后台不该抢：那是给前台预留的，否则用户滚动会排到后台后面")
	}
	// 再释放一个（全部空闲）后，后台才能拿到槽
	releaseThumbSlot()
	if !tryAcquireThumbSlot() {
		t.Fatal("槽全部空闲时后台应能抢到")
	}
	releaseThumbSlot()
}

// 用户交互期间后台必须静默。
func TestForegroundQuietGate(t *testing.T) {
	old := thumbQuietPeriod
	defer func() { thumbQuietPeriod = old }()
	thumbQuietPeriod = 30 * time.Millisecond

	touchThumbActivity()
	if thumbForegroundQuiet() {
		t.Fatal("刚有前台请求，后台不该判定为已静默")
	}
	time.Sleep(60 * time.Millisecond)
	if !thumbForegroundQuiet() {
		t.Fatal("超过静默期后仍判定为不静默，后台永远起不来")
	}
}

// pause 必须是抢占式的：清空待办 + 取消在跑的后台任务。
func TestPausePreemptsBackground(t *testing.T) {
	oldQueue := thumbQueue
	thumbQueue = make(chan thumbReq, 8)
	defer func() { thumbQueue = oldQueue }()
	for i := 0; i < 3; i++ {
		thumbQueue <- thumbReq{path: "/tmp/nonexistent.jpg", kind: "image", size: 320}
	}

	// 模拟一个「正在跑」的后台任务：它持有当前这把 ctx
	running := currentBackgroundCtx()
	if running.Err() != nil {
		t.Fatal("新建的后台 ctx 不该已被取消")
	}

	w := httptest.NewRecorder()
	handlePauseThumb(w, httptest.NewRequest("POST", "/api/thumb/pause", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("pause 返回 %d，期望 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("pause 响应异常：%s", w.Body.String())
	}
	if running.Err() == nil {
		t.Error("pause 未取消在跑的后台 ctx —— 已在解码的 ffmpeg 不会被 kill，仍会和新开的大图抢 CPU")
	}
	// 待办任务必须**还在**：清空队列会让这批图永久不再被预热
	// （用户可能永远不滚到它们，前台按需生成也就永远不会触发）。
	if n := len(thumbQueue); n != 3 {
		t.Errorf("暂停丢弃了待办任务：队列里还剩 %d 个，应为 3", n)
	}

	w2 := httptest.NewRecorder()
	handleResumeThumb(w2, httptest.NewRequest("POST", "/api/thumb/resume", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("resume 返回 %d，期望 200", w2.Code)
	}
	thumbPauseMu.Lock()
	paused := thumbPaused
	thumbPauseMu.Unlock()
	if paused {
		t.Error("resume 后仍处于暂停状态")
	}
	if err := currentBackgroundCtx().Err(); err != nil {
		t.Errorf("resume 后应拿到干净的后台 ctx，实际 %v", err)
	}
}

// maxdim 必须有服务端硬上限：手工拼 URL 传超大值不能让 ffmpeg 做全尺寸重编码。
func TestClampMaxdim(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 0}, {0, 0}, {1, 1}, {320, 320}, {2048, 2048},
		{4096, 4096}, {4097, maxAllowedMaxdim}, {99999, maxAllowedMaxdim},
	}
	for _, c := range cases {
		if got := clampMaxdim(c.in); got != c.want {
			t.Errorf("clampMaxdim(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 端到端：前台在跑时，后台不得让总并发超过 ThumbConcurrency。
// 这条直接对应原始现象 ——「打开未缓存目录时缩略图大批量生成，
// 把用户点开的大图挤到后面等」。
func TestPeakThumbConcurrencyRespectsLimit(t *testing.T) {
	withThumbConcurrency(t, 2)

	// 缩略图写到临时目录，别污染源码目录
	settingsMu.Lock()
	oldDir := settings.ThumbDir
	settings.PreloadConcurrency = 3
	settings.ThumbDir = t.TempDir()
	settingsMu.Unlock()
	applyThumbRoot()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbDir = oldDir
		settingsMu.Unlock()
		applyThumbRoot()
	})

	oldQuiet := thumbQuietPeriod
	thumbQuietPeriod = 0 // 后台立刻可跑，本用例专测并发上限
	defer func() { thumbQuietPeriod = oldQuiet }()

	atomic.StoreInt64(&thumbGenPeak, 0)

	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		img := image.NewRGBA(image.Rect(0, 0, 900, 900))
		for i := range img.Pix { // 填点花样，避免 PNG 压得太小、解码快到测不出重叠
			img.Pix[i] = uint8(i*7 + i/13)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var fg, bg []string
	for i := 0; i < 4; i++ {
		fg = append(fg, mk("fg"+strconv.Itoa(i)+".png"))
	}
	for i := 0; i < 6; i++ {
		bg = append(bg, mk("bg"+strconv.Itoa(i)+".png"))
	}

	// 拉起后台 worker
	oldCtx := thumbCtx
	thumbCtx = context.Background()
	t.Cleanup(func() { thumbCtx = oldCtx })
	spawnThumbWorkers()
	t.Cleanup(func() {
		thumbWorkerMu.Lock()
		if thumbWorkerCancel != nil {
			thumbWorkerCancel()
			thumbWorkerCancel = nil
		}
		thumbWorkerMu.Unlock()
	})
	for _, p := range bg {
		enqueueThumb(p, "image", 64)
	}

	// 前台 4 个请求同时打进来
	var wg sync.WaitGroup
	for _, p := range fg {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			_, _ = ensureThumb(p, "image", 64)
		}(p)
	}
	wg.Wait()

	// 等后台收尾
	for i := 0; i < 100 && atomic.LoadInt64(&thumbGenActive) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	limit := int64(cap(getThumbSem()))
	peak := atomic.LoadInt64(&thumbGenPeak)
	if peak > limit {
		t.Fatalf("缩略图解码并发峰值 = %d，超过上限 %d —— 前台与后台又叠加了", peak, limit)
	}
	t.Logf("并发峰值 = %d（上限 %d）", peak, limit)
}

// 前端这三块是「打开大图不再干等」的关键，被回退时必须被拦住。
func TestFrontendViewerResponsivenessPieces(t *testing.T) {
	js := fetchFrontend(t, "/app.js").Body.String()
	css := fetchFrontend(t, "/style.css").Body.String()
	for _, s := range []string{
		"suspendGridRequests", "resumeGridRequests",
		"attachHoverPrefetch", "X-MediaView-Prefetch", "v-spin-box", "PICK_STEP",
		"syncTrackToIndex", "sameFile",
	} {
		if !strings.Contains(js, s) {
			t.Errorf("app.js 里缺少 %q —— 大图响应性修复被回退了？", s)
		}
	}
	// 悬停预取必须用 fetch 发（URL 与打开时一致，才能命中浏览器缓存）。
	// 用 <img> 预取时 URL 上的 prefetch 参数会让缓存分键，等于白预取。
	if !strings.Contains(js, "fetch(url, { headers: { 'X-MediaView-Prefetch': '1' }") {
		t.Error("悬停预取没有改成 fetch + 请求头 —— URL 与打开时不一致，浏览器缓存不会命中")
	}
	// 当前图被「重建」是闪屏的唯一来源：重建会新建 <img>，而新图在 decode 前是
	// visibility:hidden，用户看到的就是「图片消失一下再出现」。
	if strings.Contains(js, "rebuildTrackInPlace(true)") {
		t.Error("又出现了 rebuildTrackInPlace(true) —— 它会重建当前图，图片将「消失再出现」地闪")
	}
	// 光有函数不算数：打开查看器时必须真的调用挂起
	if !strings.Contains(js, "suspendGridRequests();") {
		t.Error("app.js 里没有任何地方调用 suspendGridRequests()")
	}
	// 网格 cell 必须真的挂上悬停预取
	if !strings.Contains(js, "attachHoverPrefetch(inner, item)") {
		t.Error("createCell 没有给 cell 挂悬停预取")
	}
	// 模糊占位已按实测反馈撤掉，别再回来（几何与亮度跳变就是"晃眼"的来源）
	if strings.Contains(js, "v-placeholder") || strings.Contains(css, "v-placeholder") {
		t.Error("前端又出现了模糊占位 —— 实测晃眼，已改为「转圈 + 悬停预取」")
	}
	// 转圈必须真的进 DOM 且有样式
	if !strings.Contains(js, "v-spin") {
		t.Error("app.js 里没有 v-spin 加载指示")
	}
	if !strings.Contains(css, ".v-spin") {
		t.Error("style.css 里缺少 .v-spin —— 加载指示不可见")
	}
}

// 打开（独立窗口）与动画中连点都不得重建当前图 —— 这是「闪屏」的唯一来源。
// 回归背景：悬停预取让首图秒出后，openSingle 里"先显示一次、列表返回再重建一次"
// 的双重加载才暴露出来（过去转码要等几秒，重建发生在图片显示之前，看不见）。
func TestNoCurrentImageRebuild(t *testing.T) {
	js := fetchFrontend(t, "/app.js").Body.String()
	// openSingle 的列表返回必须带「是否同一文件」判定
	if !strings.Contains(js, "var sameFile = shown") {
		t.Error("openSingle 缺少 sameFile 判定：列表返回后会重建 current → 打开时闪屏")
	}
	// 动画中连点必须走 syncTrackToIndex（复用已有元素），而不是重建轨道
	if !strings.Contains(js, "syncTrackToIndex(track)") {
		t.Error("swipeTo 的动画分支没有使用 syncTrackToIndex —— 连点切换会重建 current → 闪屏")
	}
	if strings.Contains(js, "rebuildTrackInPlace(true)") {
		t.Error("仍存在 rebuildTrackInPlace(true)：它每次都会新建 <img>，图片会「消失再出现」")
	}
	// createMediaBox 必须记录来源路径，否则 syncTrackToIndex 无从复用
	if !strings.Contains(js, "box._itemPath = item.path") {
		t.Error("createMediaBox 没有记录 _itemPath，syncTrackToIndex 无法复用已有元素")
	}
}

// 悬停预取在转码槽位占满时必须立刻放弃，而不是排队。
// 否则「提前帮你转好」的善意会变成挡住用户真正点击的那次请求。
// 两种识别方式（请求头 / 旧查询参数）都必须生效。
func TestPrefetchGivesUpInsteadOfQueueing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	img := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for i := range img.Pix {
		img.Pix[i] = uint8(i*11 + i/7)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// 占满转码槽，模拟"用户刚点开一张大图，另一个槽也被预加载占着"
	slots := cap(scaledSem)
	for i := 0; i < slots; i++ {
		scaledSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < slots; i++ {
			<-scaledSem
		}
	}()

	for _, tc := range []struct {
		name   string
		url    string
		header bool
	}{
		{"请求头 X-MediaView-Prefetch", "/api/raw?path=x&maxdim=1024", true},
		{"旧查询参数 prefetch=1", "/api/raw?path=x&maxdim=1024&prefetch=1", false},
	} {
		req := httptest.NewRequest("GET", tc.url, nil)
		if tc.header {
			req.Header.Set("X-MediaView-Prefetch", "1")
		}
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func(rq *http.Request, rc *httptest.ResponseRecorder) {
			serveScaledImage(rc, rq, p, 1024)
			close(done)
		}(req, rec)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s：槽位占满时预取没有立即放弃 —— 它会挡住用户真正点击的请求", tc.name)
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s：预取抢不到槽时应返回 204，实际 %d", tc.name, rec.Code)
		}
	}
}

// 独立窗口打开第一张图时就要把文件名写进窗口标题栏。
// 回归背景：宿主握手是**异步**的，首图显示时 host 还没连上，那一次 setTitle 会被跳过 ——
// 现象是「刚打开时左上角标题栏空白，切到下一张才出现文件名」。
func TestWindowTitleSetForFirstImage(t *testing.T) {
	js := fetchFrontend(t, "/app.js").Body.String()
	if !strings.Contains(js, "function updateWindowTitle()") {
		t.Error("缺少 updateWindowTitle()：标题设置逻辑没有收拢成一个函数，容易漏路径")
	}
	// 四处都要调用：showCurrent（首图/换图）、settleAfterSwipe（滑动切换）、
	// swipeTo 的动画分支（连点）、hostConnect().then（握手完成补设）
	if n := strings.Count(js, "updateWindowTitle();"); n < 4 {
		t.Errorf("updateWindowTitle() 只被调用 %d 次，应至少 4 处（首图 / 滑动 / 连点 / 握手补设）", n)
	}
	// 关键的一条：握手完成后必须补设，否则首图标题会一直空着
	if !strings.Contains(js, "hostConnect().then(function () { updateWindowTitle(); })") {
		t.Error("hostConnect() 没有在完成后补设标题 —— 首图打开时标题栏仍会是空白")
	}
	// 旧写法（散落各处的 singleMode && item && item.name）不应再出现
	if strings.Contains(js, "singleMode && item && item.name") {
		t.Error("标题设置又散落回各处了，应统一走 updateWindowTitle()")
	}
}
