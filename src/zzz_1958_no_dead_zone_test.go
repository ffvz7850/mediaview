package main

import (
	"strings"
	"testing"
)

// Test1958NoDeadZoneBeforePan 放大状态下拖动不得有起步死区。
//
// 症状：1.8.157 的 pointermove 在「位移不足 8px」时直接 return，
// 那 8px 里画面纹丝不动 —— 用户实测「两倍以内拖动没以上的顺滑」。
//
// 修法（1.8.159 收敛版）：放大状态下**第 1 个像素就开始平移**，且**不再按方向分流**。
// 1.8.158 曾用「8px 只判方向、判定为水平就回滚改走滑动切图」来消死区 ——
// 死区确实没了，但把「放大后水平拖能切图」接回来了（用户不要）。现在方向分流整个删掉：
// 放大 = 只平移，比「判方向 + 回滚」更简单、也更没有条件分支可以出错。
func Test1958NoDeadZoneBeforePan(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "addEventListener('pointermove'")
	if i < 0 {
		t.Fatal("找不到 pointermove")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}

	// ① 放大分支里、平移赋值之前**不得有任何条件语句**（这是死区的根因形态）
	//
	// ⚠️ 1.8.158 这里原本锚的是注释文本 `return; // 还没超过 8px`，而这个
	// rest 取自 stripLineComments 之后 —— 注释已被剥掉，该断言**永远不会触发**（假绿）。
	// 现在改成从代码结构上判：进放大分支后必须先无条件平移，中间不许出现 if / return。
	iZoom := strings.Index(rest, "if (v.scale > 1.01) {")
	if iZoom < 0 {
		t.Fatal("pointermove 没有放大分支")
	}
	iTx := strings.Index(rest, "v.tx = base.x + swipe.dx;")
	if iTx < 0 {
		t.Fatal("找不到 v.tx = base.x + swipe.dx;")
	}
	if iTx < iZoom {
		t.Fatal("平移赋值出现在放大分支之前 —— 结构不对")
	}
	// 从分支**内部**开始（跳过 `if (v.scale > 1.01) {` 这个开头本身）
	seg := rest[iZoom+len("if (v.scale > 1.01) {") : iTx]
	if strings.Contains(seg, "if (") {
		t.Error("放大分支里、平移之前出现条件语句 —— 起步会先等等看，就是死区回归")
	}
	if strings.Contains(seg, "return;") {
		t.Error("放大分支里、平移之前出现 return —— 位移不足时会直接退出，就是死区回归")
	}

	// ② 放大分支不得再有「方向分流 + 回滚」（1.8.158 那套已被用户否掉）
	if strings.Contains(rest, "swipe.horizontal = horiz") {
		t.Error("放大分支又出现按方向分流（swipe.horizontal = horiz）—— 会让放大后也能滑动切图")
	}
	if strings.Contains(rest, "v.tx = base.x;") {
		t.Error("放大分支又出现回滚平移量（v.tx = base.x）—— 那是「判定为滑动切图」的配套，本版不该有")
	}

	// ③ 随之失效的「轨道补挂」必须一起删掉，否则是留着的死代码
	if strings.Contains(rest, "querySelector('.v-track')") {
		t.Error("pointermove 里还留着补挂轨道的死代码 —— 放大已不再走滑动切图，它永远不会被用到")
	}

	// ④ pointerup 的点击守卫必须还在（「拖动≠点击」的唯一依据，不能因为这次改动被删）
	if !strings.Contains(js, "var isTap = Math.abs(swipe.dx) <= 8 && Math.abs(swipe.dy) <= 8;") {
		t.Error("pointerup 的 8px 点击守卫丢了")
	}
}
