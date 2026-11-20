package main

// zzz_2210_return_test.go —— 2.2.1「第一级无响应 + 历史守卫阻止滑动返回退出」
//
// 用户两条要求：
//   ① 箭头退到第一级 = **无响应**（不弹「是否退出媒体浏览」，也不退出应用）；
//   ② 网格里滑动返回 = **无反应**（不退出应用）。
//
// ② 的做法：触屏档启动时压一条常驻守卫条目（history.pushState），popstate 里立刻把
// 守卫补回去，除此之外什么都不做。唯一例外是**大图查看器** —— 那里页面内没有任何
// 返回出口（大图的 ← 按钮 2.1.1 已删、手势 2.2.0 已删），若把返回也吃掉就成了死胡同，
// 所以查看器开着时要把它关掉。
//
// 为什么必须真跑（node + 假 history/window）：静态断言只能证明"符号在"，
// 证明不了"连退五次不会把页面退出去"，也证明不了"我们一次都没有主动退历史"。

import (
	"os/exec"
	"strings"
	"testing"
)

// Test2210ExitConfirmLayerRemoved 反向：2.2.0 的「退出媒体浏览？」确认层必须彻底消失
// —— DOM、样式、代码三处都要干净。
//
// ⚠️ 三处都查是有教训的：本项目栽过"只删样式不删 DOM"（留下一坨看不见的死标记），
// 也栽过"删了函数定义却留着调用点"（跑到那里直接 ReferenceError）。
func Test2210ExitConfirmLayerRemoved(t *testing.T) {
	html := mustReadWebFile(t, "index.html")
	for _, id := range []string{"confirmBox", "confirmOk", "confirmCancel", "confirmMask", "confirmTitle", "confirmText"} {
		if strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 里仍有 id=%q —— 第一级返回改成无响应后，这个确认层不该再存在", id)
		}
	}

	css := mv2100StripCSSComments(mustReadWebFile(t, "style.css"))
	for _, cls := range []string{".confirm-box", ".confirm-text"} {
		if strings.Contains(css, cls) {
			t.Errorf("style.css 里仍有 %s 的样式 —— 结构已删，样式就是死代码", cls)
		}
	}

	// ⚠️ 必须剥注释再查：这几个名字我在源码注释里**特意写过**（"这里曾有…"），
	// 不剥注释的话反向断言会被自己的注释满足（本项目栽过的"被注释骗"）。
	app := stripLineComments(mustReadWebFile(t, "app.js"))
	// 2.4.0：`syncExitTips` 从这张"死代码"名单里**移出** —— 用户明确要求
	//   "在根层返回时加装是否退出软件的弹窗"，它现在有明确调用点
	//   （buildRoot 成功后挂、进子目录与开查看器时清）。
	//   自造的弹窗仍然禁止：我们用宿主闸门，不在页面里自己画一个确认框。
	for _, def := range []string{
		"function confirmDialog(", "function askExitApp(", "function exitApp(",
	} {
		if strings.Contains(app, def) {
			t.Errorf("app.js 里仍有 %s —— 没有调用者的函数就是死代码，删掉（将来要用照注释恢复）", def)
		}
	}
	for _, call := range []string{"askExitApp(", "confirmDialog(", "exitApp("} {
		if strings.Contains(app, call) {
			t.Errorf("app.js 里仍有对 %s 的调用 —— 函数都删了，这会是运行时 ReferenceError", call)
		}
	}
	// 2.4.0 正向断言：syncExitTips 必须存在、且**必须真的被调用**
	//   （只有定义没有调用同样是死代码；它必须跟状态走 —— 根层挂、离开清）。
	if !strings.Contains(app, "function syncExitTips(") {
		t.Error("缺少 syncExitTips() —— 2.4.0 起由它负责「根层挂离开确认、其它层清掉」")
	}
	if strings.Count(app, "syncExitTips()") < 2 {
		t.Error("syncExitTips() 的调用点太少：必须既在**进根层时挂**、也在**离开根层时清**" +
			"（只挂不清会把「回上一层」整个吞掉 —— 2.1.4 实测踩过这个坑）")
	}
	if !strings.Contains(app, "PROFILE !== 'touch'") {
		t.Error("syncExitTips 必须限定手机版（用户要求 4、5 只在手机版生效）")
	}
}

// Test2210BridgeExportsExitTips 2.4.0：bridge.js **必须**透传 setExitPageTips。
//
// 历史：2.2.0 加过、2.2.1 因为"它拦不住返回、只能问一句"撤掉。
// 2.4.0 用户明确要求在**根层**加这个确认（"在根层返回加装是否退出软件的弹窗"）——
// 而"只问一句"在根层恰恰是正确的语义（根层再返回就是离开应用）。
// ⚠️ 铁律不变：**必须跟状态走**（只在根层挂，离开就清），否则会把"回上一层"吞掉。
func Test2210BridgeExportsExitTips(t *testing.T) {
	bridge := stripLineComments(mustReadWebFile(t, "bridge.js"))
	if !strings.Contains(bridge, "function setExitPageTips(") {
		t.Error("bridge.js 必须透传 setExitPageTips（2.4.0 起根层的离开确认要用它）")
	}
	if !strings.Contains(bridge, "setExitPageTips:") {
		t.Error("setExitPageTips 必须挂到对外契约上（window.fnosBridge），否则 app.js 调不到")
	}
}

// Test2210HistoryDiscipline 静态钉住"历史只被用在一个地方"（2.4.40 重整后的纪律）：
//
//	① 换目录是**真页面导航**（location.href）—— navigateTo / navigateRoot / goUpLevel 不碰历史；
//	② 全文件只有 1 处 pushState，就是查看器自己那一层；
//	③ history.back() 最多 1 处，且只允许在 closeViewer 的主动关闭路径上；
//	④ replaceState 只有 1 处（boot 把首条条目改写成显式 ?dir=），且带 qsHadDir 保护；
//	⑤ 2.4.33 ~ 2.4.39 那套「守卫条目」必须彻底消失 —— 半留半删比不删更危险。
func Test2210HistoryDiscipline(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	jsBody := func(sig string) string {
		i := strings.Index(app, sig)
		if i < 0 {
			return ""
		}
		j := strings.Index(app[i:], "{")
		if j < 0 {
			return ""
		}
		j += i
		depth := 0
		for k := j; k < len(app); k++ {
			if app[k] == '{' {
				depth++
			} else if app[k] == '}' {
				depth--
				if depth == 0 {
					return app[i : k+1]
				}
			}
		}
		return ""
	}

	// ① 2.4.46 契约：导航 = pushState + 就地渲染（**同一个文档**，不整页跳）。
	//    每层目录仍然是一条历史（返回能逐层退），但返回不再触发文档重载。
	for _, fn := range []string{"function navigateTo(", "function navigateRoot("} {
		body := jsBody(fn)
		if body == "" {
			t.Fatalf("找不到 %s", fn)
		}
		if !strings.Contains(body, "pushNav(") {
			t.Errorf("★★ %s 必须用 pushNav()（pushState）压一条历史 —— 返回要靠它逐层退", fn)
		}
		if !strings.Contains(body, "rememberScroll();") {
			t.Errorf("★ %s 必须调 rememberScroll()：离开前把\"看到第几屏\"写进快照", fn)
		}
		if strings.Contains(body, "history.replaceState(") {
			t.Errorf("%s 不许用 replaceState：它只改当前条目，历史里不会多出一层", fn)
		}
	}
	if !strings.Contains(jsBody("function navigateTo("), "loadDir(") {
		t.Error("★★ navigateTo 必须就地 loadDir()：整页导航会让每次返回都重载文档")
	}
	if !strings.Contains(jsBody("function navigateRoot("), "buildRoot()") {
		t.Error("★★ navigateRoot 必须就地 buildRoot()")
	}
	// 逐层返回必须复用同一条导航路径（进目录真导航、回上级内部切换 = 两套语义）。
	upBody := jsBody("function goUpLevel(")
	if !strings.Contains(upBody, "navigateTo(up);") || !strings.Contains(upBody, "navigateRoot();") {
		t.Error("goUpLevel 必须走 navigateTo / navigateRoot（与进目录同一条路径）")
	}

	// ② 压栈点只有一处出口：pushNav（目录层和查看器共用）
	if n := strings.Count(app, "history.pushState("); n != 1 {
		t.Errorf("★★ history.pushState( 应恰好 1 处（都在 pushNav 里），实际 %d 处 —— "+
			"多出来就是重复记账（返回要按两次）", n)
	}
	if !strings.Contains(jsBody("function pushNav("), "history.pushState(") {
		t.Error("★★ pushNav() 必须是唯一的 pushState 出口")
	}
	// 2.4.47：查看器必须把「看哪张」写进 URL（&p=），否则又回到"靠 state 记账"，
	//   一旦 pop 落点与预期不符，URL 上看不出差别，界面就会和 URL 脱节。
	viewerBody := jsBody("function openViewerOverlay(")
	if !strings.Contains(viewerBody, "pushNav({ mvViewer: 1 }, curPath(), target.path)") {
		t.Error("★★ openViewerOverlay() 必须用 pushNav 把 `&p=` 写进 URL —— " +
			"URL 里看不见大图，就等于又回到靠内部状态记账")
	}
	if !strings.Contains(jsBody("function navUrl("), "'&p='") {
		t.Error("★★ navUrl() 必须支持 `&p=`：大图是 URL 的一层，不能只藏在 history.state 里")
	}
	if !strings.Contains(viewerBody, "opts.fromUrl") {
		t.Error("★ openViewerOverlay() 必须认 fromUrl：从 URL 打开（前进/刷新/深链）时不能再压一条")
	}
	if !strings.Contains(app, "if (!singleMode) window.addEventListener('popstate', mvOnPop);") {
		t.Error("popstate 接线必须是 `if (!singleMode) ...` —— " +
			"SPA 下桌面浏览器的后退键同样会换 URL 而不重载，不接管就是一片空白状态；" +
			"单图窗口本身就是一层，挂上去会抢走「关窗」")
	}
	if n := strings.Count(app, "'popstate'"); n != 1 {
		t.Errorf("popstate 应恰好注册 1 处，实际 %d 处 —— 重复注册会让一次返回被处理多次", n)
	}

	// ③ back 只允许两处：closeViewer 的主动关闭补偿 + goUpLevel 的"从上一层点进来"回退
	if n := strings.Count(app, "history.back("); n != 2 {
		t.Errorf("history.back( 应恰好 2 处（closeViewer 补偿 / goUpLevel 回退），实际 %d 处", n)
	}
	if !strings.Contains(jsBody("function goUpLevel("), "cur.from === up") {
		t.Error("★★ goUpLevel 必须按 from 判定：从上一层点进来的话应 history.back()，" +
			"而不是再压一条（否则历史成锯齿，返回会退回刚离开的子目录）")
	}
	closeBody := jsBody("function closeViewer(")
	if !strings.Contains(closeBody, "history.back(") {
		t.Error("closeViewer() 必须补一次 history.back() —— 打开查看器时压了一条条目，" +
			"主动关闭（✕/背景/Esc/流内下滑）不收回，用户下一次返回会直接退掉一层目录")
	}
	if !strings.Contains(closeBody, "urlState().photo") {
		t.Error("★ closeViewer() 必须按 URL 里有没有 `p` 判断该不该退历史 —— URL 是唯一真相，" +
			"不要再靠 mvViewerFromPop 这种内部标记（它会和真实落点脱节）")
	}
	if !strings.Contains(closeBody, "opts.fromUrl") {
		t.Error("★★ closeViewer() 必须按 fromUrl 分流 —— 返回触发时 URL 已经退好了，" +
			"此时再补一次 back 就是一次返回退两层")
	}
	if strings.Contains(closeBody, "mvViewerFromPop") {
		t.Error("★★ closeViewer() 不许再用 mvViewerFromPop：那套内部标记正是" +
			"「一次滑动退两层」的来源，2.4.47 已改成「URL 有没有 p」")
	}
	if strings.Contains(jsBody("function mvOnPop("), "history.") {
		t.Error("mvOnPop 里不许碰历史 —— 那时历史已经退了，再退一次会和宿主那一次叠加（一滑退两层）")
	}

	// ④ replaceState = replaceNav 一处（换张 / 收 p）+ fixEntryURL 两个分支
	if n := strings.Count(app, "history.replaceState("); n != 3 {
		t.Errorf("history.replaceState( 应恰好 3 处（replaceNav + fixEntryURL 两分支），实际 %d 处", n)
	}
	if !strings.Contains(jsBody("function replaceNav("), "history.replaceState(") {
		t.Error("★★ replaceNav() 必须承担「同一层里换状态」——大图内换张不许涨历史")
	}
	fixBody := jsBody("function fixEntryURL(")
	if !strings.Contains(fixBody, "history.replaceState(") {
		t.Fatal("★ fixEntryURL 必须用 replaceState —— 它只改当前条目的 URL，不新增条目")
	}
	if !strings.Contains(fixBody, "qsHadDir") {
		t.Error("★ fixEntryURL 必须用 qsHadDir 保护：已经带 ?dir= 的页面（真导航落地）URL 就是真相，不许改写")
	}
	if strings.Contains(fixBody, "history.back(") || strings.Contains(fixBody, "history.pushState(") {
		t.Error("★ fixEntryURL 里不许 back / pushState")
	}

	// ⑤ 守卫体系与 SPA 专用件必须彻底消失
	for _, dead := range []string{
		"mvGuard", "MV_GUARD_DEPTH", "mvTxAction", "mvSyncURL", "mvPushGuardOnce",
		"dirViews", "saveDirView", "restoreDirView", "prefetchDir", "takePrefetch",
		"cancelPendingGridRequests", "resumeCanceledThumbs", "mobileSpaNav",
	} {
		if strings.Contains(app, dead) {
			t.Errorf("★★ %s 属于「内部切换 + 历史守卫」那套，真导航后已无意义 —— 必须删净"+
				"（两套机制并存时，谁也说不出哪条在生效）", dead)
		}
	}
}

// Test2210LevelOneIsSilent 用 node 真跑 goUpLevel：
//
//	第一级 → **什么都不发生**（不退目录、不重建根、不请宿主关窗口）；
//	子目录 → 仍要能正常回上级（这条不能因为"无响应"一起丢掉）。
func Test2210LevelOneIsSilent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 源码里找不到 ' + name); process.exit(1); }
  var depth = 0, j = appjs.indexOf('{', i);
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL ' + name + ' 括号不配平'); process.exit(1);
}
function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL ' + msg + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

var state = { root: true, path: null };
var hostCalls = [], loaded = [], rooted = 0, asks = 0;
// 2.4.2：留一个桩用于断言"不该再被调用"（自绘方案已弃用）
function askExitApp() { asks++; }
// 2.3.1：goUpLevel 现在走 navigateTo / navigateRoot（触屏档真导航）。
//   探针把两者都当成"换目录"记录，于是不论实现是内部切换还是真导航，
//   "该不该退、退到哪一层"这个契约都照样能被验证。
function navigateRoot() { rooted++; }
function navigateTo(p) { loaded.push(p); }
function buildRoot() { rooted++; }   // 保留：桌面档与降级路径仍用它
function loadDir(p) { loaded.push(p); }
var code =
  'function hostCall(m) { hostCalls.push(m); return { catch: function () {} }; }' +
  // 2.4.18：goUpLevel 还会问 isCustomRoot(up)（上一层是不是已添加的文件夹）。
  //   这里是"未开启该选项"的场景，把 cfg 桩成空列表即可。
  'var cfg = { onlyRoots: false, roots: [] };' +
  grab('isCustomRoot') + ';' +
  grab('normPath') + ';' + grab('dirName') + ';' +
  // 2.4.1：goUpLevel 现在用 isInsideAnyUserDir 判"是否还在用户目录之内"
  grab('isInsideAnyUserDir') + ';' + grab('goUpLevel') + ';' +
  'return { up: goUpLevel, st: state,' +
  '  snap: function () { return { loaded: loaded, rooted: rooted, host: hostCalls, asks: asks }; } };';
var A = new Function('state', 'navigateRoot', 'navigateTo', 'buildRoot', 'loadDir', 'hostCalls',
                     'askExitApp', code)(
  state, navigateRoot, navigateTo, buildRoot, loadDir, hostCalls, askExitApp);

// A1：第一级 —— 2.4.2 起弹**自绘**确认层（页面内入口）；系统返回走宿主原生框（见 GuardStopsHostBack）
A.st.root = true; A.st.path = null;
A.up();
eq(A.snap().loaded.length, 0, '★ 第一级返回不得退目录');
eq(A.snap().rooted, 0, '★ 第一级返回不得重建根');
// 2.4.2：聚合根层 goUpLevel **什么都不做** —— "是否退出"由**宿主原生弹窗**负责
//   （syncExitTips 在进根层时把 setExitPageTips 挂上；系统返回手势由宿主处理并弹它自己的框）。
//   所以这里既不自绘框、也不请宿主关窗口。
eq(A.snap().asks, 0, '★ 聚合根层不该自绘确认框（只用宿主原生弹窗）');
eq(A.snap().host.length, 0, '★★ goUpLevel 不得直接请宿主关闭应用');

// A2：子目录 —— 仍要能回上级（别把功能一起删掉）
A.st.root = false; A.st.path = '/vol1/1000/pics';
A.up();
eq(A.snap().loaded.join(','), '/vol1/1000', '子目录里箭头必须仍能回上级 —— 无响应只针对第一级');
eq(A.snap().host.length, 0, '子目录返回也不该请宿主关窗口');

// A3：2.4.1 起 /vol1（存储空间那一层）**不再是可达层级** —— 直接回聚合根。
//   用户要求："到聚合文件夹目录就应该停止，现在还能退到储存空间目录的文件夹然后再返回退到储存空间"。
A.st.root = false; A.st.path = '/vol1';
A.up();
eq(A.snap().rooted, 1, '/vol1 不在任何用户目录之内 ⇒ 直接回聚合根（不再显示"存储空间"这一层）');
// A3b：/vol1/1000 再往上也是聚合根（1000 是用户目录本身，它的父级就是存储空间层）
A.st.root = false; A.st.path = '/vol1/1000';
A.up();
eq(A.snap().rooted, 2, '/vol1/1000 的上一级是存储空间层 ⇒ 应回聚合根');
// A3c：用户目录内部仍要能逐层退（这条最关键，别把正常回退一起弄坏）
A.st.root = false; A.st.path = '/vol2/1000/图库/照片';
A.up();
eq(A.snap().loaded[A.snap().loaded.length - 1], '/vol2/1000/图库',
   '★ 用户目录内部必须仍能逐层回退（只拦"出用户目录"这一步）');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("第一级无响应探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
