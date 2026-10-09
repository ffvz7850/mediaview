package main

import (
	"strings"
	"testing"
)

// TestSwipeDoesNotWaitForDecode 守住「切换时位移立即发生，不等 decode」。
//
// 背景：1.8.148 及以前，swipeTo 里是 `targetImg.decode().then(doIt, doIt)` ——
// 位移被推迟到解码完成（上限 1500ms），这段时间画面纹丝不动，
// 用户感受就是"点了没反应 / 不跟手"。改成位移立即 + 目标盒子的转圈表达加载中。
func TestSwipeDoesNotWaitForDecode(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function swipeTo(")
	if i < 0 {
		t.Fatal("找不到 swipeTo")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "decode().then(doIt") {
		t.Error("swipeTo 又在等 decode 才位移 —— 那 1.5 秒画面不动，就是不跟手的来源")
	}
	if strings.Contains(rest, "setTimeout(doIt, 1500)") {
		t.Error("swipeTo 里还有 doIt 的 1500ms 兜底定时器 —— 位移应该是立即的")
	}
	if !strings.Contains(rest, "doIt();") {
		t.Error("swipeTo 没有立即调用 doIt()（位移）")
	}
}

// TestEveryBoxHasSpin 守住「每个媒体盒都带转圈」。
//
// 背景：转圈原来只在 `isCurrent` 时创建，而切换目标图是 `isCurrent:false`，
// 于是位移过去之后那段时间画面是空的、没有任何指示。
func TestEveryBoxHasSpin(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function createMediaBox(")
	if i < 0 {
		t.Fatal("找不到 createMediaBox")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "if (isCurrent) {\n        spin = document.createElement") {
		t.Error("转圈仍只在 isCurrent 时创建 —— 切换目标（isCurrent:false）会没有指示")
	}
	if !strings.Contains(rest, "if (true) {\n        spin = document.createElement") {
		t.Error("createMediaBox 里没有为每个盒子创建转圈")
	}
}

// TestNoDuplicatePrefetch 守住「不再发纯重复的预取请求」。
func TestNoDuplicatePrefetch(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	for _, dead := range []string{"function preloadNextImage", "function preloadPrevImage", "function preloadAhead", "var preloadCache"} {
		if strings.Contains(js, dead) {
			t.Errorf("又出现了 %q —— 它的 URL 与轨道里盒子的 src 完全相同，"+
				"只白占转码槽并多解码一份大图", dead)
		}
	}
	if !strings.Contains(js, "function warmTrackBoxes") {
		t.Error("没有 warmTrackBoxes —— 应该改为只对轨道里已有的盒子提前 decode")
	}
}

// TestEnsureMediaVisibleUnconditional 守住「ensureMediaVisible 不被 if 误吞」。
func TestEnsureMediaVisibleUnconditional(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if strings.Contains(js, "if (cfg.viewerPreload > 1)") &&
		strings.Contains(js, "if (cfg.viewerPreload > 1)     ensureMediaVisible") {
		t.Error("ensureMediaVisible 又被 viewerPreload 的条件吞掉了")
	}
	if !strings.Contains(js, "ensureMediaVisible();") {
		t.Error("找不到 ensureMediaVisible() 的调用")
	}
}
