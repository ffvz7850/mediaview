package main

// 2.4.50：二次审查（mediaview_2.4.49-二次审查报告.md）修复项的**正向探针**。
//
// 这份文件只做一件事：把"修完之后必须成立"的行为钉住。报告里点名的几条
// 之所以能长期漏检，恰恰是因为原有用例都**绕开**了出问题的那个分支：
//   · #1 zzz_1850 靠 400ms ctx 超时让 waitWhilePaused 返回，从未走 TTL 分支；
//   · #2 zzz_1960 只做源码字符串断言，抓不到「n=0 起不到 worker」；
//   · #4/#5 完全没有「白名单外路径 / 非普通文件」的请求级用例；
//   · #11 Items[].Mtime 存了却从不比较，测试 fixture 用 time.Now() 近似，
//     正好掩盖了「不比较也能过」。
//
// 用例一律保持毫秒级、不依赖 ffmpeg：需要的只是几条 os.Stat / ReadDir
// 与一个能立刻失败的任务。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- #1 暂停 TTL 到点必须唤醒已阻塞的 worker ----------

// neverCancelCtx 是一个**永不取消**的 ctx：只给 #1 的探针用。
//
// 用它而不是 context.Background()+超时，是为了让"函数返回"只能由 TTL 唤醒解释 ——
// 原有用例（zzz_1850）用 400ms ctx 超时，正好让缺陷实现也能返回，于是永远抓不到。
type neverCancelCtx struct{}

func (neverCancelCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (neverCancelCtx) Done() <-chan struct{}       { return nil }
func (neverCancelCtx) Err() error                  { return nil }
func (neverCancelCtx) Value(any) any               { return nil }

func Test2450WaitWhilePausedWakesOnTTL(t *testing.T) {
	resumeViaHTTP1850(t) // 前置：确保不处于暂停态

	thumbPauseMu.Lock()
	thumbPaused = true
	thumbPauseUntil = time.Now().Add(150 * time.Millisecond)
	thumbPauseMu.Unlock()
	t.Cleanup(func() { resumeViaHTTP1850(t) })

	done := make(chan bool, 1)
	started := time.Now()
	go func() { done <- waitWhilePaused(neverCancelCtx{}) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("waitWhilePaused 返回 false（被当成取消），应当是 TTL 到期后恢复")
		}
		if el := time.Since(started); el > 2*time.Second {
			t.Errorf("TTL 到点后 %v 才返回，唤醒不及时", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("★★ TTL 到期 3 秒后 worker 仍阻塞在 waitWhilePaused —— " +
			"后台预生成永久停摆（二次审查 P1-1 回归）")
	}
}

// ---------- #2 后台预生成关闭时，手动通道必须真的有消费者 ----------

func Test2450ManualWorkersDrainQueueWithPreloadOff(t *testing.T) {
	withPreload(t, 0, true) // 复现 P1-2 的前提：PreloadConcurrency=0
	resumeViaHTTP1850(t)
	setThumbActivityAge(time.Hour) // 让 waitBackgroundTurn 立刻判定「用户已停手」

	// 保存并在结束时还原全局队列 / worker（别把临时 worker 留给后面的用例）
	thumbWorkerMu.Lock()
	oldQ, oldCancel := thumbQueue, thumbWorkerCancel
	thumbWorkerMu.Unlock()
	t.Cleanup(func() {
		thumbWorkerMu.Lock()
		if thumbWorkerCancel != nil {
			thumbWorkerCancel() // 停掉本用例拉起的临时 worker
		}
		thumbQueue, thumbWorkerCancel = oldQ, oldCancel
		thumbWorkerMu.Unlock()
	})

	// 这正是 ensureManualWorkers 的修法：**显式传数量**，而不是让它按
	// PreloadConcurrency（=0）算出一个空循环。
	spawnThumbWorkersWith(manualWorkerCount)
	q := currentThumbQueue()
	if q == nil {
		t.Fatal("spawnThumbWorkersWith 之后队列仍是 nil")
	}

	// 投 2 个注定失败的任务（路径不存在）：worker 会取走并放弃 ——
	// 队列被消费掉就证明「真的有消费者」，这正是原 bug 缺的东西。
	missing := filepath.Join(t.TempDir(), "nope.jpg")
	for i := 0; i < 2; i++ {
		q <- thumbReq{path: missing, kind: "image", size: 320}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(q) > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(q); n != 0 {
		t.Fatalf("★★ 队列里还剩 %d 个任务没被消费 —— PreloadConcurrency=0 时"+
			"「立即生成」投了没人跑（二次审查 P1-2 回归）", n)
	}

	// 静态闸门：ensureManualWorkers 必须走显式数量入口（防止有人改回无参调用）
	if src, err := os.ReadFile("thumb.go"); err == nil {
		s := string(src)
		i := strings.Index(s, "func ensureManualWorkers(")
		if i < 0 {
			t.Fatal("找不到 ensureManualWorkers")
		}
		seg := s[i:]
		if j := strings.Index(seg, "\nfunc "); j > 0 {
			seg = seg[:j]
		}
		if !strings.Contains(seg, "spawnThumbWorkersWith(manualWorkerCount)") {
			t.Error("★ ensureManualWorkers 又调回了按 PreloadConcurrency 取值的入口 —— " +
				"PreloadConcurrency=0 时 n=0，任务无人消费")
		}
	}
}

// ---------- #4 /api/pregen 必须过白名单、必须有 body 上限 ----------

func Test2450PregenRejectsOutsideWhitelist(t *testing.T) {
	installThumbQueueSpy(t, 8)
	withPreload(t, 2, true)
	base := withPregenRoot(t) // workDir = 临时目录（白名单根之一）

	outside := filepath.Join(filepath.Dir(base), "outside_2450.jpg") // 与 workDir 同级 ⇒ 不在白名单内
	body, _ := json.Marshal(map[string]any{"paths": []string{outside}, "manual": false})
	rec := httptest.NewRecorder()
	handlePregen(rec, httptest.NewRequest("POST", "/api/pregen", bytes.NewReader(body)))
	if n := pregenEnqueued(t, rec); n != 0 {
		t.Fatalf("★★ 白名单外的绝对路径被入队了（enqueued=%d）—— root 进程会替调用者"+
			"解码任意文件，产物还能经 /api/raw 取回（二次审查 P1-4）", n)
	}

	// 合法路径（workDir 之下）照旧通过：修复不能把正常链路一起挡掉
	ok := filepath.Join(base, "ok_2450.jpg")
	body2, _ := json.Marshal(map[string]any{"paths": []string{ok}, "manual": false})
	rec2 := httptest.NewRecorder()
	handlePregen(rec2, httptest.NewRequest("POST", "/api/pregen", bytes.NewReader(body2)))
	if n := pregenEnqueued(t, rec2); n != 1 {
		t.Fatalf("白名单内的路径应当入队 1 个，实际 %d", n)
	}
}

func Test2450PregenRejectsOversizedBody(t *testing.T) {
	installThumbQueueSpy(t, 8)
	withPreload(t, 2, true)

	// 2MB 的合法 JSON（远超 1MB 上限）：必须 400，而不是吃光内存
	huge := `{"paths":["` + strings.Repeat("a", 2<<20) + `"],"manual":false}`
	rec := httptest.NewRecorder()
	handlePregen(rec, httptest.NewRequest("POST", "/api/pregen", strings.NewReader(huge)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("超过 1MB 的请求体应回 400，实际 %d（二次审查 P1-4：body 无上限 = DoS）", rec.Code)
	}
}

// ---------- #5 /api/thumb 与 /api/meta 必须拒绝非普通文件 ----------

func Test2450ThumbAndMetaRejectNonRegular(t *testing.T) {
	withPreload(t, 0, true) // 确保 ThumbEnabled=true
	base := withPregenRoot(t)

	// 用**目录**冒充媒体文件：IsRegular()==false，与 FIFO 走同一条闸门
	// （Windows 上造不出 FIFO，但判据是同一条，足以拦住「非普通文件」这一类）。
	fake := filepath.Join(base, "evil_2450.jpg")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		url  string
		fn   http.HandlerFunc
	}{
		{"thumb", "/api/thumb?path=" + url.QueryEscape(fake), handleThumb},
		{"meta", "/api/meta?path=" + url.QueryEscape(fake), handleMeta},
	} {
		rec := httptest.NewRecorder()
		c.fn(rec, httptest.NewRequest("GET", c.url, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: 非普通文件应回 403，实际 %d（body=%q）—— "+
				"FIFO 会让 os.Open 永久阻塞、占死 goroutine 与解码槽（二次审查 P1-5）",
				c.name, rec.Code, strings.TrimSpace(rec.Body.String()))
			continue
		}
		if !strings.Contains(rec.Body.String(), "not a regular file") {
			t.Errorf("%s: 403 文案不是 not a regular file，实际 %q", c.name, rec.Body.String())
		}
	}
}

// ---------- #10 中间产物名带 cacheID 短哈希 ----------

func Test2450ThumbTmpSuffixDiffersByCacheID(t *testing.T) {
	a := thumbTmpSuffix("0123456789abcdef|q80")
	b := thumbTmpSuffix("fedcba9876543210|q95")
	if a == b {
		t.Errorf("不同 cacheID 得到同一份中间产物后缀（%q）—— 两路并发会互相删对方半成品", a)
	}
	if len(a) != 8 || len(b) != 8 {
		t.Errorf("短哈希长度应为 8，实际 %d/%d", len(a), len(b))
	}
}

// ---------- #11 coverStillValid 必须比较 Items[].Mtime ----------

func Test2450CoverStillValidDetectsItemMtimeChange(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.jpg")
	touch(t, img)
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &coverPick{
		V:      coverCacheVersion,
		Dir:    dir,
		PickAt: time.Now().UnixMilli(),
		Items:  []coverItem{{Path: img, Mtime: imgMtime(t, img), Kind: "image"}},
		Checks: []coverCheck{{Path: dir, Mtime: st.ModTime().UnixMilli()}},
	}
	if !coverStillValid(p, dir) {
		t.Fatal("内容未变时封面应有效")
	}
	// 原地覆盖这张图（文件名不变、目录项数不变 ⇒ 目录 mtime 也不变）
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(img, future, future); err != nil {
		t.Fatal(err)
	}
	if coverStillValid(p, dir) {
		t.Error("★★ 选中的图片被原地覆盖后封面仍判有效 —— 封面会永久陈旧，" +
			"只能手动重建（二次审查 P2-11 回归）")
	}

	// 兼容：老 meta 缺字段（Mtime==0）必须**跳过**比较，否则升级后全量重抽封面
	pOld := &coverPick{
		V:      coverCacheVersion,
		Dir:    dir,
		PickAt: time.Now().UnixMilli(),
		Items:  []coverItem{{Path: img, Mtime: 0, Kind: "image"}},
		Checks: []coverCheck{{Path: dir, Mtime: st.ModTime().UnixMilli()}},
	}
	if !coverStillValid(pOld, dir) {
		t.Error("老 meta（Items[].Mtime==0）被判失效 —— 升级后会全量重抽封面，是一次明显的 IO 峰值")
	}
}

// ---------- #19 parseSort 的下划线别名必须生效 ----------

func Test2450ParseSortUnderscoreAliases(t *testing.T) {
	cases := []struct {
		in    string
		field string
		desc  bool
	}{
		{"file_name", sortFieldName, false},
		{"modify_time", sortFieldMtime, true}, // 旧默认：修改时间倒序
		{"file_size", sortFieldSize, true},    // 旧默认：大小倒序
		{"create_time", sortFieldBtime, false},
		{"birth_time", sortFieldBtime, false},
		{"file_name_asc", sortFieldName, false},
		{"file_name_desc", sortFieldName, true},
		{"modify_time_asc", sortFieldMtime, false},
		{"name:desc", sortFieldName, true},
		{"file_name;bogus", sortFieldName, false}, // 无法识别 → 默认（name 升序）
	}
	for _, c := range cases {
		got := parseSort(c.in)
		if got.field != c.field || got.desc != c.desc {
			t.Errorf("parseSort(%q) = %s/%v，期望 %s/%v（二次审查 P3-19）",
				c.in, got.field, got.desc, c.field, c.desc)
		}
	}
}

// ---------- #13 空目录树也必须被 max 安全阀挡住 ----------

func Test2450CollectDirsBoundedOnEmptyTree(t *testing.T) {
	root := t.TempDir()
	const dirs = 120
	for i := 0; i < dirs; i++ {
		if err := os.MkdirAll(filepath.Join(root, "d"+itoa(i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 一个媒体文件都没有：原实现只在 hasMedia 时检查 max，这棵树会被完整走完
	got, truncated := collectDirsWithMedia([]string{root}, 20, make(chan struct{}))
	if len(got) != 0 {
		t.Fatalf("空目录树不该产出结果，实际 %d", len(got))
	}
	if !truncated {
		t.Error("★★ 120 个空目录、max=20 却没报 truncated —— 安全阀原来只约束" +
			"「有媒体的目录数」，病态目录树照样能撑爆内存（二次审查 P2-13）")
	}
}

// ---------- #3 清目录缩略图必须解析真实路径 ----------

func Test2450ClearThumbUsesResolveRealPath(t *testing.T) {
	src, err := os.ReadFile("thumb.go")
	if err != nil {
		t.Skipf("读不到 thumb.go：%v", err)
	}
	s := string(src)
	i := strings.Index(s, "func handleClearThumbCache(")
	if i < 0 {
		t.Fatal("找不到 handleClearThumbCache")
	}
	seg := s[i:]
	if j := strings.Index(seg, "\nfunc "); j > 0 {
		seg = seg[:j]
	}
	// 只做词法 underRoot 是不够的：缓存根内的一层符号链接就能让 root 进程
	// 去 chmod/RemoveAll /etc；必须解析真实路径并用它做 Walk/RemoveAll。
	if !strings.Contains(seg, "resolveRealPath(dir)") {
		t.Error("★ 清目录缩略图没有解析真实路径 —— 缓存根内的符号链接可让 root " +
			"越权删除系统目录（二次审查 P1-3 回归）")
	}
	if !strings.Contains(seg, "os.RemoveAll(realDir)") {
		t.Error("★ RemoveAll 用的不是解析后的真实路径 —— 仍会跟随中间层符号链接")
	}
}

// ---------- #16 在途子进程注册表必须能安全地空转 ----------

func Test2450KillLiveCommandsIsIdempotent(t *testing.T) {
	// 没有在途进程时必须立刻返回（退出流程里它会无条件下调用一次）
	start := time.Now()
	killLiveCommands(200 * time.Millisecond)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("没有在途子进程时 killLiveCommands 花了 %v，应当立即返回", el)
	}
	// 注册/注销要成对，且注销后表里不留垃圾
	cmd := exec.Command(os.DevNull)
	untrack := trackCmd(cmd)
	liveCmdMu.Lock()
	_, tracked := liveCmds[cmd]
	liveCmdMu.Unlock()
	if !tracked {
		t.Error("trackCmd 没有把命令登记进在途表 —— SIGTERM 时杀不掉孤儿 ffmpeg")
	}
	untrack()
	liveCmdMu.Lock()
	_, still := liveCmds[cmd]
	liveCmdMu.Unlock()
	if still {
		t.Error("注销后命令仍在在途表里（会无限增长）")
	}
}

// ---------- #6 SIGTERM 退出流程：顺序 + 幂等 ----------

func Test2450ExitRestoresSystemThumbnailer(t *testing.T) {
	// ① 未接管时必须是**完全的 no-op**（这条分支在任何环境都能跑）：
	//    退出流程无处不在地调它，多打一条 systemctl 都是噪音。
	old := getSettings().TakeoverSystemThumb
	settingsMu.Lock()
	settings.TakeoverSystemThumb = false
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.TakeoverSystemThumb = old
		settingsMu.Unlock()
	})

	start := time.Now()
	restoreSystemThumbnailerOnExit()
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("未接管时 restoreSystemThumbnailerOnExit 花了 %v，应当立即返回", el)
	}

	// ② 退出顺序（源码闸门）：先关接管 server + 删 socket，再恢复系统服务，
	//    最后才 Shutdown 主 server。顺序错了会出现"系统服务起来时旧 socket 还在"
	//    的窗口（文件管理器连到死地址）或"主 server 还在处理请求时系统服务已被恢复"。
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Skipf("读不到 main.go：%v", err)
	}
	m := string(src)
	iSrv := strings.Index(m, "stopSystemThumbServer(3 * time.Second)")
	iRestore := strings.Index(m, "restoreSystemThumbnailerOnExit()")
	iCtx := strings.Index(m, "shutdownCtx, shutdownCancel := context.WithTimeout(")
	if iSrv < 0 || iRestore < 0 {
		t.Fatal("main.go 找不到 stopSystemThumbServer / restoreSystemThumbnailerOnExit —— " +
			"退出流程被改坏（或这两个调用被删了）")
	}
	if !(iSrv < iRestore && iRestore < iCtx) {
		t.Error("★ 退出顺序不对：应为 ①停接管 server ②恢复 auto_thumbnailer ③Shutdown 主 server" +
			"（二次审查 P1-6）")
	}
	// 接管 server 的句柄必须是包级变量，否则退出时拿不到它
	if _, err := os.Stat("systemthumb.go"); err == nil {
		ss, _ := os.ReadFile("systemthumb.go")
		txt := string(ss)
		for _, want := range []string{"sysThumbSrv", "func stopSystemThumbServer("} {
			if !strings.Contains(txt, want) {
				t.Errorf("systemthumb.go 缺少 %q —— 接管 server 的句柄没提升为包级变量，"+
					"退出时无法 Shutdown（二次审查 P1-6）", want)
			}
		}
	}
}
