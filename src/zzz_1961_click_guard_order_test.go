package main

import (
	"strings"
	"testing"
)

// Test1961TapGuardBeforeZoomReset vStage 的 click 处理器里，**距离守卫必须在"≥2 倍归位"之前**。
//
// 1.8.160 引入的 bug（用户实测"图片放大后一拖动就回归窗内大小"）：
// 那条 ≥2 倍的判断原来是 `return`（无副作用），放在距离守卫之前无害；
// 1.8.160 为了统一"点图与键盘行为"把它换成了 `resetView()`，副作用就来了：
//
//	放大到 2 倍以上 → 拖动图片 → 松手时浏览器仍会发一个 click
//	→ 命中 ≥2 倍分支 → resetView() → 缩放被打回"适应窗口"。
//
// 所以顺序是硬约束：**先问"这是不是点击"，再问"要不要归位"**。
// 这类"顺序敏感"的缺陷最容易在后续重构里回来，所以单独钉一条。
func Test1961TapGuardBeforeZoomReset(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "$('vStage').addEventListener('click'")
	if i < 0 {
		t.Fatal("找不到 vStage 的 click 监听")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  });"); j > 0 {
		seg = seg[:j]
	}

	// ① suppressClick 的消费必须仍在最前（否则会切两次）
	iSup := strings.Index(seg, "if (swipe.suppressClick)")
	if iSup < 0 {
		t.Fatal("click 里没有 suppressClick 的消费")
	}

	// ② 距离守卫
	iTap := strings.Index(seg, "if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) return;")
	if iTap < 0 {
		t.Fatal("click 里没有「位移 > 8px 不触发点击」的距离守卫")
	}

	// ③ ≥2 倍归位
	iReset := strings.Index(seg, "if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }")
	if iReset < 0 {
		t.Fatal("click 里没有 ≥2 倍的「先归位」")
	}

	if !(iSup < iTap && iTap < iReset) {
		t.Errorf("click 处理器的判据顺序不对（suppressClick=%d, 距离守卫=%d, ≥2倍归位=%d）："+
			"距离守卫**必须**在 ≥2 倍归位之前，否则「放大后拖动」松手触发的 click "+
			"会把缩放重置回适应窗口", iSup, iTap, iReset)
	}
}

// Test1961DragDoesNotResetScale 放大后拖动只应改 tx/ty，绝不改 scale。
//
// 这是上一条的行为侧对偶：即使顺序对了，也保证 pointermove 的放大分支里
// 没有任何写 v.scale 的语句。
func Test1961DragDoesNotResetScale(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "addEventListener('pointermove'")
	if i < 0 {
		t.Fatal("找不到 pointermove")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  });"); j > 0 {
		seg = seg[:j]
	}
	iZoom := strings.Index(seg, "if (v.scale > 1.01) {")
	iTap := strings.Index(seg, "if (!swipe.locked) {")
	if iZoom < 0 {
		t.Fatal("pointermove 没有放大分支")
	}
	branch := seg[iZoom:]
	if iTap > iZoom {
		branch = seg[iZoom:iTap]
	}
	if strings.Contains(branch, "v.scale =") || strings.Contains(branch, "resetView(") {
		t.Error("pointermove 的放大分支里写了 v.scale / 调了 resetView —— 拖动会改变缩放")
	}
	if !strings.Contains(branch, "v.tx = base.x + swipe.dx;") {
		t.Error("放大分支没有按位移更新 v.tx")
	}
}
