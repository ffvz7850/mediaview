package main

// 1.8.72：大图切换「连点被吞」与「排序不持久化」的回归守卫。
//
// 用户报的现象：从飞牛文件管理器双击打开一张图，初始显示正确，但**一按切换就乱套**。
// 根因有两处，都在前端：
//   ① swipeTo 先等目标图 decode()，期间 v.index 还没变；这期间的再次点击只是把
//      旧轮次作废，新轮次算出的目标图仍是同一张 →「点两次只前进一张」，
//      连点几下切换顺序就全乱。
//   ② 排序只存在内存里，而文件管理器每打开一张图都是**全新的 iframe 实例**，
//      用户设好的排序换个窗口就丢了（默认值恰好是 mtim:desc，所以初始看着是对的）。
//
// 另外顺手修掉：目录列表异步返回时按「打开时那张」定位，可能把已切换的用户弹回原图。
import (
	"os"
	"strings"
	"testing"
)

func appJS1872(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func Test1872SwipeDoesNotDropClicks(t *testing.T) {
	js := appJS1872(t)

	// ① 等解码期间必须有「待定」标记，并且被置起来
	if !strings.Contains(js, "v.pendingSwipe = true;") {
		t.Error("没有 pendingSwipe 标记 —— 等目标图解码期间的点击会被吞掉（点两次只前进一张）")
	}
	// ② 快速路径必须覆盖「动画中 **或** 等解码中」
	if !strings.Contains(js, "if (track.classList.contains('anim') || v.pendingSwipe) {") {
		t.Error("快速切换分支没有覆盖『正在等解码』的情况 —— 连点仍会丢点击")
	}
	// ③ 快速路径必须真的跳转，而不是像旧实现那样只作废旧轮次
	if !strings.Contains(js, "v.pendingSwipe = false;\n      jumpToIndex(track, dir);") {
		t.Error("快速切换分支没有调用 jumpToIndex —— 点击仍被静默丢弃")
	}
	// ④ 跳转函数必须存在且推进索引
	if !strings.Contains(js, "function jumpToIndex(track, dir) {") {
		t.Error("缺少 jumpToIndex：无法在不叠加动画的情况下兑现连点")
	}
	if !strings.Contains(js, "v.index += dir;\n    if (v.index < 0) v.index = 0;") {
		t.Error("jumpToIndex 没有推进 v.index")
	}
	// ⑤ 过期轮次必须先 return，再清 pending 标记；顺序颠倒会把新一轮的保护清掉
	if !strings.Contains(js, "if (mySeq !== swipeSeq) return;\n        v.pendingSwipe = false;") {
		t.Error("doIt 里应先做过期判定、再清 pendingSwipe —— 否则过期轮次会把新一轮的标记清掉")
	}
}

func Test1872SortIsPersisted(t *testing.T) {
	js := appJS1872(t)

	if !strings.Contains(js, "localStorage.getItem('mediaview_sort')") {
		t.Error("排序没有从 localStorage 恢复 —— 每次从文件管理器打开都是新 iframe 实例，排序会退回默认值")
	}
	if !strings.Contains(js, "localStorage.setItem('mediaview_sort', state.sort)") {
		t.Error("手动改排序后没有落盘 —— 换个窗口就丢")
	}
	// 默认值必须仍是 mtim:desc（与文件管理器默认视图一致），别在改动中被顺手改掉
	if !strings.Contains(js, "|| 'mtim:desc';") {
		t.Error("排序默认值不再是 mtim:desc")
	}
}

func Test1872ListLocatesCurrentlyShown(t *testing.T) {
	js := appJS1872(t)

	if !strings.Contains(js, "var shown0 = v.list[v.index];") {
		t.Error("异步列表返回时没有取『当前正在显示的那张』")
	}
	if !strings.Contains(js, "var want = normPath(shown0 ? shown0.path : path);") {
		t.Error("异步列表仍按打开时的 path 定位 —— 会把已切换的用户弹回原图")
	}
}
