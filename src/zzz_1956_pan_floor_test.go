package main

import (
	"strings"
	"testing"
)

// Test1956PanHasFloorWhenZoomed 放大后必须保证"拖得动"。
//
// 实测问题：竖图在放大两倍附近**恰好不溢出容器**，于是 applyTransform 算出的可拖范围为 0，
// 拖动完全没有响应 —— 用户报的就是"两倍左右竖图还是不能拖动"。
// 修法是给一个下限：只要真的放大过，至少允许拖动"自身被放大出来的那一段"的一半。
func Test1956PanHasFloorWhenZoomed(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function applyTransform(")
	if i < 0 {
		t.Fatal("找不到 applyTransform")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "if (v.scale > 1.01) {") {
		t.Error("applyTransform 里没有「放大后给可拖范围下限」的分支")
	}
	if !strings.Contains(rest, "if (maxTx <= 0) { maxTx = Math.max(8,") {
		t.Error("X 方向没有下限 —— 图没溢出时左右拖会纹丝不动")
	}
	if !strings.Contains(rest, "if (maxTy <= 0) { maxTy = Math.max(8,") {
		t.Error("Y 方向没有下限 —— 竖图在两倍附近上下拖会纹丝不动（用户实测到的）")
	}
}

// Test1956PanBranchInPointerMove 契约：pointermove 里"放大即平移"的分支必须还在。
func Test1956PanBranchInPointerMove(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "addEventListener('pointermove'")
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "if (v.scale > 1.01) {") {
		t.Error("pointermove 里没有「放大即平移」分支")
	}
	if !strings.Contains(rest, "v.tx = base.x + swipe.dx;") {
		t.Error("pointermove 没有按拖动位移更新 v.tx")
	}
}
