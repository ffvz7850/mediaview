package main

// 1.8.92：切图收敛为「单真源 + 幂等渲染」。
//
// 之前的问题是状态分散在 7 处（v.index / v.list / track.children / v.media /
// swipeSeq / pendingSwipe / 各种延迟回调），靠"谁先谁后"的约定保持一致，
// 于是每修一处又冒出另一处：
//   · 点击被 8px 守卫吞掉
//   · 计数在加、图片不切换（缓存命中 onload 不触发）
//   · 一次点击跳两张（索引双重推进）
//   · 连点后画面停在第 2 张（旧回调晚到，手工增删子元素把它改回去）
//
// 收敛点：**所有收尾都调用 syncTrackToIndex(track)** —— 它完全由 v.index 派生、
// 复用已在 DOM 里的元素，因此重复调用没有副作用。任何一条延迟回调落地，
// 都只会把轨道校正到正确状态，而不是把它改错。
import (
	"os"
	"strings"
	"testing"
)

func Test1892SettleIsIdempotent(t *testing.T) {
	js, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)

	i := strings.Index(s, "function settleAfterSwipe")
	if i < 0 {
		t.Fatal("找不到 settleAfterSwipe")
	}
	end := strings.Index(s[i:], "function jumpToIndex")
	if end < 0 {
		end = 2200
	}
	seg := s[i : i+end]

	if !strings.Contains(seg, "syncTrackToIndex(track)") {
		t.Error("settleAfterSwipe 没有用幂等重排（syncTrackToIndex）—— 延迟回调仍可能把轨道改错")
	}
	if strings.Contains(seg, "insertBefore(createMediaBox") || strings.Contains(seg, "appendChild(createMediaBox") {
		t.Error("settleAfterSwipe 仍在手工增删子元素 —— 连点/取消时会错位（这正是「停在第二张」的成因）")
	}
}

// 幂等渲染函数本身要满足：由 v.index 派生、复用已有元素、可重复调用。
func Test1892RenderIsIdempotent(t *testing.T) {
	js, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	i := strings.Index(s, "function syncTrackToIndex")
	if i < 0 {
		t.Fatal("找不到 syncTrackToIndex")
	}
	seg := s[i : i+2600]
	if !strings.Contains(seg, "var want = v.list[v.index];") {
		t.Error("syncTrackToIndex 不是由 v.index 派生的")
	}
	if !strings.Contains(seg, "keep = b") {
		t.Error("syncTrackToIndex 没有复用已在 DOM 里的元素 —— 会闪屏")
	}
	if !strings.Contains(seg, "track.style.transform = 'translateX(-100%)'") {
		t.Error("syncTrackToIndex 没有把轨道复位到中间")
	}
}

// 索引只能有一处推进：swipeTo 入口（点击/键盘）与 endSwipe（手势）。
// doSwipe / jumpToIndex 都不许再动 v.index，否则「一次点击跳两张」。
func Test1892SingleIndexAdvance(t *testing.T) {
	js, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)

	for _, fn := range []string{"function doSwipe", "function jumpToIndex"} {
		i := strings.Index(s, fn)
		if i < 0 {
			t.Fatalf("找不到 %s", fn)
		}
		seg := s[i : i+900]
		if strings.Contains(seg, "v.index++") || strings.Contains(seg, "v.index--") ||
			strings.Contains(seg, "v.index += dir") {
			t.Errorf("%s 里仍在推进 v.index —— 会与入口的推进叠加成「跳两张」", fn)
		}
	}
}
