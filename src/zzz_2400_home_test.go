package main

// 2.4.0 新增：首页聚合 + 两项"仅手机版"行为的断言。
//
//	① 首页必须走**聚合根**（/api/list?path=/），不再列盘位；
//	② 手机版**不得**再"跳回上次目录"（桌面版保持）；
//	③ 离开确认闸门（syncExitTips）必须**跟状态走**：只在聚合根层挂、离开就清，
//	   且限定手机版。
//
// 这几条都是"光看有没有函数"查不出来的行为约束，所以用静态断言精确钉住表达式本身。

import (
	"strings"
	"testing"
)

func Test2400HomeIsAggregatedRoot(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	// ① 首页走聚合根
	if !strings.Contains(app, "API + '/list?path=' + enc('/')") {
		t.Error("首页必须请求**聚合根** `/api/list?path=/` —— " +
			"用户要求「不区分存储空间、只显示文件夹」（像飞牛文件管理器那样）")
	}
	// 首页返回的 dirs 必须转成 {kind:'dir'} —— 否则会被当成文件渲染（真 Edge 实测：格子空白）
	// ⚠️ 断言范围必须**限定在 buildRoot 体内**：别处（如 loadDir）也有同样的 map，
	//    只做全文 Contains 的话，变异打在别处就抓不到（M28 第一次就是这样逃逸的）。
	rootBody := jsFuncBody(app, "function buildRoot(")
	if rootBody == "" {
		t.Fatal("取不到 buildRoot 的函数体")
	}
	if !strings.Contains(rootBody, "{ kind: 'dir', name: x.name, path: x.path }") {
		t.Error("★ 首页的 dirs 必须 map 成 {kind:'dir', name, path} —— " +
			"渲染靠 item.kind 判定；漏了这一步文件夹会被当成文件（格子空白 + 白跑缩略图）")
	}
	if strings.Contains(rootBody, "state.items = (d && d.dirs) || [];") {
		t.Error("首页不能直接把后端的 DirEntry 当 items —— 它没有 kind 字段")
	}

	// 宿主原生弹窗（setExitPageTips）：
	//   · 参数名必须是 {title, content} —— 我 2.4.0/2.4.1 写成了 {tip}，宿主收不到有效参数、**根本不弹**；
	//   · 挂载条件必须含「查看器关着」与「已在聚合根层」—— 常挂会把"回上一层"整个吞掉。
	sy := jsFuncBody(app, "function syncExitTips(")
	if sy == "" {
		t.Fatal("取不到 syncExitTips 的函数体")
	}
	if !strings.Contains(sy, "title:") || !strings.Contains(sy, "content:") {
		t.Error("★★ setExitPageTips 的参数必须是 {title, content} —— 官方签名如此；" +
			"写成 {tip} 宿主收不到有效参数、根本不弹（2.4.1 实测踩过）")
	}
	if !strings.Contains(sy, "viewer.classList.contains('hidden')") {
		t.Error("宿主闸门必须含「查看器关着」—— 查看器里返回是「关查看器」，不该问退出")
	}
	// 2.4.3：判据改用 isAggregateRoot()（内存状态 + URL 双保险），不再直接写 state 判断
	if !strings.Contains(sy, "isAggregateRoot()") {
		t.Error("宿主闸门必须含「已在聚合根层」的判据 —— 常挂会把「回上一层」整个吞掉；" +
			"2.4.3 起用 isAggregateRoot()")
	}
	if !strings.Contains(sy, "(PROFILE === 'touch')") {
		t.Error("宿主闸门必须限定手机版（系统返回手势只在手机端由宿主处理）")
	}
	// 2.4.3：**闸门常挂**的根因是"页面级变量 ≠ 应用级宿主状态"：
	//   真导航整页重载后新页面里 exitTipsArmed 回到初始值，而宿主侧的应用级闸门还在。
	//   修法两条，缺一不可：
	//     ① 初始值必须是 **null**（"不知道宿主侧状态"）⇒ 每个新页面首次一定下发一次来纠正；
	//     ② 判据必须含 **URL**（聚合根页面 URL 固定）⇒ 重载后内存 state 未就绪也能判对。
	if !strings.Contains(app, "var exitTipsArmed = null;") {
		t.Error("★★ exitTipsArmed 初始值必须是 **null** —— 用 false 会让新页面" +
			"在子目录算出的 want 也是 false、判定状态没变而**不下发**，" +
			"于是上个页面挂的宿主闸门一直留着（实测：任何层返回都弹框）")
	}
	if !strings.Contains(app, "function isAggregateRoot()") {
		t.Fatal("缺少 isAggregateRoot() —— 聚合根层判据要看 URL")
	}
	ag := jsFuncBody(app, "function isAggregateRoot(")
	for _, frag := range []string{"location.search", "?&]dir=", "?&]path="} {
		if !strings.Contains(ag, frag) {
			t.Errorf("★ isAggregateRoot 必须看 URL（缺 %s）—— 真导航下 URL 是层级的唯一真相", frag)
		}
	}
	if !strings.Contains(sy, "isAggregateRoot()") {
		t.Error("★ syncExitTips 的判据必须用 isAggregateRoot()（内存状态 + URL 双保险）")
	}
	if !strings.Contains(app, "try { syncExitTips(); } catch (e) {}") {
		t.Error("关掉查看器后必须重新判定闸门（旧版 2.2.0 也有这一处）")
	}

	// 2.4.3：**闸门常挂**的根因是"页面级变量 ≠ 应用级宿主状态"：
	//   真导航整页重载后新页面里 exitTipsArmed 回到初始值，而宿主侧的应用级闸门还在。
	//   修法两条，缺一不可：
	//     ① 初始值必须是 **null**（"不知道宿主侧状态"）⇒ 每个新页面首次一定下发一次来纠正；
	//     ② 判据必须含 **URL**（聚合根页面 URL 固定）⇒ 重载后内存 state 未就绪也能判对。
	if !strings.Contains(app, "var exitTipsArmed = null;") {
		t.Error("★★ exitTipsArmed 初始值必须是 **null** —— 用 false 会让新页面" +
			"在子目录算出的 want 也是 false、判定状态没变而**不下发**，" +
			"于是上个页面挂的宿主闸门一直留着（实测：任何层返回都弹框）")
	}
	if !strings.Contains(app, "function isAggregateRoot()") {
		t.Fatal("缺少 isAggregateRoot() —— 聚合根层判据要看 URL")
	}
	if !strings.Contains(sy, "isAggregateRoot()") {
		t.Error("★ syncExitTips 的判据必须用 isAggregateRoot()（内存状态 + URL 双保险）")
	}
	if !strings.Contains(app, "try { syncExitTips(); } catch (e) {}") {
		t.Error("关掉查看器后必须重新判定闸门（旧版 2.2.0 也有这一处）")
	}

	// 自绘方案已弃用：不该再有这三个函数
	for _, dead := range []string{"function confirmDialog(", "function askExitApp(", "function exitApp("} {
		if strings.Contains(app, dead) {
			t.Errorf("不该再有 %s —— 用户要求只调用宿主原生弹窗，页面不自绘确认层", dead)
		}
	}

	// 至少两个调用点：进根层挂、离开清
	if got := strings.Count(app, "syncExitTips()"); got < 2 {
		t.Errorf("syncExitTips() 调用点应 >= 2（进根层挂 + 离开清），实际 %d", got)
	}
}

func Test2400DoubleTapZoomIsThree(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))
	if !strings.Contains(app, "v.scale = 3; v.zoomed = true;") {
		t.Error("双击放大应为 **3 倍**（2.4.0 按用户要求从 2 倍改为 3 倍）")
	}
	if strings.Contains(app, "v.scale = 2; v.zoomed = true;") {
		t.Error("还残留 2 倍的双击放大 —— 应已改为 3 倍")
	}
}

func Test2400ResetOnlyOnKeyboard(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	// swipeTo 里不得有归位（prev/next 都走它 ⇒ 会让点击退化成"先归位不切图"）
	i := strings.Index(app, "function swipeTo(")
	if i < 0 {
		t.Fatal("找不到 swipeTo()")
	}
	rest := app[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "if (v.scale >= SWIPE_LOCK_SCALE)") {
		t.Error("swipeTo 里**不该**再有归位 —— prev/next 都走它；" +
			"归位只应留在键盘 ←/→（点击要直接切图，这是用户明确要求的）")
	}

	// 键盘里必须有归位
	k := strings.Index(app, "addEventListener('keydown'")
	if k < 0 {
		t.Fatal("找不到 keydown 监听")
	}
	kd := app[k:]
	if j := strings.Index(kd, "\n  });"); j > 0 {
		kd = kd[:j]
	}
	if !strings.Contains(kd, "if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }") {
		t.Error("键盘 ←/→ 必须保留「先归位」（那个入口离得远、不易误触）")
	}
}

// jsFuncBody 取出某个具名函数的函数体（花括号配平）。
// 用于把断言**限定在目标函数内**，避免和其它地方的同名写法混淆。
func jsFuncBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	j := strings.Index(src[i:], "{")
	if j < 0 {
		return ""
	}
	j += i
	depth := 0
	for k := j; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : k+1]
			}
		}
	}
	return ""
}
