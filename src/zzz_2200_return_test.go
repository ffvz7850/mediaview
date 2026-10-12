package main

// zzz_2200_return_test.go —— 2.2.0「去掉手势返回 + 手机顶栏瘦身」
//
// ⚠️ 本文件在 2.2.1 瘦身过一次：原来还有三条测试（退出确认框的 DOM、
//    bridge 透传 setExitPageTips、返回流程探针）。2.2.1 按用户要求把
//    "第一级弹确认框"整个撤掉了（改成**无响应**），那三条随之作废，已移出本文件；
//    新的返回契约（无响应 + 历史守卫）在 zzz_2210_return_test.go 里。
//
// 保留的两条与"确认框"无关，是本版仍要守住的东西：
//   ① 手势返回**真的没有了**（静态 + 反向）；
//   ② 手机顶栏隐藏面包屑、桌面保留。

import (
	"strings"
	"testing"
)

// Test2200NoReturnGestureLeft 反向钉住：手势返回整块不存在，且 app.js 不再给手势
// 暴露"返回动作"的出口。
//
// 为什么既要静态又要行为（zzz_2100 第 6 段）：静态只能证明"符号没了"，
// 行为才能证明"用户真在右边缘滑一下，不会发生任何事"。
func Test2200NoReturnGestureLeft(t *testing.T) {
	touch := stripLineComments(mustReadWebFile(t, "touch.js"))
	// 这些是边缘返回手势独有的符号。任何一条复活，都说明有人把它接回来了。
	for _, sym := range []string{
		"edgeStart", "updateEdge", "finishEdge",
		"EDGE_PX", "EDGE_MAX_SHIFT", "BACK_TRIGGER_PX", "EDGE_MARGIN_Y",
	} {
		if strings.Contains(touch, sym) {
			t.Errorf("touch.js 里仍有 %s —— 右边缘横滑返回已按用户要求整块删除，不许残留", sym)
		}
	}

	app := stripLineComments(mustReadWebFile(t, "app.js"))
	// 精确形态：只有 __mvMobile 的暴露处才会这样写（避免误伤同名函数定义）
	for _, m := range []string{"closeViewer: closeViewer,", "goUpLevel: goUpLevel,"} {
		if strings.Contains(app, m) {
			t.Errorf("__mvMobile 仍在暴露 %s —— 那是给已删除的返回手势准备的出口，"+
				"接口越窄越不会误碰（这层隔离的意义就在这里）", m)
		}
	}
}

// Test2200TopbarHidesCrumbOnlyOnTouch 手机上左上角只留「箭头 + 工具」：
// 面包屑整块隐藏，但**只在触屏块里**隐藏（桌面那套是主要导航，必须保留）。
func Test2200TopbarHidesCrumbOnlyOnTouch(t *testing.T) {
	css := mustReadWebFile(t, "style.css")

	hides := false
	for _, b := range mv2100MediaBlocks(css) {
		if strings.Contains(b.prelude, "pointer: coarse") &&
			strings.Contains(b.body, "#topbar .crumb { display: none; }") {
			hides = true
		}
	}
	if !hides {
		t.Error("触屏块里缺少 `#topbar .crumb { display: none; }` —— " +
			"手机左上角还会挂着「存储空间 / vol1 / …」那一串，把工具按钮挤到看不清")
	}

	// 反向：桌面（媒体块之外）不许隐藏面包屑
	top := mv2100StripMediaBlocks(css)
	if strings.Contains(top, ".crumb { display: none") {
		t.Error("桌面也把面包屑隐藏了 —— 桌面端面包屑是主要导航方式，只该在触屏档位隐藏")
	}
}
