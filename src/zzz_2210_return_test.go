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

// Test2210GuardWiringIsTouchOnlyAndNeverCallsBack 静态钉住守卫机制的三条纪律：
//
//	① 接线包在 `if (PROFILE === 'touch')` 里（桌面不该被改动 —— 手机与桌面不搅和）；
//	② 全文件只有 mvGuardArm 里那一处 pushState；
//	③ **没有任何 history.back()** —— 我们永不主动退历史。
func Test2210GuardWiringIsTouchOnlyAndNeverCallsBack(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	if !strings.Contains(app, "if (PROFILE === 'touch' && !singleMode) mvGuardInit();") {
		t.Error("守卫接线必须是 `if (PROFILE === 'touch' && !singleMode) mvGuardInit();` —— " +
			"① 只在触屏档（桌面有自己的关闭按钮，不该被这套机制改动）；" +
			"② **单图 / 独立看图窗口里不许压** —— 那个窗口本身就是一层，" +
			"返回的正确语义是关掉它，压了守卫反而让宿主去退历史（2.2.2 的实测结论）")
	}
	// 取某个函数的函数体（大括号配平）。用于**逐处校验**压栈/退历史出现在哪。
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

	// 2.3.0：pushState 从 1 处变成 2 处 —— 但**每一处都必须落在受控位置**，
	//   而不是简单放宽计数（那样"历史膨胀"就没人管了）：
	//     ① mvGuardArm()      → 那条常驻守卫（地板）
	//     ② openViewerOverlay → 查看器自己的一层（带 mvViewer 标记，仅触屏档）
	if n := strings.Count(app, "history.pushState("); n != 2 {
		t.Errorf("history.pushState( 应恰好 2 处（守卫 1 + 查看器 1），实际 %d 处 —— "+
			"多出来的压栈点会让历史膨胀、也会让\"守卫只有一条\"不再成立", n)
	}
	guardBody := jsBody("function mvGuardArm(")
	if !strings.Contains(guardBody, "history.pushState(") {
		t.Error("mvGuardArm() 里必须压那条常驻守卫条目")
	}
	viewerBody := jsBody("function openViewerOverlay(")
	if !strings.Contains(viewerBody, "history.pushState(") || !strings.Contains(viewerBody, "mvViewer: 1") {
		t.Error("openViewerOverlay() 里必须压一条带 mvViewer 标记的**查看器历史** —— " +
			"否则查看器没有自己的历史层级，返回会退掉上一层目录页（真导航重载），" +
			"我们的 popstate/closeViewer 根本没机会跑（2.3.0 修的正是这个）")
	}
	// 2.3.0：允许 1 处 history.back()，但**只允许在 closeViewer 的主动关闭路径**上。
	//   理由：我们压的条目只有我们自己收得回来；不补的话"点关闭按钮关大图 → 再按返回"
	//   会直接退掉一层目录（多退一层）。
	if n := strings.Count(app, "history.back("); n > 1 {
		t.Errorf("history.back( 最多 1 处（只允许 closeViewer 的主动关闭路径），实际 %d 处", n)
	}
	closeBody := jsBody("function closeViewer(")
	if strings.Contains(app, "history.back(") && !strings.Contains(closeBody, "history.back(") {
		t.Error("history.back( 只允许出现在 closeViewer() 的主动关闭路径上")
	}
	// 正向断言：还必须"收得回来"且"按来源分流"。
	//   少了这两条，把 history.back 整行删掉也不会被抓红（那就是假绿）。
	if !strings.Contains(closeBody, "history.back(") {
		t.Error("closeViewer() 必须补一次 history.back() —— 打开查看器时压了一条 mvViewer 条目，" +
			"点关闭按钮/背景/Esc 主动关闭时若不收回，用户**下一次**按返回会直接退掉一层目录（多退一层）")
	}
	if !strings.Contains(closeBody, "mvViewerFromPop") {
		t.Error("closeViewer() 必须按 mvViewerFromPop 分流 —— " +
			"返回触发时历史**已经退了**，此时再补一次 back 就是一次返回退两层")
	}
	if !strings.Contains(closeBody, "history.state") || !strings.Contains(closeBody, "mvViewer") {
		t.Error("closeViewer() 必须先确认栈顶确实是我们的查看器条目（history.state.mvViewer），再退历史")
	}
	// 更严的一条：**返回路径自身不许碰历史**（与 2.1.3 的实测教训一致 ——
	//   那里主动退一次 + 宿主退一次 = 一次滑动退两层）。
	for _, fn := range []string{
		"function mvOnPop(", "function mvGuardArm(", "function mvGuardInit(", "function goUpLevel(",
	} {
		if strings.Contains(jsBody(fn), "history.back(") {
			t.Errorf("%s 里不得出现 history.back( —— 那条路径上历史已经退了，"+
				"再退一次会和宿主那一次叠加：一次返回退两层", fn)
		}
	}
	if strings.Contains(app, "history.replaceState(") {
		t.Error("app.js 里出现了 history.replaceState( —— 守卫机制只需要 pushState，别引入第二种写法")
	}
	if n := strings.Count(app, "'popstate'"); n != 1 {
		t.Errorf("popstate 应恰好注册 1 处，实际 %d 处 —— 重复注册会让一次返回被处理多次", n)
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

// Test2210GuardStopsHostBack 用 node 真跑守卫机制（mvGuardArm / mvOnPop / mvGuardInit）：
//
//	① 启动压**恰好一条**守卫、并接上 popstate；
//	② 宿主发起的返回：补守卫、**永不主动退历史**、网格里什么都不做；
//	③ 连来 5 次也不掉出去（每次都被补回）；
//	④ 唯一例外：查看器开着时把它关掉（否则大图成死胡同）。
func Test2210GuardStopsHostBack(t *testing.T) {
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
function ok(c, msg) { if (!c) { console.error('FAIL ' + msg); process.exit(1); } }

// --- 假环境：真实记录 pushState 的内容、主动退历史的次数、被关查看器的次数 ---
var pushed = [], backs = 0, closed = 0, listeners = {};
var history = {
  pushState: function (st) { pushed.push(st || {}); },
  back: function () { backs++; },
  replaceState: function () { console.error('FAIL 守卫机制不该用 replaceState'); process.exit(1); }
};
var window = { addEventListener: function (n, f) { listeners[n] = f; } };
var viewer = {
  hidden: true,
  classList: { contains: function (c) { return c === 'hidden' ? viewer.hidden : false; } }
};
var up = 0, exits = 0;
function closeViewer() { closed++; }
function goUpLevel() { up++; }
// 2.4.2：根层 popstate 也不再自绘 —— "是否退出"统一交给宿主原生弹窗
function askExitApp() { exits++; }
var state = { root: false, path: '/vol2/1000/图库' };
// 2.3.2：mvOnPop 里的"自我回退"开关（closeViewer 主动退历史时置位）
var mvSelfBack = false;

var code =
  grab('mvGuardArm') + ';' + grab('mvOnPop') + ';' + grab('mvGuardInit') + ';' +
  'return { init: mvGuardInit, pop: mvOnPop,' +
  '  snap: function () { return { pushed: pushed, backs: backs, closed: closed, up: up,' +
  '    exits: exits, popListener: !!listeners.popstate }; } };';
var G = new Function('PROFILE', 'viewer', 'closeViewer', 'history', 'window', 'state', 'goUpLevel',
                     'mvSelfBack', 'askExitApp', code)(
  'touch', viewer, closeViewer, history, window, state, goUpLevel, mvSelfBack, askExitApp);

// ① 启动接线
G.init();
var s0 = G.snap();
eq(s0.pushed.length, 1, '★ 启动必须压**恰好一条**守卫条目 —— 多了会让历史膨胀，少了就没有落点');
eq(s0.pushed[0].mvGuard, 1, '守卫条目要带 mvGuard 标记（否则没法用 history.state 核查它在不在）');
eq(s0.popListener, true, '★ 必须接上 popstate —— 否则宿主发起的返回没人补守卫，第二次就退到应用外');

// ②③ 网格（子目录）里连来 5 次宿主返回 —— 2.3.0 起每次都应"回上一层目录"
viewer.hidden = true;
state.root = false; state.path = '/vol2/1000/图库';
for (var i = 0; i < 5; i++) { listeners.popstate(); }
var s1 = G.snap();
eq(s1.pushed.length, 6, '★ 每一次返回都要把守卫补回去（1 条启动 + 5 次补 = 6）');
eq(s1.backs, 0, '★★ 我们**永不主动 history.back()** —— 主动退一次会和宿主那次叠加，一次滑动退两层');
eq(s1.closed, 0, '★ 网格里返回不该关查看器（那时它是关着的）');
eq(s1.up, 5, '★ 2.3.0：子目录里每次返回都要**回上一层目录** —— ' +
  '与"查看器 → 关查看器"统一成同一个出口（2.2.x 这里曾是"什么都不做"，导致网格返回完全失效）');

// ③b 已在最上层（聚合根）：2.4.1 起**弹"是否退出"确认**（用户实测：宿主闸门不弹 ⇒ 页面自己弹）
state.root = true; state.path = '';
listeners.popstate();
var s1b = G.snap();
eq(s1b.up, 5, '★★ 已在最上层时返回不许再回上级');
eq(s1b.exits, 0, '★ 已在聚合根层返回**不再自绘确认框** —— 交给宿主原生弹窗（syncExitTips）');
eq(s1b.closed, 0, '★ 也不许关查看器（它没开）');
eq(s1b.backs, 0, '★★ 更不许主动退历史');
eq(s1b.pushed.length, 7, '补守卫照旧（守卫与"退到哪一层"是两件事，互不替代）');
state.root = false; state.path = '/vol2/1000/图库';

// ④ 唯一例外：查看器开着
viewer.hidden = false;
listeners.popstate();
var s2 = G.snap();
eq(s2.closed, 1, '★ 查看器开着时返回要把它关掉 —— 否则大图里没有任何出口（← 2.1.1 已删、手势 2.2.0 已删）');
eq(s2.pushed.length, 8, '关查看器那一次也要补守卫（关查看器与补守卫是两件事，互不替代）');
eq(s2.backs, 0, '关查看器也不许走 history.back()');

ok(s2.pushed.every(function (p) { return p.mvGuard === 1; }), '压进历史的条目必须全部是守卫标记');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("守卫机制探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
