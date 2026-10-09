package main

// ---------------------------------------------------------------------------
// 1.8.54 合并点回归测试。
//
// 本版以我方 1.8.52 为基线，只吸收对方 1.8.51/1.8.53 的一处真实优点：
// 前端在关窗 / 页面卸载时用 sendBeacon 抢发 POST /thumb/resume。
// 同时把「我方独有、且直接对应用户诉求」的两条后端行为钉成断言，防止下次被无意回退：
//   A. 飞牛 /thumb/getIcon 入口必须打点（1.8.51/1.8.53 没有）；
//   B. 大图请求必须触发抢占式暂停（1.8.57 起由 /api/raw 承接，此前挂在 size=big）。
//
// 明确**未**吸收的两点，见文件末尾 NOTE。
// ---------------------------------------------------------------------------

import (
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// (1) 前端：关窗兜底必须存在，且必须在 hostCall('close') 之前调用。
func Test1854MergeUnloadFlushExists(t *testing.T) {
	js := fetchFrontend(t, "/app.js").Body.String()

	for _, need := range []string{"flushResumeThumbGen", "sendBeacon", "beforeunload", "pagehide"} {
		if !strings.Contains(js, need) {
			t.Errorf("app.js 缺少 %q —— 关窗兜底被回退了？", need)
		}
	}

	// 顺序断言：singleMode 分支里必须先 flushResumeThumbGen()，再 hostCall('close')。
	// hostCall 会立刻销毁窗口，写在它后面的恢复逻辑根本不会执行。
	idxHost := strings.Index(js, "single mode: ask host to close viewer window")
	if idxHost < 0 {
		t.Fatal("找不到 singleMode 的关窗分支")
	}
	start := idxHost - 500
	if start < 0 {
		start = 0
	}
	if !strings.Contains(js[start:idxHost], "flushResumeThumbGen();") {
		t.Error("flushResumeThumbGen() 没有出现在 hostCall('close') 之前 —— " +
			"宿主销毁窗口后它不会被执行，暂停状态会残留到 TTL 到期")
	}
}

// (2) 后端：飞牛链路必须仍然打点。
func Test1854MergeFnOsEntryTouchesActivity(t *testing.T) {
	setThumbActivityAge(time.Hour)
	if !thumbForegroundQuiet() {
		t.Fatal("前置条件不成立：应当已进入静默期")
	}
	before := atomic.LoadInt64(&thumbActivityUnixNano)
	w := httptest.NewRecorder()
	handleSystemThumbGetIcon(w, httptest.NewRequest("GET",
		"/thumb/getIcon?path=vol3/1000/photos/x.JPG&size=list", nil))
	after := atomic.LoadInt64(&thumbActivityUnixNano)
	if after == before {
		t.Errorf("飞牛 /thumb/getIcon（HTTP %d）未更新前台活动时间戳 —— "+
			"文件管理器整页缩略图请求会与后台预生成抢槽位与 CPU", w.Code)
	}
}

// (3) 后端：飞牛 size=big 应触发抢占式暂停（用户核心诉求）。
// 用户诉求「用飞牛文件管理打开大图浏览，后台缩略图生成全部停掉」有**两个真正生效的**
// 承接点：① socket 的 size=big（文件管理器大图预览；1.8.56 曾被误删，理由
// 「文件管理器从不请求 big」已证伪，见 zzz_1861_*）；② /api/raw 的转码路径
// （media.go serveScaledImage，mediaview 自己的查看器）。
// 本用例守三件事：① 网格/卡片（list/medium）不乱停后台；② big 必须停后台
// （行为细测见 TestAudit1850Fnos_SysThumbPausesOnlyOnBigPreview）；
// ③ /api/raw 的暂停触发点没有被删掉（行为验证见 Test1857RawTranscode...）。
func Test1854MergeFnOsPreviewTriggerKept(t *testing.T) {
	thumbPauseMu.Lock()
	oldPaused, oldUntil := thumbPaused, thumbPauseUntil
	thumbPaused, thumbPauseUntil = false, time.Time{}
	thumbPauseMu.Unlock()
	t.Cleanup(func() {
		thumbPauseMu.Lock()
		thumbPaused, thumbPauseUntil = oldPaused, oldUntil
		thumbPauseMu.Unlock()
	})

	pausedNow := func() bool {
		thumbPauseMu.Lock()
		defer thumbPauseMu.Unlock()
		return thumbPaused && time.Now().Before(thumbPauseUntil)
	}

	// ① 网格 list / 卡片 medium 不该整体停后台：打开一个目录会连发几十个请求，
	//    按这两种 size 停等于把用户自己手里的活全掐了。
	for _, size := range []string{"list", "medium"} {
		w := httptest.NewRecorder()
		handleSystemThumbGetIcon(w, httptest.NewRequest("GET",
			"/thumb/getIcon?path=vol3/1000/photos/x.JPG&size="+size, nil))
		if pausedNow() {
			t.Errorf("size=%s 触发了后台整体暂停 —— 网格/卡片视图不该停后台", size)
		}
	}

	// ② 大图预览（big）必须停后台。删掉它，用户就得「先跑完一页缩略图才出大图」。
	w := httptest.NewRecorder()
	handleSystemThumbGetIcon(w, httptest.NewRequest("GET",
		"/thumb/getIcon?path=vol3/1000/photos/x.JPG&size=big", nil))
	if !pausedNow() {
		t.Error("size=big 没有让后台停摆 —— 用户打开大图预览时后台预生成仍在抢 CPU，" +
			"表现为「必须先等一页缩略图」")
	}
	// 复位暂停态，别影响下面的断言
	thumbPauseMu.Lock()
	thumbPaused, thumbPauseUntil = false, time.Time{}
	thumbPauseMu.Unlock()

	// ③ 触发点仍在 /api/raw 的转码路径上，且必须排除预取（悬停预取连发会反复 kill 后台）
	b, err := os.ReadFile("media.go")
	if err != nil {
		t.Skipf("读不到 media.go（测试 CWD 不是源码目录）：%v", err)
	}
	s := string(b)
	if !strings.Contains(s, "noteForegroundPreview()") {
		t.Fatal("media.go 里没有 noteForegroundPreview() —— 大图请求不再停后台，" +
			"用户诉求「打开大图浏览时后台缩略图生成全部停掉」被回退")
	}
	if !strings.Contains(s, "if !isPrefetch {") {
		t.Error("media.go 的暂停触发没有排除预取请求 —— 悬停划过网格就会把后台预生成一直摁住")
	}
}

// (4) resume 必须立即生效：不引入后端延迟（否则与前端 1s 缓冲叠加成 ~2.5s 空窗）。
func Test1854MergeResumeIsImmediate(t *testing.T) {
	thumbPauseMu.Lock()
	oldPaused, oldUntil := thumbPaused, thumbPauseUntil
	thumbPaused, thumbPauseUntil = false, time.Time{}
	thumbPauseMu.Unlock()
	t.Cleanup(func() {
		thumbPauseMu.Lock()
		thumbPaused, thumbPauseUntil = oldPaused, oldUntil
		thumbPauseMu.Unlock()
	})

	w := httptest.NewRecorder()
	handlePauseThumb(w, httptest.NewRequest("POST", "/api/thumb/pause", nil))
	w = httptest.NewRecorder()
	handleResumeThumb(w, httptest.NewRequest("POST", "/api/thumb/resume", nil))

	thumbPauseMu.Lock()
	stillPaused := thumbPaused
	thumbPauseMu.Unlock()
	if stillPaused {
		t.Error("resume 之后 thumbPaused 仍为 true —— 后端凭空加了延迟，" +
			"会与前端 1s 缓冲叠加，用户关掉大图后要等更久缩略图才回来")
	}
}

// NOTE 未吸收对方两处改动及原因：
//
//  1) systemthumb 入口的 `if isThumbPaused() { 503 }`。
//     在对方架构里 isThumbPaused() 只由 mediaview 前端触发，语义是「前端看图 > 飞牛请求」。
//     本版暂停**由飞牛 size=big 自己触发**，加了这个闸门就会把同一批浏览请求
//     （list/medium）也 503 掉 → 文件管理器里其他缩略图变破图，自伤。
//
//  2) thumbPauseTTL 拉长 / 后端延迟 1.5s 恢复。
//     TTL 长是为了兜「收不到 resume」的异常路径，方向对但把最长停摆时间放大到 2 分钟；
//     本版改用「前端 1s 缓冲 + beforeunload/pagehide 抢发 resume」覆盖同一场景，
//     异常路径的兜底仍由 TTL 承担但不必再加长。后端延迟恢复则纯属叠加延迟，已用 (4) 钉死。
