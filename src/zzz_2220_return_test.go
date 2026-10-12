package main

// zzz_2220_return_test.go —— 2.2.2「大图返回退到网格：触屏档强制遮罩模式 + 单图窗口不压守卫」
//
// 用户报的原话：
//   「大图浏览应该退到网格，可现在点击进去退出再点击大图退出就会退出软件」
//
// 定位过程（都有实跑依据，不是推断）：
//   ① 真浏览器探针 **S1（遮罩模式）连做 3 轮「点开大图 → 返回」全绿** ——
//      文档存活、每次都退回网格、守卫每次被补回。⇒ 浏览器层没问题，问题在"大图走的是哪条路"。
//   ② `viewerMode` 的**默认值是 'window'** —— 也就是请宿主另开一个独立看图窗口
//      （那份文档 URL 带 ?path=，`singleMode=true`）。**它的历史栈是它自己的**，
//      主窗口压的守卫管不到；它"返回"时怎么收尾完全由宿主决定，我们拦不住。
//
// 两条改动：
//   · 触屏档 `openViewer()` **强制走遮罩模式**（不看设置）⇒ 大图叠在网格上，
//     返回 = 我们的 popstate → 关查看器 → 退回网格，全程可控。桌面仍尊重设置。
//   · 单图 / 独立看图窗口（?path=）**不压守卫** ⇒ 宿主 canGoBack 为 false 时直接 finish
//     那个窗口（返回 = 关掉它，这才是期望语义）。

import (
	"os/exec"
	"strings"
	"testing"
)

// Test2220TouchForcesOverlayViewer 用 node 真跑 openViewer：
//
//	触屏档**无论设置成什么**都必须走遮罩模式；桌面档仍按设置走。
func Test2220TouchForcesOverlayViewer(t *testing.T) {
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
var code = grab('openViewer') + '; return { open: openViewer };';
function mk(profile, viewerMode) {
  var calls = [];
  var env = new Function('PROFILE', 'cfg', 'openViewerOverlay', 'openViewerWindow', code)(
    profile, { viewerMode: viewerMode },
    function () { calls.push('overlay'); },
    function () { calls.push('window'); });
  env.calls = calls;
  return env;
}

// ★ 核心：触屏档即使设置成独立窗口，也必须走遮罩
var A = mk('touch', 'window');
A.open({ path: '/vol1/1.jpg' });
eq(A.calls.join(','), 'overlay',
   '★★ 触屏档必须强制走遮罩模式 —— 独立窗口的返回语义由宿主决定，我们拦不住（用户报的"返回却退出软件"）');

var B = mk('touch', 'overlay');
B.open({ path: '/vol1/1.jpg' });
eq(B.calls.join(','), 'overlay', '触屏档 + 遮罩设置：仍是遮罩');

// 桌面档：尊重设置（多窗口在桌面上有意义，不该被手机的问题牵连）
var C = mk('mouse', 'window');
C.open({ path: '/vol1/1.jpg' });
eq(C.calls.join(','), 'window', '★ 桌面档必须仍尊重设置（不能连桌面一起改掉）');

var D = mk('mouse', 'overlay');
D.open({ path: '/vol1/1.jpg' });
eq(D.calls.join(','), 'overlay', '桌面档 + 遮罩设置：走遮罩');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("触屏强制遮罩探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2220SingleModeJudgeHasSingleSource 静态钉住两件事：
//
//	① 单图模式的判据**只有一处**（顶层按 URL 定）—— 两处写迟早分叉；
//	② 它的定义必须**早于**守卫接线 —— 否则接线时读到 undefined，
//	   单图窗口照样会把守卫压上（纯看条件表达式看不出来的时序缺陷）。
func Test2220SingleModeJudgeHasSingleSource(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	if n := strings.Count(app, "var singleMode = false;"); n != 1 {
		t.Errorf("`var singleMode = false;` 应恰好 1 处（顶层），实际 %d 处 —— "+
			"守卫压不压完全取决于它，判据分两处写迟早会分叉", n)
	}
	if strings.Contains(app, "singleMode = true;") {
		t.Error("还残留 `singleMode = true;` —— 单图模式的判据必须只有一处（顶层按 URL 定），" +
			"boot() 里不该再赋值")
	}

	wiring := "if (PROFILE === 'touch' && !singleMode) mvGuardInit();"
	iDef := strings.Index(app, "var singleMode = false;")
	iWire := strings.Index(app, wiring)
	if iDef < 0 || iWire < 0 {
		t.Fatalf("找不到判据定义或守卫接线（iDef=%d iWire=%d）", iDef, iWire)
	}
	if iDef > iWire {
		t.Error("★ `singleMode` 的定义必须**早于**守卫接线 —— 否则接线时读到的是 undefined，" +
			"单图窗口照样会压上守卫（时序缺陷，光看条件表达式看不出来）")
	}
}
