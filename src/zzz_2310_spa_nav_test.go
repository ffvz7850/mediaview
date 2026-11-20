package main

// 2.4.46：导航契约 = **单文档 SPA**（每层 pushState 一条 + 就地渲染 + popstate 按 event.state 落地）。
//
// 为什么把 2.4.40 的契约翻过来：
//   · 2.4.40 用 location.href 整页导航，换来"每层一条历史"，但代价是**每次返回都重载文档** ——
//     WebView 要重新拉 HTML/JS、重新鉴权、重建全部状态，慢、会闪，返回的落点也全交给宿主去猜
//     （"突然退出 / 不按层级返回"就是这么来的）。
//   · piGallery2 在同一个宿主上返回正常，它用的就是 SPA 路由：目录在 URL 里、切换只 pushState、
//     内容就地渲染，全程一个文档。这一版对齐它。
//
// 本文件守着五件事：
//   ① 两个导航入口走 pushNav（pushState）+ 就地 loadDir/buildRoot，不再整页跳转；
//   ② 首条历史条目被改写成显式 ?dir= 且**带 state**（返回判落点要用）；
//   ③ popstate 按 event.state（mvDir / mvRoot）决定渲染哪一层，不再靠"查看器是否可见"猜；
//   ④ 查看器只压一条 {mvViewer:1}（压栈点唯一，多一条会让返回要按两次）；
//   ⑤ 进目录与回上级是同一条路径（调用点仍在）。

import (
	"strings"
	"testing"
)

func Test2446NavigationIsSpa(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	// ① 两个入口：pushState + 就地渲染，且必须记住滚动
	type navFn struct {
		sig   string
		wants []string
	}
	for _, f := range []navFn{
		{"function navigateTo(", []string{"pushNav(st, path)", "loadDir(path)", "rememberScroll();"}},
		{"function navigateRoot(", []string{"pushNav(st, '/')", "buildRoot()", "rememberScroll();"}},
	} {
		body := segmentBetween(app, f.sig, "\n  }")
		if body == "" {
			t.Fatalf("找不到 %s", f.sig)
		}
		for _, w := range f.wants {
			if !strings.Contains(body, w) {
				t.Errorf("★★ %s 里缺 %q：换目录必须是 pushState + 就地渲染"+
					"（整页导航会让每次返回都重载文档）", f.sig, w)
			}
		}
		if strings.Contains(body, "location.replace(") {
			t.Errorf("★★ %s 不许用 location.replace：它只改当前条目，历史里不会多出一层", f.sig)
		}
	}

	// ② 入口条目：URL 改写成显式 ?dir=，并把 state 一起写上
	for _, need := range []string{
		"history.replaceState(st, ''",
		"{mvRoot: 1}",
		"fixEntryURL(dirParam);",
	} {
		if !strings.Contains(app, need) {
			t.Errorf("★ 入口条目缺少 %q：返回时要靠它判断「该回哪一层」", need)
		}
	}

	// ③ 2.4.47：popstate 只把 **URL** 应用成界面（不再按 event.state / 查看器可见性分流）
	for _, need := range []string{
		"if (u.dir !== curPath())",
		"if (u.photo)",
		"if (!viewerHidden()) closeViewer({ fromUrl: true });",
		"if (!singleMode) window.addEventListener('popstate', mvOnPop);",
	} {
		if !strings.Contains(app, need) {
			t.Errorf("★★ applyUrl 缺少 %q：界面必须由 URL 决定（目录 + `&p=`），"+
				"否则一次 pop 的落点会被吞掉，用户看到「跳层 / 退出」", need)
		}
	}
	// popstate 的入口函数**只许调用 applyUrl**，不许自己动历史
	if !strings.Contains(app, "function mvOnPop() { applyUrl(); }") {
		t.Error("★★ mvOnPop 只许是 `applyUrl()` 一行 —— 自己判查看器/补历史就是" +
			"「一次滑动退两层」的来源")
	}

	// ④ 查看器：压栈一处（打开）+ 改写一处（换张），且都不许多压
	if got := strings.Count(app, "pushNav({ mvViewer: 1 }, curPath(), target.path)"); got != 1 {
		t.Errorf("★★ 查看器压栈应恰好 1 处，实际 %d —— 多压一条，返回就得按两次", got)
	}
	if got := strings.Count(app, "replaceNav({ mvViewer: 1 }, u.dir || curPath(), path)"); got != 1 {
		t.Errorf("★ 大图内换张应恰好 1 处 replaceNav（换张不许涨历史），实际 %d", got)
	}

	// ⑤ 进目录与回上级仍是同一条路径
	for _, need := range []string{
		"navigateTo(item.path)",
		"if (path === null) navigateRoot(); else navigateTo(path);",
		"if (!up || up === '/') { navigateRoot(); return; }",
		"navigateTo(up);",
	} {
		if !strings.Contains(app, need) {
			t.Errorf("★ 缺导航调用点 %q（进目录与回上级必须是同一条路径）", need)
		}
	}

	// ⑥ 回上级在"从上一层点进来"时应回退历史，而不是再压一条（锯齿历史）
	if !strings.Contains(app, "if (cur && cur.from === up) { history.back(); return; }") {
		t.Error("★ goUpLevel 缺少 from 判定：箭头回上一层会压新条目，" +
			"之后再按系统返回就退回刚离开的子目录")
	}

	// ⑦ boot 必须认 ?dir=（刷新/整页落地后靠它恢复）
	if !strings.Contains(app, "var dirParam = qs.get('dir');") {
		t.Error("boot 必须读 `?dir=`")
	}
}
