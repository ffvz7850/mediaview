package main

import (
	"strings"
	"testing"
)

// Test1954TrackRebuildAppliesTransform 换图后必须把 transform 也应用回去。
//
// Bug：1.8.152 在 syncTrackToIndex 里加了 resetView()，但它只改 v.scale/v.tx/v.ty 这些
// 状态变量；图片实际的缩放靠 CSS transform 表达，那一步在 applyTransform()。
// 只调 resetView() 的话状态归位了、DOM 上还留着上一张的 transform ——
// 于是新图看上去"自己放大了一些"。
func Test1954TrackRebuildAppliesTransform(t *testing.T) {
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
		t.Fatal("syncTrackToIndex 没有 resetView()")
	}
	if !strings.Contains(rest, "applyTransform()") {
		t.Error("syncTrackToIndex 调了 resetView() 却没有 applyTransform() —— " +
			"状态归位了但 DOM 上还留着旧 transform，新图会「自己放大」")
	}
	// 顺序也要求：applyTransform 必须在 resetView 之后
	if strings.Index(rest, "applyTransform()") < strings.Index(rest, "resetView()") {
		t.Error("applyTransform() 出现在 resetView() 之前 —— 应用的还是旧状态")
	}
}

// Test1954DragPansWithoutSwitching 放大后的拖动**只平移、不切图**；切图只认点击。
//
// 契约（2026-10-07 定稿）：放大后不允许滑动切图 —— 1.8.158 的「8px 判方向、水平就改走切图」
// 被用户否掉（反馈：放大到 2 倍内又能左右滑动切图了）。现在放大分支无条件平移。
// 配套的三件事必须同时在，缺一个就会退回：
//
//	① pointerdown 不得预设 panStart（那是"按下就定平移"，会让点击没法在 pointermove 前区分）；
//	② pointerdown 必须清 swipe.horizontal —— 否则上一轮残留的 true 会让 endSwipe 走进切图
//	   收尾分支并设 suppressClick，把紧随其后的 click 也吞掉 —— 放大后的"点击切图"就彻底没了；
//	③ pointerup 的 8px 距离守卫必须在（"拖动 ≠ 点击"的唯一依据）。
func Test1954DragPansWithoutSwitching(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	if strings.Contains(js, "swipe.panStart = { x: e.clientX - v.tx") {
		t.Error("pointerdown 仍在预设 panStart")
	}
	if !strings.Contains(js, "swipe.panBase") {
		t.Error("pointerdown 没有记下平移基准 panBase")
	}

	i := strings.Index(js, "addEventListener('pointerdown'")
	if i < 0 {
		t.Fatal("找不到 pointerdown")
	}
	down := js[i:]
	if j := strings.Index(down, "\n  });"); j > 0 {
		down = down[:j]
	}
	if !strings.Contains(down, "swipe.horizontal = false;") {
		t.Error("pointerdown 没有清 swipe.horizontal —— 残留值会让放大后的点击切图被 endSwipe 吞掉")
	}

	i = strings.Index(js, "addEventListener('pointermove'")
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "swipe.horizontal = horiz && v.scale < SWIPE_LOCK_SCALE;") {
		t.Error("pointermove 又有「两倍以内水平拖动才切图」了 —— 放大后不该能滑动切图")
	}
	if !strings.Contains(rest, "swipe.horizontal = false;") {
		t.Error("pointermove 的放大分支没有显式关闭 horizontal")
	}

	if !strings.Contains(js, "var isTap = Math.abs(swipe.dx) <= 8 && Math.abs(swipe.dy) <= 8;") {
		t.Error("pointerup 的点击切图没有 8px 距离守卫 —— 拖着看图松手会把图切走")
	}
	if !strings.Contains(js, "if (isTap && v.scale < SWIPE_LOCK_SCALE) {") {
		t.Error("点击切图的判据没有同时包含「几乎没动」与「两倍以内」")
	}
}
