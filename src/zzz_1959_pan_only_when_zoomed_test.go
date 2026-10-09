package main

import (
	"strings"
	"testing"
)

// Test1959ZoomedDragNeverSwitches 放大后拖动**不得**触发切换。
//
// 用户 2026-10-07 定稿的交互表（按倍数分三档）：
//
//	≤1.01（未放大）：水平拖动 = 跟手切图（保留）
//	1.01~2 倍      ：任意方向拖动 = 平移图片；切图只能靠点击左右半区
//	≥2 倍          ：任意方向拖动 = 平移图片；点击也不切图
//
// 这条钉住「放大后一律不切图」：只要放大分支里出现任何把 horizontal 置真的写法，
// 放大后就又会左右滑动切图（1.8.158 的回归）。零死区那半边由 zzz_1958 负责。
func Test1959ZoomedDragNeverSwitches(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	i := strings.Index(js, "addEventListener('pointermove'")
	if i < 0 {
		t.Fatal("找不到 pointermove")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "swipe.horizontal = horiz") {
		t.Error("放大分支又按方向分流了 —— 放大后不该能滑动切图")
	}
	// 放大分支必须显式关闭 horizontal（否则 endSwipe 可能走进切图收尾）
	if !strings.Contains(rest, "swipe.horizontal = false;") {
		t.Error("放大分支没有显式 swipe.horizontal = false")
	}
	// 放大分支必须排在最前：先判放大，再判方向锁定
	iZoom := strings.Index(rest, "if (v.scale > 1.01) {")
	iLock := strings.Index(rest, "if (!swipe.locked) {")
	if iZoom < 0 {
		t.Fatal("pointermove 没有放大分支")
	}
	if iLock > 0 && iLock < iZoom {
		t.Error("未放大的方向判定排在放大分支之前 —— 放大会落进切图分支")
	}
}

// Test1959UnzoomedSwipeStillWorks 未放大时水平拖动仍必须能切图。
//
// 这条是 1.8.159 特意补的：用户明确要求「1× 保留拖动切图」。
// 放大与未放大共用 swipe.horizontal —— 很容易在「放大后禁止滑动切图」的改动里
// 顺手把未放大那条也砍掉，而**砍掉后没有任何别的测试会红**
// （1954/1958 都只看放大分支）。所以这里为它单独上一次保险。
func Test1959UnzoomedSwipeStillWorks(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	i := strings.Index(js, "addEventListener('pointermove'")
	if i < 0 {
		t.Fatal("找不到 pointermove")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "swipe.horizontal = Math.abs(swipe.dx) > Math.abs(swipe.dy);") {
		t.Error("未放大分支丢了「按方向决定是否滑动切图」—— 1× 时水平拖动将无法切图")
	}
	// 未放大时也要在 8px 之后才定方向（否则垂直拖动会被误判成切图）
	if !strings.Contains(rest, "Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8") {
		t.Error("未放大分支丢了 8px 方向判定")
	}
	// 轨道必须仍在 pointerdown 里准备（未放大才需要轨道）
	di := strings.Index(js, "addEventListener('pointerdown'")
	if di < 0 {
		t.Fatal("找不到 pointerdown")
	}
	down := js[di:]
	if j := strings.Index(down, "\n  });"); j > 0 {
		down = down[:j]
	}
	if !strings.Contains(down, "querySelector('.v-track')") {
		t.Error("pointerdown 不再准备轨道 —— 未放大的滑动切图会静默失效")
	}
}
