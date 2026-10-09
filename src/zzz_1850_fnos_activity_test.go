package main

// ---------------------------------------------------------------------------
// 1.8.50 复核：飞牛文件管理器链路（unix socket /thumb/getIcon）是否接入了
// 「前台活动感知 / 后台让路」机制。
//
// 静态事实（可 grep 复核）：
//   mediaview 自己的 handler 全部在第一行调用 touchThumbActivity()：
//     handleList    media.go:59
//     handleScan    media.go:101
//     handleRaw     media.go:143
//     handleMeta    media.go:512
//     handleVolumes config.go:1045
//     handleThumb   thumb.go:1045
//   而飞牛的 handleSystemThumbGetIcon（systemthumb.go:94）没有。
//
// 本文件用可执行断言把这个差异钉死，并顺带确认静默期语义与缓存档位划分。
// ---------------------------------------------------------------------------

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func setThumbActivityAge(age time.Duration) {
	atomic.StoreInt64(&thumbActivityUnixNano, time.Now().Add(-age).UnixNano())
}

// 对照组：mediaview 自己的 handler 确实会 touch（证明本测试方法有区分能力）。
func TestAudit1850Fnos_OwnHandlersTouchActivity(t *testing.T) {
	cases := []struct {
		name string
		url  string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"handleList", "/api/list?path=/no/such/dir", handleList},
		{"handleThumb", "/api/thumb?path=/no/such.jpg", handleThumb},
		{"handleRaw", "/api/raw?path=/no/such.jpg", handleRaw},
		{"handleMeta", "/api/meta?path=/no/such.jpg", handleMeta},
		{"handleVolumes", "/api/volumes", handleVolumes},
	}
	for _, c := range cases {
		setThumbActivityAge(time.Hour)
		before := atomic.LoadInt64(&thumbActivityUnixNano)
		w := httptest.NewRecorder()
		c.fn(w, httptest.NewRequest("GET", c.url, nil))
		after := atomic.LoadInt64(&thumbActivityUnixNano)
		if after == before {
			t.Errorf("%s 未更新前台活动时间戳（HTTP %d）—— 与代码事实不符", c.name, w.Code)
		} else {
			t.Logf("OK  %-14s HTTP %-3d  活动时间戳已更新", c.name, w.Code)
		}
	}
}

// 实验组（当前应为 RED）：飞牛链路应当也被当作「前台活动」。
func TestAudit1850Fnos_GetIconShouldTouchActivity(t *testing.T) {
	setThumbActivityAge(time.Hour) // 先确保处于静默期（后台此刻可以抢槽）
	if !thumbForegroundQuiet() {
		t.Fatal("前置条件不成立：应当已进入静默期")
	}

	before := atomic.LoadInt64(&thumbActivityUnixNano)
	lastCode := 0
	for i := 0; i < 20; i++ { // 模拟文件管理器渲染整页网格
		w := httptest.NewRecorder()
		handleSystemThumbGetIcon(w, httptest.NewRequest("GET",
			"/thumb/getIcon?path=vol3/1000/photos/DSC0001.JPG&size=list", nil))
		lastCode = w.Code
	}
	after := atomic.LoadInt64(&thumbActivityUnixNano)

	if after != before {
		t.Logf("飞牛链路已让后台让路（活动时间戳已更新）")
		return
	}
	t.Errorf("飞牛 /thumb/getIcon 连续 20 次请求（HTTP %d）都没有更新前台活动时间戳，"+
		"thumbForegroundQuiet() 仍返回 true → 后台预生成会在文件管理器渲染整页缩略图的"+
		"**同时**照常抢槽位与 CPU。\n期望：handleSystemThumbGetIcon 入口处调用 "+
		"touchThumbActivity()（与 /api/thumb、/api/list 一致）。", lastCode)
}

// 静默期语义：这就是「N 秒后恢复」的实现方式 —— 它数的是「距最后一次前台请求」，不是「界面是否关闭」。
func TestAudit1850Fnos_QuietPeriodAutoResume(t *testing.T) {
	old := thumbQuietPeriod
	thumbQuietPeriod = 150 * time.Millisecond
	defer func() { thumbQuietPeriod = old }()

	touchThumbActivity()
	if thumbForegroundQuiet() {
		t.Fatal("刚 touch 过就判定为静默，语义不符")
	}
	time.Sleep(thumbQuietPeriod + 80*time.Millisecond)
	if !thumbForegroundQuiet() {
		t.Fatal("超过静默期仍未判定为静默")
	}
	t.Logf("静默期语义确认：touch 后 %v 内后台不抢槽；超过后自动恢复。"+
		"→ 用户停留在看图界面、不再发请求时，后台会在「最后一次缩略图请求 + 静默期」"+
		"自行复活，而不是等界面被关闭。", thumbQuietPeriod)
}

// 用户诉求的第二半：「打开大图浏览时后台**全部停掉**」。
// 触发点有两条（1.8.61 修正过一次错误结论）：
//   - 飞牛文件管理器打开大图预览 → /thumb/getIcon?size=big
//     （飞牛前端 ImagePlayer-*.js 里就是 `{size:B.Big,path:e[t]}`）；
//   - mediaview 自己打开查看器 → /api/raw 要转码时（见 Test1857Raw...）。
//
// 本用例守三件事：① 网格/卡片视图（list/medium）不停后台；
// ② 大图预览（big）必须停后台；③ 前台活动打点仍在。
func TestAudit1850Fnos_SysThumbPausesOnlyOnBigPreview(t *testing.T) {
	thumbPauseMu.Lock()
	oldPaused, oldUntil := thumbPaused, thumbPauseUntil
	thumbPaused, thumbPauseUntil = false, time.Time{}
	thumbPauseMu.Unlock()
	oldQuiet := thumbQuietPeriod
	thumbQuietPeriod = 1500 * time.Millisecond
	t.Cleanup(func() {
		thumbPauseMu.Lock()
		thumbPaused, thumbPauseUntil = oldPaused, oldUntil
		thumbPauseMu.Unlock()
		thumbQuietPeriod = oldQuiet
	})

	get := func(size string) {
		w := httptest.NewRecorder()
		handleSystemThumbGetIcon(w, httptest.NewRequest("GET",
			"/thumb/getIcon?path=vol3/1000/photos/DSC0001.JPG&size="+size, nil))
	}
	isPaused := func() (bool, time.Time) {
		thumbPauseMu.Lock()
		defer thumbPauseMu.Unlock()
		return thumbPaused && time.Now().Before(thumbPauseUntil), thumbPauseUntil
	}

	// ① 网格 / 卡片视图不该整体停后台：打开一个目录会连发几十个 list 请求，
	//    按 size=list 停后台等于把用户自己手里的活全掐了。
	for _, size := range []string{"list", "medium"} {
		get(size)
		if on, _ := isPaused(); on {
			t.Errorf("size=%s 触发了后台整体暂停 —— 网格/卡片视图不该停后台"+
				"（真实触发点是 big，见下面 ②）", size)
		}
	}

	// ② 大图预览（size=big）= 用户此刻正在等这一张，必须让后台整体停摆。
	//    1.8.56 曾把这个钩子删掉，理由是「文件管理器从不请求 size=big」——该结论已证伪：
	//    飞牛 ImagePlayer-*.js 就是 `{size:B.Big,path:e[t]}`，index-*.js 是
	//    `gX({size:dX.Big,path:e})`。删掉它 = 打开大图时后台预生成照旧抢 CPU 与信号量槽，
	//    用户看到的就是「必须先跑完一页缩略图，大图才出来」。
	get("big")
	if on, until := isPaused(); !on {
		t.Error("size=big（文件管理器大图预览）没有让后台停摆 —— " +
			"用户打开大图时后台预生成仍在抢 CPU，表现为「要先跑完一页缩略图」")
	} else if d := time.Until(until); d < thumbPreviewPauseTTL-time.Second {
		t.Errorf("big 触发的暂停只剩 %v，期望约 %v（滑动续期）", d, thumbPreviewPauseTTL)
	} else {
		t.Logf("size=big → 后台暂停（滑动 %v）：打开大图预览时后台预生成停摆", thumbPreviewPauseTTL)
	}
	// 复位，别让暂停态影响后续断言
	thumbPauseMu.Lock()
	thumbPaused, thumbPauseUntil = false, time.Time{}
	thumbPauseMu.Unlock()

	// ③ P0 打点必须在：这条 socket 请求 = 文件管理器正在前台渲染
	atomic.StoreInt64(&thumbActivityUnixNano, 0)
	get("list")
	if thumbForegroundQuiet() {
		t.Error("handleSystemThumbGetIcon 没有打点（touchThumbActivity）—— " +
			"文件管理器整页加载时后台预生成不会让路，仍会抢 CPU 与信号量槽")
	}

	// ④ 真正会停后台的是「用户打开查看器」：前端 /api/thumb/pause
	w := httptest.NewRecorder()
	handlePauseThumb(w, httptest.NewRequest("POST", "/api/thumb/pause", nil))
	t0 := time.Now()
	on, until := isPaused()
	if !on {
		t.Fatal("查看器发来的 /api/thumb/pause 没有让后台停掉")
	}
	if d := until.Sub(t0); d < thumbPauseTTL-time.Second || d > thumbPauseTTL+time.Second {
		t.Errorf("暂停到期时刻距现在 %v，期望约 %v（前端异常关窗时靠它自动恢复）", d, thumbPauseTTL)
	} else {
		t.Logf("查看器打开 → 后台进入暂停，%v 后自动恢复", d.Round(time.Second))
	}

	// ⑤ 功能验证：暂停期间后台 worker 确实被挡住（不是只改了标志位）
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	started := time.Now()
	if waitWhilePaused(ctx) {
		t.Errorf("暂停期间 waitWhilePaused 直接返回 true —— 标志位没有真正挡住 worker")
	} else if el := time.Since(started); el < 300*time.Millisecond {
		t.Errorf("暂停期间 waitWhilePaused 只阻塞了 %v，过短", el)
	} else {
		t.Logf("暂停期间后台 worker 确实被挡住（阻塞 %v 后随 ctx 超时退出）", el.Round(time.Millisecond))
	}

	// 收尾：恢复，别把暂停态留给后面的用例
	w2 := httptest.NewRecorder()
	handleResumeThumb(w2, httptest.NewRequest("POST", "/api/thumb/resume", nil))
}

// 飞牛 list/medium/big 三档各自独立缓存，且与 mediaview 网格档位不重合。
func TestAudit1850Fnos_SizeTiersIndependentCache(t *testing.T) {
	list, medium, big := sizeToPixels("list"), sizeToPixels("medium"), sizeToPixels("big")
	if list != 320 || medium != 800 || big != 1920 {
		t.Fatalf("飞牛 size 映射异常：list=%d medium=%d big=%d", list, medium, big)
	}
	const p = "/vol3/1000/photos/DSC0001.JPG"
	a, b, c := thumbPathFor(p, list), thumbPathFor(p, medium), thumbPathFor(p, big)
	if a == b || b == c || a == c {
		t.Errorf("不同 size 落到同一缓存路径：%s / %s / %s", a, b, c)
	}
	t.Logf("三档各自独立缓存（singleflight key 亦含 size，不会互相复用）：\n"+
		"  list(%d)   %s\n  medium(%d) %s\n  big(%d)   %s", list, a, medium, b, big, c)
	t.Logf("mediaview 前端网格用 thumbRequestSize()=%d，只与 list 档可能重合；medium/big 必须各自再解码一次原图",
		thumbRequestSize())
}
