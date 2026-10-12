// 2.3.0 新增：三份区域点击、以双击点为中心、查看器历史条目。
//
// 这些都是"光看静态文本看不出来"的行为，所以用 node 真跑源码里抠出来的处理器：
//
//	· 三份判定：中间点击**不得**切图（这是"双击不误切、且不必延迟 300ms"的前提）
//	· 双击：以双击点为中心（v.tx/v.ty = 双击点相对图片中心的偏移取负）
//	· 查看器：触屏档压一条 mvViewer 历史；桌面档**不许**压
//
// ⚠️ 为什么要单独钉"中间不切图"：双击的两下 click 都落在中间，
//
//	一旦中间也切图，就会退化成"先切一张/两张、再放大"（用户实测报的现象）。
//	而把中间留空，才可能做到**零延迟**地区分单击与双击。
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ① 三份区域：中间不切图、左右各切一张
// ---------------------------------------------------------------------------
func Test2230StageClickIsThreeRegions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');

// 抠出 vStage 的 click 处理器（匿名回调，靠锚点 + 大括号配平）
function grabAnon(anchor) {
  var i = appjs.indexOf(anchor);
  if (i < 0) { console.error('FAIL 找不到锚点: ' + anchor); process.exit(1); }
  var j = appjs.indexOf('{', i + anchor.length);
  var depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(j + 1, k); }
  }
  console.error('FAIL 大括号不配平'); process.exit(1);
}
var body = grabAnon("$('vStage').addEventListener('click', function (e) ");
// 2.3.3：切图延迟执行 ⇒ 需要可控定时器
var __timers = [];
function flushTimers() {
  var ts = __timers.slice();
  __timers.length = 0;
  for (var i = 0; i < ts.length; i++) ts[i]();
}

function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// 2.3.3：切图改为延迟执行 ⇒ 探针必须提供可控的定时器。
var __timers = [];
function flushTimers() {
  var ts = __timers.slice();
  __timers.length = 0;
  for (var i = 0; i < ts.length; i++) ts[i]();
}
function makeCtx(profile) {
  var __p = profile || 'touch';
  return {
    calls: [],
    PROFILE_OVERRIDE: __p,
    setTimeout: function (fn) { __timers.push(fn); return __timers.length; },
    clearTimeout: function (id) { if (id > 0) __timers[id - 1] = function () {}; },
    tgesture: { muteClickUntil: 0 },
    swipe: { suppressClick: false, dx: 0, dy: 0 },
    VIEWER_CTL_SEL: 'button, a, input',
    SWIPE_LOCK_SCALE: 2,
    // 默认触屏档；桌面档用 makeCtx('mouse') —— 下面这行在 makeCtx 里被覆盖
    PROFILE: 'touch',
    v: { scale: 1, index: 1, list: [1, 2, 3] },
    mvTap: { undo: 0, at: 0, timer: null },
    MV_DBL_TAP_MS: 300, MV_TAP_DELAY_MS: 220,
    showCurrent: function () { this.calls.push('showCurrent'); },
    flashCounter: function () { this.calls.push('flashCounter'); },
    resetView: function () { this.calls.push('reset'); },
    applyTransform: function () {},
    // 与真实的 swipeTo 一样做**边界钳位**（真实代码里 v.index 会被夹在 [0, len-1]）
    prev: function () { this.calls.push('prev'); if (this.v.index > 0) this.v.index--; },
    next: function () { this.calls.push('next'); if (this.v.index < this.v.list.length - 1) this.v.index++; }
  };
}
// 用 with(this) 让处理器里的自由变量落到 ctx 上
// autoFlush 默认 true：切图是**延迟**执行的（2.3.3 为双击零闪烁而引入），
//   普通断言要把定时器跑掉才看得到效果；双击用例传 false（第二下要能取消它）。
function run(ctx, x, width, tagName, detail, autoFlush) {
  ctx.calls = [];
  if (ctx.PROFILE_OVERRIDE) ctx.PROFILE = ctx.PROFILE_OVERRIDE;   // 让用例能指定触屏/桌面
  // 处理器里的 this 就是"被点击的 vStage 元素"，所以 getBoundingClientRect 要挂在 ctx 上
  ctx.getBoundingClientRect = function () { return { left: 0, top: 0, width: width, height: 300 }; };
  var fn = new Function('e', 'with (this) { ' + body + ' }');
  fn.call(ctx, {
    clientX: x, clientY: 150, detail: detail || 1,
    target: {
      tagName: tagName || 'IMG',
      closest: function (sel) { return null; }
    }
  });
  if (autoFlush !== false) flushTimers();
  return ctx;
}
var W = 300;   // 300 宽 → 三份边界在 100 / 200

// 左 1/3 → 上一张
var c1 = run(makeCtx(), 40, W);
eq(c1.calls.join(','), 'prev', '左 1/3 点击应切上一张');

// 右 1/3 → 下一张
var c2 = run(makeCtx(), 260, W);
eq(c2.calls.join(','), 'next', '右 1/3 点击应切下一张');

// **中间 1/3 → 什么都不做**（这条是双击能零延迟的前提）
var c3 = run(makeCtx(), 150, W);
eq(c3.calls.length, 0, '★ 中间 1/3 点击**不得切图** —— 双击的两下 click 都落在这里，' +
  '中间若也切图就会"先切一张再放大"；留空才能零延迟区分单击与双击');

// 边界：刚过 1/3 就该算中间（不许把 100 算进左区）
var c4 = run(makeCtx(), 101, W);
eq(c4.calls.length, 0, '刚越过 1/3 应落入中间区（不切图）');
// 边界：刚好到 2/3 仍算中间
var c5 = run(makeCtx(), 199, W);
eq(c5.calls.length, 0, '未到 2/3 应仍在中间区（不切图）');
var c6 = run(makeCtx(), 201, W);
eq(c6.calls.join(','), 'next', '越过 2/3 应切下一张');

// 视频也必须用同一套三份（不再有自己的 25% / 75%）
var c7 = run(makeCtx(), 40, W, 'VIDEO');
eq(c7.calls.join(','), 'prev', '视频左 1/3 也应切上一张（与图片统一）');
var c8 = run(makeCtx(), 150, W, 'VIDEO');
eq(c8.calls.length, 0, '视频中间 1/3 也不切图');

// 2.4.1：**手机版放大后不响应左右切图**（用户要求："放大后应该不响应左右切图，只能拖动查看"）。
//   所以这里既不归位、也不切图。
var c9 = makeCtx(); c9.v.scale = 3;
run(c9, 40, W);
eq(c9.calls.indexOf('reset'), -1, '★ 放大后点击不该归位');
eq(c9.calls.length, 0, '★★ 手机版放大后点左右**什么都不做**（用户要求：只能拖动查看）');
// 桌面版不受影响：鼠标精确，放大后点击仍直接切图
var c9m = makeCtx('mouse'); c9m.v.scale = 3;
run(c9m, 40, W);
eq(c9m.calls.join(','), 'prev', '桌面版放大后点左 1/3 仍应直接切上一张');

// 拖动过（>8px）不进任何点击逻辑 —— 这是"拖动只平移、不误切"的保险
var c10 = makeCtx(); c10.swipe.dx = 20;
run(c10, 260, W);
eq(c10.calls.length, 0, '拖动过 8px 的 click 必须被距离守卫拦掉（否则拖动松手会误切图）');

// 2.3.3：切图改为**延迟判定**，双击的第二下直接取消 ⇒ **全程没有切过图**（零闪烁）。
//   这是用户要的效果：不再有"切过去→瞬间切回→放大"。
var c11 = makeCtx();
run(c11, 40, W, 'IMG', 1, false);   // 第一下：只是排入延迟队列，**还没切**
eq(c11.calls.indexOf('prev'), -1, '★ 第一下点击不该立即切图（要等判定窗，否则双击会闪）');
run(c11, 40, W, 'IMG', 2, false);   // 第二下：取消队列
flushTimers();
eq(c11.calls.indexOf('prev'), -1, '★★ 双击的第二下必须取消待执行的切图 —— 全程一次都没切过');
eq(c11.v.index, 1, '★★ 双击后索引不变（不动图，随后由 dblclick 放大）');
// 对照：只点一下（不跟第二下），判定窗过去后应正常切图
var c12 = makeCtx();
run(c12, 40, W, 'IMG', 1);
eq(c12.calls.join(','), 'prev', '单击（detail=1）在判定窗后仍应正常切图 —— 延迟不能让单击失效');

// ★ 2.3.4：**桌面档不延迟** —— 鼠标单击切图必须立即生效（220ms 对鼠标是能感知的延迟）。
var c15 = makeCtx('mouse');
run(c15, 40, W, 'IMG', 1);                 // run 默认 flush，但桌面档根本不该有定时器
eq(c15.calls.join(','), 'prev', '桌面档单击应**立即**切图（不能套用手机版的判定窗）');
var c16 = makeCtx('mouse');
run(c16, 260, W, 'IMG', 1);
eq(c16.calls.join(','), 'next', '桌面档右 1/3 单击也应立即切下一张');
// 桌面档双击：两下都是"立即切图"（不做取消），这正是鼠标的既有语义
var c17 = makeCtx('mouse');
run(c17, 40, W, 'IMG', 1, false);
eq(c17.calls.join(','), 'prev', '桌面档第一下就是立即切图（没有待取消的队列）');
eq(c17.mvTap.timer, null, '桌面档不该创建判定窗定时器');

// ★ 关键回归（2.3.3 延迟方案）：**边界上双击后索引必须不变**。
//   在第一张点左 1/3 时 prev() 被钳位 —— 加上双击取消 ⇒ 索引绝不能被推到下一张。
var c13 = makeCtx();
c13.v.index = 0;                       // 已在第一张
run(c13, 40, W, 'IMG', 1, false);      // 第一下（排入延迟队列）
run(c13, 40, W, 'IMG', 2, false);      // 第二下（取消队列）
flushTimers();
eq(c13.v.index, 0, '★★ 边界上双击后索引不得改变（不许因为边界钳位而前进到下一张）');
eq(c13.calls.length, 0, '★★ 边界上双击全程不该有任何切图动作');
// 对照：从中间（index=2）单击左 1/3 ⇒ 延迟窗后应退到前一张
var c14 = makeCtx();
c14.v.index = 2;
run(c14, 40, W, 'IMG', 1);
eq(c14.v.index, 1, '从中间点左 1/3 应在判定窗后退到前一张');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("三份区域探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// ---------------------------------------------------------------------------
// ② 双击：以双击点为中心
// ---------------------------------------------------------------------------
func Test2230DblclickUsesDoubleClickPointAsCenter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');

function grabAnon(anchor) {
  var i = appjs.indexOf(anchor);
  if (i < 0) { console.error('FAIL 找不到锚点: ' + anchor); process.exit(1); }
  var j = appjs.indexOf('{', i + anchor.length);
  var depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(j + 1, k); }
  }
  console.error('FAIL 大括号不配平'); process.exit(1);
}
var body = grabAnon("img.addEventListener('dblclick', function (e) ");

function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

function makeCtx(el) {
  return {
    v: { scale: 1, zoomed: false, actual: false, tx: 0, ty: 0 },
    up: 0, applied: 0,
    resetView: function () { this.v.scale = 1; this.v.zoomed = false; this.v.tx = 0; this.v.ty = 0; },
    updateFitBtn: function () {},
    applyTransform: function () { this.applied++; },
    maybeUpgradeToFull: function () {},
    el: el
  };
}
function dbl(ctx, clientX, clientY) {
  // 同上：处理器里的 this 是"被双击的 img 元素"
  ctx.getBoundingClientRect = ctx.el.getBoundingClientRect;
  var fn = new Function('e', 'with (this) { ' + body + ' }');
  fn.call(ctx, { clientX: clientX, clientY: clientY });
  return ctx;
}
// 图片 400x300，中心在 (200,150)（getBoundingClientRect 的 left/top 为 0）
var el = { getBoundingClientRect: function () { return { left: 0, top: 0, width: 400, height: 300 }; } };

// 双击在正中心 → 不平移
var c1 = dbl(makeCtx(el), 200, 150);
eq(c1.v.scale, 3, '双击应放大到 3 倍（2.4.0 按用户要求）');
eq(c1.v.tx, 0, '双击正中心时不平移');
eq(c1.v.ty, 0, '双击正中心时不平移');

// 双击在中心右侧 60px、下方 40px → 平移量应为 (-60, -40)
var c2 = dbl(makeCtx(el), 260, 190);
eq(c2.v.scale, 3, '双击应放大到 3 倍（2.4.0 按用户要求）');
eq(c2.v.tx, -60, '★ 以双击点为中心：双击点相对图片中心的偏移取负作为平移量（x）');
eq(c2.v.ty, -40, '★ 以双击点为中心：双击点相对图片中心的偏移取负作为平移量（y）');
ok(c2.v.zoomed === true, '双击后应标记为已放大');
ok(c2.applied > 0, '双击后必须应用 transform');

// 已放大时双击 → 还原（且要应用 transform，否则界面不动）
var c3 = makeCtx(el);
c3.v.scale = 3; c3.v.zoomed = true; c3.v.tx = -60; c3.v.ty = -40;
dbl(c3, 260, 190);
eq(c3.v.scale, 1, '已放大时双击应还原到 1 倍');
eq(c3.v.tx, 0, '还原时平移量应清零');
ok(c3.applied > 0, '还原也必须应用 transform');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("双击探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// ---------------------------------------------------------------------------
// ③ 查看器历史条目：触屏档压、桌面档不压
// ---------------------------------------------------------------------------
func Test2230ViewerPushesHistoryOnTouchOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = appjs.indexOf('{', i), depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }

var code =
  grab('openViewerOverlay') + ';' +
  'return { open: openViewerOverlay, snap: function () { return pushed.slice(); } };';

function makeEnv(profile) {
  var env = {
    pushed: [],
    history: {
      pushState: function (st, title) { env.pushed.push({ state: st, title: title }); },
      back: function () {}
    }
  };
  var v = { list: [], index: 0 };
  var viewer = { classList: { remove: function () {}, add: function () {}, contains: function () { return false; } } };
  var state = { items: [{ kind: 'file', path: '/vol1/a.jpg', name: 'a.jpg' }] };
  var vWrap = { querySelector: function () { return null; }, appendChild: function () {} };
  var cfg = {};
  var api = new Function(
    'PROFILE', 'history', 'v', 'viewer', 'state', 'vWrap', 'cfg',
    'stopAllMediaIn', 'applyViewerAnimation', 'suspendGridRequests', 'showCurrent', 'pauseThumbGen',
    code
  )(
    profile, env.history, v, viewer, state, vWrap, cfg,
    function () {}, function () {}, function () {}, function () {}, function () {}
  );
  env.api = api;
  return env;
}

// 触屏档：必须压**一条**带 mvViewer 标记的条目
var e1 = makeEnv('touch');
e1.api.open({ kind: 'file', path: '/vol1/a.jpg', name: 'a.jpg' });
eq(e1.pushed.length, 1, '★ 触屏档打开查看器必须压**恰好一条**历史 —— ' +
  '查看器没有自己的历史层级时，返回会退掉上一层目录页（真导航重载），closeViewer 根本没机会跑');
eq(e1.pushed[0].state && e1.pushed[0].state.mvViewer, 1, '查看器条目必须带 mvViewer 标记（供 closeViewer 识别）');

// 桌面档：**不许**压（桌面查看器是独立窗口，语义不同）
var e2 = makeEnv('mouse');
e2.api.open({ kind: 'file', path: '/vol1/a.jpg', name: 'a.jpg' });
eq(e2.pushed.length, 0, '桌面档不得压查看器历史（桌面用独立窗口，不该被手机的问题牵连）');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("查看器历史探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// ---------------------------------------------------------------------------
// ④ P1：prev 也必须预加载（原来 prevBox 恒为空占位 ⇒ 向左回看第一下必白屏）
// ---------------------------------------------------------------------------
func Test2230PrevIsPreloadedLikeNext(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))
	i := strings.Index(app, "function rebuildTrackInPlace(")
	if i < 0 {
		t.Fatal("找不到 rebuildTrackInPlace()")
	}
	j := strings.Index(app[i:], "{")
	if j < 0 {
		t.Fatal("rebuildTrackInPlace 没有函数体")
	}
	j += i
	depth := 0
	body := ""
	for k := j; k < len(app); k++ {
		if app[k] == '{' {
			depth++
		} else if app[k] == '}' {
			depth--
			if depth == 0 {
				body = app[i : k+1]
				break
			}
		}
	}
	if body == "" {
		t.Fatal("rebuildTrackInPlace 括号不配平")
	}
	// prev 与 next 必须**对称**：都存在"存在上一张就渲染它"的分支。
	if !strings.Contains(body, "v.list[v.index - 1]") {
		t.Error("★ prev 也必须预加载（v.list[v.index - 1]）—— " +
			"prevBox 恒为空占位时，向左回看第一下必然白屏等待，而向右切因为有 next 是顺的：" +
			"左右手感不对称，用户能直接感觉到（2.3.0 P1）")
	}
	if !strings.Contains(body, "v.list[v.index + 1]") {
		t.Error("next 的预加载分支不该被误删")
	}
	if n := strings.Count(body, "createMediaBox("); n < 2 {
		t.Errorf("prev 与 next 都要用 createMediaBox 渲染（当前 %d 处）", n)
	}
}

// ---------------------------------------------------------------------------
// ⑤ endSwipe 的"抬指切图"也必须是三份（2.3.1 修的真正源头）
//
// 背景：双击的两下 pointerup 都会走 endSwipe 的 tap 分支。
//
//	它原来用**左右半区**判定，于是双击 = 切两张 + dblclick 放大，
//	而且它设了 suppressClick，抢先把 vStage click 里那道三份判定整个抑制掉 ——
//	所以只改 click 是没用的（2.3.0 就是这么漏的）。
//
// ---------------------------------------------------------------------------
func Test2310EndSwipeTapIsThreeRegions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = appjs.indexOf('{', i), depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
var src = grab('endSwipe');
// ⚠️ 必须剥掉注释再断言 —— 这个函数的注释里就写着 "它随后设了 suppressClick = true"，
//    不剥注释会把说明文字当成代码（第一版就栽在这里）。
src = src.replace(/\/\*[\s\S]*?\*\//g, '').split('\n').map(function (l) {
  var i = l.indexOf('//');
  return i >= 0 ? l.slice(0, i) : l;
}).join('\n');

// 2.3.3：endSwipe **不得再碰索引** —— 切图统一由 vStage 的 click 负责（带判定窗）。
//   原因：它在 pointerup 就切图，导致"边缘双击"必然出现"切过去→瞬间切回→放大"的闪烁。
ok(src.indexOf('isTap') === -1, 'endSwipe 里不该再有 isTap（点击切图已移出）');
ok(src.indexOf('prev()') === -1, 'endSwipe 不该再调 prev()');
ok(src.indexOf('next()') === -1, 'endSwipe 不该再调 next()');
ok(src.indexOf('mvTap.undo') === -1, 'endSwipe 不该再碰撤销标记');
ok(src.indexOf('suppressClick = true') === -1, 'endSwipe 不该再设 suppressClick（那会吞掉 click）');

// 仍然要能用真实三份判定那套（现在只在 click 里）——这里只保证 endSwipe 干净。
console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("endSwipe 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

func Test2310ZoomedMiddleClickDoesNotReset(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
function grabAnon(anchor) {
  var i = appjs.indexOf(anchor);
  if (i < 0) { console.error('FAIL 找不到锚点'); process.exit(1); }
  var j = appjs.indexOf('{', i + anchor.length);
  var depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(j + 1, k); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
var body = grabAnon("$('vStage').addEventListener('click', function (e) ");
// 2.3.3：切图延迟执行 ⇒ 这段探针也需要可控定时器
var __timers = [];
function flushTimers() {
  var ts = __timers.slice();
  __timers.length = 0;
  for (var i = 0; i < ts.length; i++) ts[i]();
}
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function run(x, width, scale, profile) {
  var calls = [];
  var __p = profile || 'touch';
  var ctx = {
    calls: calls,
    tgesture: { muteClickUntil: 0 },
    swipe: { suppressClick: false, dx: 0, dy: 0 },
    VIEWER_CTL_SEL: 'button, a, input',
    SWIPE_LOCK_SCALE: 2,
    // 默认触屏档；桌面档用 makeCtx('mouse') —— 下面这行在 makeCtx 里被覆盖
    PROFILE: 'touch',
    v: { scale: scale },
    mvTap: { undo: 0, at: 0, timer: null },
    MV_DBL_TAP_MS: 300, MV_TAP_DELAY_MS: 220,
    // 必须提供可控定时器：否则会落到 Node 全局的 setTimeout（真异步），
    // flushTimers 抓不到，断言就会看到"没切图"。
    setTimeout: function (fn) { __timers.push(fn); return __timers.length; },
    clearTimeout: function (id) { if (id > 0) __timers[id - 1] = function () {}; },
    showCurrent: function () { calls.push('showCurrent'); },
    resetView: function () { calls.push('reset'); },
    applyTransform: function () {},
    prev: function () { calls.push('prev'); },
    next: function () { calls.push('next'); },
    getBoundingClientRect: function () { return { left: 0, top: 0, width: width, height: 300 }; }
  };
  var fn = new Function('e', 'with (this) { ' + body + ' }');
  fn.call(ctx, { clientX: x, clientY: 150, detail: 1,
                 target: { tagName: 'IMG', closest: function () { return null; } } });
  flushTimers();   // 2.3.3：切图延迟执行，断言前要跑掉定时器
  return calls;
}
var W = 300;
// 放大到 3 倍后点中间 → **什么都不做**（不归位、不切图）
eq(run(150, W, 3).length, 0, '★ 放大后点中间不得归位、也不切图');
// 2.4.1：**手机版放大后左右都不响应切图**（只能拖动查看）
var zl = run(40, W, 3);
eq(zl.length, 0, '★★ 手机版放大后点左 1/3 应**什么都不做**（不归位、不切图）');
var zr = run(260, W, 3);
eq(zr.length, 0, '★★ 手机版放大后点右 1/3 应**什么都不做**');
// 未放大时点左/右 → 同样切图
var ul = run(40, W, 1);
eq(ul.join(','), 'prev', '未放大点左 1/3 应切上一张');
var ur = run(260, W, 1);
eq(ur.join(','), 'next', '未放大点右 1/3 应切下一张');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("放大后中间点击探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
