package main

import (
	"strings"
	"testing"
)

// Test1952SwipeNotBlockedByZoomFlag 所有"阻止切图"的判据都必须用放大倍数，不能用 v.zoomed。
//
// Bug：v.zoomed 只要 scale > 1.01 就为真（applyActualScale / wheel 都这么算），
// 于是**点过一次 1:1 之后这张图就再也切不动了** —— 用户报的就是这个。
// 要求：放大不到 1.5 倍时应当允许切换。
func Test1952SwipeNotBlockedByZoomFlag(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if !strings.Contains(js, "var SWIPE_LOCK_SCALE = 2;") {
		t.Error("找不到 SWIPE_LOCK_SCALE = 2")
	}
	// 1.8.160：这里的守卫分成两种形态，要分别查 —— 不能只数字面量总数。
	//   · `... ) return;`             —— endSwipe 的提前返回（滑动切图的兜底）
	//   · `... ) { resetView(); ... }` —— "先归位"形态：swipeTo（键盘/❮❯）与 vStage 的 click
	// 后一种必须**恰好 2 处**：这两个入口必须表现一致，否则会出现"点图没反应、按键盘却归位"。
	nRet := strings.Count(js, "if (v.scale >= SWIPE_LOCK_SCALE) return;")
	if nRet < 1 {
		t.Errorf("endSwipe 少了「到阈值以上不切」的提前返回（只有 %d 处）", nRet)
	}
	nReset := strings.Count(js, "if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }")
	if nReset != 2 {
		t.Errorf("「先归位」形态有 %d 处，期望恰好 2 处（swipeTo 键盘/❮❯ + vStage 的 click）"+
			"—— 两个入口必须表现一致，否则点图与键盘行为不同", nReset)
	}
	// 点击切图那处原来是 `if (!v.zoomed)`
	if !strings.Contains(js, "if (isTap && v.scale < SWIPE_LOCK_SCALE) {") {
		t.Error("pointerup 的「点击切图」判据不对 —— 应同时要求「几乎没动(isTap)」与「2 倍以内」")
	}
	// swipeTo 开头也不能再用 v.zoomed
	i := strings.Index(js, "function swipeTo(")
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "if (v.zoomed)") {
		t.Error("swipeTo 仍用 v.zoomed 判断是否先归位")
	}
	if !strings.Contains(rest, "if (v.scale >= SWIPE_LOCK_SCALE)") {
		t.Error("swipeTo 没有按放大倍数决定是否先归位")
	}
}

// Test1952TrackRebuildResetsView 换图必须完整重置视图状态。
//
// Bug：syncTrackToIndex 原来只写 `v.actual = false`，
// v.scale / v.zoomed / v.tx / v.ty 会留到下一张 —— 于是新图"自己放大了一些"，
// 且 v.zoomed 为真导致切不动。
func Test1952TrackRebuildResetsView(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function syncTrackToIndex(")
	if i < 0 {
		t.Fatal("找不到 syncTrackToIndex")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "resetView()") {
		t.Error("syncTrackToIndex 换图时没有完整重置视图（resetView）—— " +
			"上一张的缩放会带到下一张，表现为「它自己放大了一些」且切不动图")
	}
	if strings.Contains(rest, "v.actual = false;") && !strings.Contains(rest, "resetView()") {
		t.Error("syncTrackToIndex 仍在只手写 v.actual = false")
	}
}
