package main

import (
	"strings"
	"testing"
)

// ---------- 1.8.160：对 1.8.159 全量审查所发现问题的回归守卫 ----------

// Test1960ManualWorkerNotOnce 「手动生成」的临时 worker 不得再用可重置的 sync.Once。
//
// 原实现在收尾 goroutine 里写 `manualWorkersOnce = sync.Once{}` 来"允许下次再拉起"：
//
//	· sync.Once 不是设计来重置的；
//	· 那个赋值在另一个 goroutine，与下次 Do() 并发 → data race。
//
// race 的后果不是崩溃，而是可能丢掉一次拉起：任务进了队列却没人消费，
// 表现为"点了立即生成但一直没动静"，且难以复现。
func Test1960ManualWorkerNotOnce(t *testing.T) {
	th := readSourceOrSkip(t, "thumb.go")
	code := stripLineComments(th)
	if strings.Contains(code, "manualWorkersOnce") {
		t.Error("thumb.go 又用回了 manualWorkersOnce（sync.Once）—— 在 goroutine 里重置它是 data race")
	}
	if !strings.Contains(code, "manualWorkerActive") || !strings.Contains(code, "manualWorkerMu") {
		t.Error("没有看到「互斥量 + 显式标志」的替代实现")
	}
	// 互斥量必须真的在使用（不能只声明）。
	//
	// 注意：**不能要求 Lock 与 Unlock 数量相等** —— 一个 Lock 允许在两个分支里各 Unlock 一次
	// （`if active { Unlock; return }` + 正常路径 Unlock），这恰恰是常见的正确写法。
	// 这里只保证：两处临界区（拉起点 + 收尾点）都真的加了锁。
	lock := strings.Count(code, "manualWorkerMu.Lock()")
	unlock := strings.Count(code, "manualWorkerMu.Unlock()")
	if lock < 2 || unlock < 2 {
		t.Errorf("manualWorkerMu 使用不足（Lock %d / Unlock %d）—— 起止两处临界区都应加锁", lock, unlock)
	}
	if !strings.Contains(code, "manualWorkerMu.Unlock()\n\t\treturn") &&
		!strings.Contains(code, "manualWorkerMu.Unlock()\n\t\t\treturn") {
		t.Log("提示：未检测到「已激活则提前返回」的分支，若实现有意如此可忽略")
	}
}

// Test1960SuppressClickIsReset 每次手势开始必须清 suppressClick。
//
// 它只在 vStage 的 click 里被清，而 click **只在"按下与松开在同一元素"时才触发**：
// 按在图上、拖到 vStage 外面再松手就没有 click → 标记残留 true →
// 下一次真点击会在第一行被 return 掉，表现为偶发"点一下没反应，再点一下才行"。
func Test1960SuppressClickIsReset(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "addEventListener('pointerdown'")
	if i < 0 {
		t.Fatal("找不到 pointerdown")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  });"); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "swipe.suppressClick = false;") {
		t.Error("pointerdown 没有重置 suppressClick —— 拖出舞台后松手会让下一次点击被吞")
	}
	// 消费侧必须还在（否则会切两次）
	if !strings.Contains(js, "if (swipe.suppressClick) { swipe.suppressClick = false; return; }") {
		t.Error("vStage click 里的 suppressClick 消费没了 —— 点击会切两次")
	}
}

// Test1960ClickAndKeyboardAgree ≥2 倍时「点图」与「键盘/❮❯」必须表现一致（都是先归位）。
//
// 统一前：点图什么都不做，键盘却会归位 —— 同一个动作两个入口不同，
// 而"点了一下完全没反应"最容易让人以为功能坏了。
func Test1960ClickAndKeyboardAgree(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	const want = "if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }"
	if n := strings.Count(js, want); n != 2 {
		t.Errorf("「先归位」形态有 %d 处，期望恰好 2 处（vStage 的 click + swipeTo）—— "+
			"两个切图入口必须表现一致", n)
	}
	// swipeTo 里必须是它
	i := strings.Index(js, "function swipeTo(")
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, want) {
		t.Error("swipeTo（键盘 ←/→ 与界面 ❮/❯）没有「先归位」")
	}
	// vStage click 里也必须是它。
	// ⚠ 必须锚 "$('vStage')"，否则会匹配到页面上其它 'click' 监听（我第一版就错在这里）。
	k := strings.Index(js, "$('vStage').addEventListener('click'")
	if k < 0 {
		t.Fatal("找不到 vStage 的 click 监听")
	}
	clk := js[k:]
	if l := strings.Index(clk, "\n  });"); l > 0 {
		clk = clk[:l]
	}
	if !strings.Contains(clk, want) {
		t.Error("vStage 的 click 没有「先归位」—— 与键盘行为不一致")
	}
}

// Test1960CloseViewerResetsView 关闭查看器必须重置视图状态。
//
// closeViewer 里是 `vWrap.innerHTML = ”` —— 轨道连同 <img> 一起销毁，
// 但 v.scale / v.zoomed / v.tx / v.ty 挂在 v 上、不会被它清掉。
// 于是"放大到 8 倍 → 关闭 → 重新打开一张图"时，新图会直接按 8 倍显示。
func Test1960CloseViewerResetsView(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function closeViewer(")
	if i < 0 {
		t.Fatal("找不到 closeViewer")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "vWrap.innerHTML = ''") {
		t.Fatal("closeViewer 不再销毁轨道 —— 前提变了，请重新评估本测试")
	}
	if !strings.Contains(rest, "resetView()") {
		t.Error("closeViewer 销毁了轨道却没有 resetView() —— " +
			"放大后关闭再打开，新图会按旧 scale 显示（「自己放大」）")
	}
	// rebuildTrackInPlace 里的重置也不能被当成冗余删掉（它是"重开"路径的另一处保底）
	i2 := strings.Index(js, "function rebuildTrackInPlace(")
	if i2 < 0 {
		t.Fatal("找不到 rebuildTrackInPlace")
	}
	r2 := js[i2:]
	if j := strings.Index(r2, "\n  function "); j > 0 {
		r2 = r2[:j]
	}
	if !strings.Contains(r2, "resetView()") || !strings.Contains(r2, "applyTransform()") {
		t.Error("rebuildTrackInPlace 少了 resetView()+applyTransform() —— " +
			"这条路径只在「关闭后重开」时走到，删掉会让新图按旧 scale 显示")
	}
}

// Test1960DeadCodeStaysDeleted 已删除的死代码不得回来。
func Test1960DeadCodeStaysDeleted(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if strings.Contains(js, "panStart") {
		t.Error("swipe.panStart 又回来了 —— 它已无任何写入方（1.8.154 起改用 panBase）")
	}
	if strings.Contains(js, "function fmtSize(") {
		t.Error("死函数 fmtSize 又回来了")
	}
	for _, f := range []string{"ffmpeg.go"} {
		code := stripLineComments(readSourceOrSkip(t, f))
		if strings.Contains(code, "func firstExisting(") {
			t.Errorf("%s 里死函数 firstExisting 又回来了", f)
		}
		if strings.Contains(code, "func floatStr(") {
			t.Errorf("%s 里死函数 floatStr 又回来了", f)
		}
	}
}
