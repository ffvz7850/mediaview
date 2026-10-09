package main

// 1.8.66：大图浏览期间，缩略图请求「不生成、快速返回占位」。
//
// 这一版把 1.8.57 引入的「挂住式让路」整个换掉了。原因是一条实测确认的硬约束：
// 浏览器对同一 origin 只有 6 条 HTTP/1.1 连接，而在 handler 里挂住 = 占着连接不放，
// 结果是**大图请求自己也发不出去**（浏览器排不到空连接）—— 让得越久大图越打不开，
// 正好挡住它要保护的那张图。把窗口从 10s 收到 1.5s 只是减轻，消除不了。
//
// 正确做法：暂停期间**未命中**的请求立刻返回占位图（200 + 1x1 灰 + no-store）：
// 网格请求秒回 → 连接立刻释放 → 大图请求马上拿得到连接；同时 CPU 不跑 ffmpeg。
// 代价是网格里没生成出来的位置暂时是灰块，所以把这些请求记进待补齐队列，
// resume（用户关掉大图窗口）后由后台补生成，用户滚动回来就是缓存命中。
//
// 触发源仍是两条（与 1.8.57 相同）：
//  1. mediaview 自己的查看器打开 → POST /api/thumb/pause；
//  2. 飞牛文件管理器打开大图预览 → GET /thumb/getIcon?size=big。
//
// 为什么在 handler 层测：判定与「返回占位」都在 handler 里（生成层只剩预留槽），
// 在生成层断言拿不到这个行为。

import (
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// withThumbRootTemp 把缩略图根目录改到临时目录，避免污染源码目录。
func withThumbRootTemp(t *testing.T) {
	t.Helper()
	settingsMu.Lock()
	old := settings.ThumbDir
	settings.ThumbDir = t.TempDir()
	settingsMu.Unlock()
	applyThumbRoot()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbDir = old
		settingsMu.Unlock()
		applyThumbRoot()
	})
}

// pauseViaHTTP 通过真实 HTTP handler 进入暂停态（不直接写全局变量，
// 这样「谁在写 pause」这件事本身也被覆盖）。
func pauseViaHTTP(t *testing.T) {
	t.Helper()
	w := httptest.NewRecorder()
	handlePauseThumb(w, httptest.NewRequest("POST", "/api/thumb/pause", nil))
	if w.Code != 200 {
		t.Fatalf("pause 返回 %d，期望 200", w.Code)
	}
}

func resumeViaHTTP(t *testing.T) {
	t.Helper()
	w := httptest.NewRecorder()
	handleResumeThumb(w, httptest.NewRequest("POST", "/api/thumb/resume", nil))
	if w.Code != 200 {
		t.Fatalf("resume 返回 %d，期望 200", w.Code)
	}
}

// writeRelativePNG 在 CWD 下写一张 PNG 并返回**相对路径**。
// 必须用相对路径：thumbPathFor 在 Windows 上处理带盘符的绝对路径会把 "C:" 当目录名。
func writeRelativePNG(t *testing.T, name string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for i := range img.Pix {
		img.Pix[i] = uint8(i*7 + i/13)
	}
	f, err := os.Create(name)
	if err != nil {
		t.Fatalf("建样张失败: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("写样张失败: %v", err)
	}
	f.Close()
	t.Cleanup(func() { _ = os.Remove(name) })
	return name
}

// defaultThumbSizeForTest 取当前设置里的缩略图边长（并做与 handler 一致的兜底）。
func defaultThumbSizeForTest() int {
	size := getSettings().ThumbSize
	if size < 64 || size > 640 {
		size = 320
	}
	return size
}

func readSourceOrSkip(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("读不到 %s（测试 CWD 不是源码目录）：%v", name, err)
	}
	// 1.8.150：契约测试不该对**行尾**敏感 —— 源码被 CRLF 检出时，
	// 跨行字面量断言（"...;\n      jumpToIndex(...)"）会永远匹配不上、造成假红。
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// stripLineComments 去掉每行 `//` 之后的内容（含 `//` 本身）。
//
// 源码断言必须只看可执行代码：注释里提到某个函数名不代表真的调用了它。
// 1.8.66 变异测试 M11 就栽在这上面 —— 删掉真实调用后，注释里那句
// 「① noteForegroundPreview()」让 Contains 继续命中，测试照绿。
// 本仓源码无块注释（/* */）与含 `//` 的字符串字面量，行注释剥除是安全的。
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "//"); idx >= 0 {
			lines[i] = ln[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// ---- 暂停态的判定本身 ----

func Test1866ForegroundPreviewActiveFollowsPauseResume(t *testing.T) {
	resumeViaHTTP(t)
	if foregroundPreviewActive() {
		t.Fatal("resume 之后 foregroundPreviewActive 仍为 true")
	}
	pauseViaHTTP(t)
	if !foregroundPreviewActive() {
		t.Fatal("pause 之后 foregroundPreviewActive 应为 true —— 判据取不到暂停态，整条「暂停期间不生成」就是死的")
	}
	resumeViaHTTP(t)
	if foregroundPreviewActive() {
		t.Fatal("resume 之后 foregroundPreviewActive 仍为 true")
	}
}

func Test1866ForegroundPreviewActiveExpiresAtTTL(t *testing.T) {
	// 到点自动恢复：前端异常关窗时收不到 resume，靠 TTL 兜底
	resumeViaHTTP(t)
	thumbPauseMu.Lock()
	thumbPaused = true
	thumbPauseUntil = time.Now().Add(-1 * time.Second) // 已过期
	thumbPauseMu.Unlock()
	t.Cleanup(func() { resumeViaHTTP(t) })
	if foregroundPreviewActive() {
		t.Fatal("暂停已过期，foregroundPreviewActive 应为 false（否则文件管理器会被永久挡住）")
	}
}

// ---- 核心行为：暂停期间不生成，立刻回占位 ----

// resume 之后，被占位挡回去的那些图必须**自动补生成** —— 这就是
// 「关掉大图浏览窗口后再继续生成」。

// ---- 占位图本身 ----

// ---- 接线守卫：两处入口都要挂暂停判定，且 big 必须排除 ----

func Test1866PauseGateWiredOnBothEntrypoints(t *testing.T) {
	// 断言前必须先剥掉注释。1.8.66 变异测试里 M11 就是这么漏网的：
	// 真实调用 `noteForegroundPreview()` 被整块删掉后，上方那段
	// 「① noteForegroundPreview()：后台预生成整体停摆」的注释仍让 Contains 命中，
	// 测试照绿。源码断言一律针对**可执行代码**，不针对文字说明。
	st := stripLineComments(readSourceOrSkip(t, "systemthumb.go"))

	// 1.8.100：占位图让路已按用户要求**撤销**（宁可慢，也不要白缩略图）。
	// 用户实测反馈：占位图会让网格留下几张空白缩略图，且拦不住已经发出的整页请求，
	// 收益不抵副作用。所以这里反转断言 —— 不得再出现任何占位接管调用。
	// 1.8.112：恢复让路 —— 1.8.100 撤掉它是因为当时占位图是**浅灰 JPEG**（深色网格上
	// 就是一块白）。改成**透明 GIF** 后白块问题消失，而收益必须保留：飞牛文件管理器一次
	// 发出整页/两页请求，mediaview 的前端并发闸管不到它，只能靠后端快速还响应，
	// 否则它的 JS 主线程被几十个 onload 占满，用户点大图要排在这些回调后面。
	if !strings.Contains(st, "tryServeDeferredThumb(") {
		t.Error("systemthumb.go 缺少占位让路 —— 飞牛文件管理器里点大图要等整页缩略图才响应")
	}
	// 占位图的 Content-Type 断言必须锚定**可观察行为**，不能 grep 源码里是否出现
	// "image/gif" —— thumb.go 第 8 行有一句 `_ "image/gif" // 注册 GIF 解码器`，
	// 那是解码器副作用导入，会让断言永远为真（代码审查 P0-1 指出的假绿）。
	// 这里改成直接检查占位图写出口的 Content-Type 行。
	th2 := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	if !strings.Contains(th2, `Set("Content-Type", "image/gif")`) {
		t.Error(`占位图的 Content-Type 不是 image/gif —— ` +
			`要么是浅色 JPEG（深色网格上像白块），要么断言又被无关文本蒙混`)
	}
	// 必须是「被 size==\"big\" 条件包住的真实调用」，不是随便哪里提一句。
	// 光看 Contains("noteForegroundPreview()") 会被注释骗过去。
	// 1.8.118：允许 size=="big" 分支里插入诊断日志，所以不再要求两行紧邻。
	if !strings.Contains(st, "size == \"big\"") || !strings.Contains(st, "noteForegroundPreview()") {
		t.Error("systemthumb.go 丢了 size=big 的暂停触发点（1.8.56 误删过一次）—— " +
			"打开大图预览时后台预生成不会停摆")
	}
	if !strings.Contains(st, "touchThumbActivity()") {
		t.Error("handleSystemThumbGetIcon 丢了 touchThumbActivity() —— " +
			"文件管理器整页加载时后台预生成不会让路")
	}
	// 生成路径仍在（暂停只挡新增，不改变正常路径的存在）
	if !strings.Contains(st, "ensureThumbSys(") {
		t.Error("systemthumb.go 不再调用 ensureThumbSys —— 文件管理器的缩略图请求失去了预留槽能力")
	}

	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	// 1.8.100：/api/thumb 同样不得再走占位图接管。
	if strings.Contains(th, "tryServeDeferredThumb(w, r, path, size,") {
		t.Error("thumb.go 的 /api/thumb 又接上了占位图让路（1.8.100 已撤销）")
	}
	// 后台预生成的让路仍然必须保留 —— 它只停后台任务，不产生占位图、不影响前台。
	if !strings.Contains(st, "noteForegroundPreview()") {
		t.Error("systemthumb.go 丢了 noteForegroundPreview() —— 大图预览时后台预生成不会停摆")
	}
	// 1.8.120：挂起等待是**有意为之**（行为测试见 Test1866SuspendHasBoundedWait）。
	// 与 1.8.66 那版的区别：那时是「无差别挂起」，连打开大图的**导航请求**也被挂住
	// （它与 list 抢同 6 条连接），结果把大图自己挡死；现在只在**大图已经打开之后**
	// 挂起 —— 导航早已完成，大图不再需要新连接，所以是安全的。
	if !strings.Contains(th, "waitWhileForegroundPreview") {
		t.Error("挂起等待不见了 —— 大图期间会退回占位图，留下永久白格")
	}
	if strings.Contains(th, "sysThumbYieldMax") {
		t.Error("让路窗口常量又回来了 —— 1.8.66 已改为「不生成、快速返回占位」")
	}
}

// ---- 配置：预生成默认关闭 + 一次性迁移 ----

func Test1866PreloadDisabledEnqueuesNothing(t *testing.T) {
	settingsMu.Lock()
	oldPC := settings.PreloadConcurrency
	oldEnabled := settings.ThumbEnabled
	settings.ThumbEnabled = true
	settings.PreloadConcurrency = 0
	settingsMu.Unlock()

	// 必须装一个**真实队列**再断言。1.8.66 变异测试 M7 就是这样漏网的：
	// 原写法直接读全局 thumbQueue，而测试进程里它从未被 spawnThumbWorkers 建过 → 是 nil，
	// before/after 恒为 0，断言无条件成立，把 enqueueThumb 的早退整个删掉也照样绿。
	// 给 enqueueThumb 一个真能收到的队列，断言才有区分能力。
	thumbWorkerMu.Lock()
	oldQ := thumbQueue
	probe := make(chan thumbReq, 16)
	thumbQueue = probe
	thumbWorkerMu.Unlock()

	t.Cleanup(func() {
		thumbWorkerMu.Lock()
		thumbQueue = oldQ
		thumbWorkerMu.Unlock()
		settingsMu.Lock()
		settings.PreloadConcurrency = oldPC
		settings.ThumbEnabled = oldEnabled
		settingsMu.Unlock()
	})

	enqueueThumb("zz_1866_never.jpg", "image", 320)
	if n := len(probe); n != 0 {
		t.Fatalf("预生成已关闭（PreloadConcurrency=0），队列却收到 %d 条 —— 关不掉预生成", n)
	}

	// 正对照：同一个队列、同一个调用，把开关打开必须收到 1 条。
	// 没有这一步，「收到 0 条」既可能是真早退，也可能是队列压根没接上 —— 两者无法区分。
	settingsMu.Lock()
	settings.PreloadConcurrency = 3
	settingsMu.Unlock()
	enqueueThumb("zz_1866_never2.jpg", "image", 320)
	if n := len(probe); n != 1 {
		t.Fatalf("正对照失败：PreloadConcurrency=3 时队列收到 %d 条，期望 1 条 —— "+
			"说明这套观测机制本身不可靠，上一条断言不算数", n)
	}
}

// 迁移语义在 1.8.67 扩宽（每个旧默认值独立判断），断言迁到
// Test1867MigrationUpgradesEachLegacyDefaultIndependently。

// 默认缩略图并发按核数收敛（永远给前台留 2 个核），预生成 0（关闭）。
// 1.8.109 起不再是固定 3：4 核机器上「3 个缩略图转码 + 大图转码」会把核占满，
// 表现就是点开大图后干等（缩略图抢走了全部 CPU）。
func Test1866NewDefaultsAndRanges(t *testing.T) {
	d := defaultSettings()
	if want := thumbConcurrencyDefault(); d.ThumbConcurrency != want {
		t.Errorf("默认缩略图并发 = %d，期望 %d（按核数收敛）", d.ThumbConcurrency, want)
	}
	if d.ThumbConcurrency < 1 || d.ThumbConcurrency > maxThumbConcurrency {
		t.Errorf("默认缩略图并发 %d 超出范围 1~%d", d.ThumbConcurrency, maxThumbConcurrency)
	}
	// 1.8.135（选项3）：预生成默认改为开启（把等待提前到空闲时段）
	if want := defaultSettings().PreloadConcurrency; d.PreloadConcurrency != want {
		t.Errorf("默认预加载并发 = %d，期望 %d", d.PreloadConcurrency, want)
	}
	if d.PreloadConcurrency <= 0 {
		t.Error("默认预生成并发应 > 0（选项3 要求默认开启预生成）")
	}
	if maxThumbConcurrency != 6 {
		t.Errorf("maxThumbConcurrency = %d，期望 6", maxThumbConcurrency)
	}
	// 越界回落必须落到**新默认**，不能落到旧值
	s := Settings{ThumbConcurrency: 99, PreloadConcurrency: 99}
	s.normalize()
	// 越界回落必须落到**新默认**（按核数收敛），不能落到旧的硬编码 3
	if s.ThumbConcurrency != thumbConcurrencyDefault() || s.PreloadConcurrency > 2 {
		t.Errorf("越界回落得到 %d/%d，期望 %d/<=2",
			s.ThumbConcurrency, s.PreloadConcurrency, thumbConcurrencyDefault())
	}
	// 0 必须被保留（不能被当成越界值补成 1，否则用户设的「关闭」自己变回开启）
	s2 := Settings{ThumbConcurrency: 3, PreloadConcurrency: 0}
	s2.normalize()
	if s2.PreloadConcurrency != 0 {
		t.Errorf("PreloadConcurrency=0 被 normalize 改成 %d —— 用户设的「关闭后台预生成」被改掉了", s2.PreloadConcurrency)
	}
}

// ---- 1.8.57 起就成立、本版必须继续成立的边界 ----

// 「用户正在看大图」的触发点有两条，两条都得在：
//  1. 文件管理器大图预览 → /thumb/getIcon?size=big；
//  2. mediaview 自己打开查看器 → /api/raw 要转码时。
//
// 第二条的三条边界必须成立：要转码的大图触发、悬停预取不触发、缓存命中不触发。
func Test1857RawTranscodeTriggersPauseButPrefetchAndCacheHitDoNot(t *testing.T) {
	resumeViaHTTP(t)
	t.Cleanup(func() { resumeViaHTTP(t) })

	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skipf("无 ffmpeg，跳过：%v", err)
	}
	oldFF := ffmpegPath
	ffmpegPath = testFFmpeg
	t.Cleanup(func() { ffmpegPath = oldFF })

	p := writeRelativePNG(t, "zz_1857_raw.png")

	// ① 缓存未命中 → 真要起一次 ffmpeg：用户正在等这一张，必须触发暂停
	resumeViaHTTP(t)
	rec := httptest.NewRecorder()
	serveScaledImage(rec, httptest.NewRequest("GET", "/api/raw?path=x&maxdim=256", nil), p, 256)
	if !foregroundPreviewActive() {
		t.Error("缓存未命中的大图请求没触发「正在看大图」—— 后台预生成与网格会继续和用户正在等的这张抢 CPU")
	}
	if rec.Code != 200 {
		t.Skipf("本机没能生成缩放产物（HTTP %d），跳过后续两条边界断言", rec.Code)
	}

	// ② 悬停预取 → 不能触发。预取会连发好几张，每张都 kill 一轮在跑的后台 ffmpeg，
	//    会让预生成反复白干（解码到一半被杀 → 重新入队 → 又被杀）。
	resumeViaHTTP(t)
	req := httptest.NewRequest("GET", "/api/raw?path=x&maxdim=512", nil)
	req.Header.Set("X-MediaView-Prefetch", "1")
	serveScaledImage(httptest.NewRecorder(), req, p, 512)
	if foregroundPreviewActive() {
		t.Error("悬停预取触发了暂停 —— 鼠标划过网格就会把后台预生成一直摁住")
	}

	// ③ 缓存命中 → 不能触发。毫秒级返回，停后台没有收益，反而会在连翻已缓存图片时
	//    一直摁住预生成（①已经生成了 256 档的产物）。
	resumeViaHTTP(t)
	serveScaledImage(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/raw?path=x&maxdim=256", nil), p, 256)
	if foregroundPreviewActive() {
		t.Error("缓存命中的大图也触发了暂停 —— 连翻已缓存图片会把后台预生成一直摁住")
	}
}

// Test1866ThumbEngineSetting 守住「压缩引擎」开关的合法取值与非法回落。
func Test1866ThumbEngineSetting(t *testing.T) {
	for _, v := range []string{"auto", "vips", "ffmpeg"} {
		st := Settings{ThumbEngine: v}
		st.normalize()
		if st.ThumbEngine != v {
			t.Errorf("ThumbEngine=%q 经 normalize 变成 %q —— 合法值被改写", v, st.ThumbEngine)
		}
	}
	st := Settings{ThumbEngine: "nonsense"}
	st.normalize()
	if st.ThumbEngine != "auto" {
		t.Errorf("非法 ThumbEngine 回落为 %q，期望 auto", st.ThumbEngine)
	}
}

// TestVipsQualityFollowsSetting 守住「vips 路径的画质跟随设置」。
//
// 背景：tryVipsImageThumb 原来硬编码 `[Q=82,strip]`，设置里的 ThumbQuality
// 对 vips 路径完全无效（只有 ffmpeg/Go 响应）。用户调画质时 vips 出的图不变。
func TestVipsQualityFollowsSetting(t *testing.T) {
	raw := readSourceOrSkip(t, "thumb.go")
	th := stripLineComments(raw)
	// 注释里也不该留旧数值（"被证伪的注释是隐患"）
	if strings.Contains(raw, "Q=82") {
		t.Error("thumb.go 里仍有 Q=82（含注释）—— 会误导后续维护者以为画质是写死的")
	}
	if strings.Contains(th, "Q=82") {
		t.Error("代码里仍有硬编码的 Q=82 —— vips 路径的画质不会跟随设置")
	}
	if !strings.Contains(th, "vipsQualitySuffix()") {
		t.Error("vips 调用没有使用 vipsQualitySuffix() —— 画质可能又写死了")
	}
	// vipsQualitySuffix 必须真的读设置
	m := regexp.MustCompile(`func vipsQualitySuffix\(\) string \{[^}]*getSettings\(\)\.ThumbQuality[^}]*\}`)
	if !m.MatchString(th) {
		t.Error("vipsQualitySuffix 没有读取 getSettings().ThumbQuality —— 画质不跟随设置")
	}

	// 行为断言：改设置后后缀必须跟着变
	settingsMu.Lock()
	old := settings.ThumbQuality
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbQuality = old
		settingsMu.Unlock()
	})

	settingsMu.Lock()
	settings.ThumbQuality = 95
	settingsMu.Unlock()
	hi := vipsQualitySuffix()
	settingsMu.Lock()
	settings.ThumbQuality = 55
	settingsMu.Unlock()
	lo := vipsQualitySuffix()
	if hi == lo {
		t.Errorf("画质 95 与 55 得到同样的后缀 %q —— 设置没有生效", hi)
	}
	if !strings.Contains(hi, "95") || !strings.Contains(lo, "55") {
		t.Errorf("后缀没有反映设置值：hi=%q lo=%q", hi, lo)
	}
	t.Logf("Q=95 → %s ； Q=55 → %s", hi, lo)
}
