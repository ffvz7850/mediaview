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
// 2.4.13：分区收敛到 stageZoneX()，探针要把**真函数**一起喂进去（不能自己再实现一份，
// 否则 stageZoneX 写错了测试照样绿）。
// 抠出**具名函数**的完整源码（含签名与函数体），用来把真身喂进探针
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
var zoneFn = grab('stageZoneX');

function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// 2.4.13：click 里**不该有任何定时器**。这里仍然提供一个记录型 setTimeout ——
//   万一谁把判定窗加回来，计数器会立刻暴露它。
function makeCtx(profile) {
  var ctx = {
    timers: [],
    calls: [],
    PROFILE: profile || 'touch',
    setTimeout: function (fn, ms) { ctx.timers.push(ms); return ctx.timers.length; },
    clearTimeout: function () {},
    tgesture: { muteClickUntil: 0 },
    swipe: { suppressClick: false, dx: 0, dy: 0 },
    VIEWER_CTL_SEL: 'button, a, input',
    SWIPE_LOCK_SCALE: 2,
    v: { scale: 1, index: 2, list: [1, 2, 3, 4, 5] },
    flashCounter: function () {},
    resetView: function () { ctx.calls.push('reset'); },
    applyTransform: function () {},
    prev: function () { ctx.calls.push('prev'); if (ctx.v.index > 0) ctx.v.index--; },
    next: function () { ctx.calls.push('next'); if (ctx.v.index < ctx.v.list.length - 1) ctx.v.index++; }
  };
  // stageZoneX 只会问 vStage；问别的说明代码跑偏了
  ctx.$ = function (id) {
    if (id !== 'vStage') throw new Error('意外的 $(' + id + ')');
    return { getBoundingClientRect: function () { return ctx._rect; } };
  };
  return ctx;
}
// stageZoneX 的定义放在 with 之外的函数作用域里，$ 用 var 注入 ——
// 这样它就是**源码里那一份**，而不是测试自己仿一份。
function run(ctx, x, width, tagName, detail) {
  // 注意：**不清 calls** —— 连点类用例要跨两次 run 累积（双击语义就靠它验证）。
  //   每次 makeCtx 都是全新对象，所以单次用例不受影响。
  ctx.timers = [];
  ctx._rect = { left: 0, top: 0, width: width || 300, height: 300 };
  var fn = new Function('e', 'var $ = this.$; ' + zoneFn + '; with (this) { ' + body + ' }');
  fn.call(ctx, {
    clientX: x, clientY: 150, detail: detail || 1,
    target: {
      tagName: tagName || 'IMG',
      closest: function (sel) { return null; }
    }
  });
  return ctx;
}
var W = 300;   // 300 宽 → 三份边界在 100 / 200

// ★★ 2.4.13 核心：触屏档也必须**当帧切图**，且一个定时器都不许有。
var t1 = run(makeCtx(), 40, W);
eq(t1.calls.join(','), 'prev', '★ 触屏档左 1/3 必须**立即**切上一张（零延迟，不允许再等判定窗）');
eq(t1.timers.length, 0, '★★ 触屏档切图**不得创建任何定时器** —— 出现定时器就说明判定窗被加回来了');
var t2 = run(makeCtx(), 260, W);
eq(t2.calls.join(','), 'next', '★ 触屏档右 1/3 必须立即切下一张');
eq(t2.timers.length, 0, '★★ 触屏档右 1/3 也不得创建定时器');

// 桌面档与触屏档**完全一致**（2.4.13 起两条路合并）
var d1 = run(makeCtx('mouse'), 40, W);
eq(d1.calls.join(','), 'prev', '桌面档左 1/3 立即切上一张');
eq(d1.timers.length, 0, '桌面档不得创建定时器');
var d2 = run(makeCtx('mouse'), 260, W);
eq(d2.calls.join(','), 'next', '桌面档右 1/3 立即切下一张');

// **中间 1/3 → 什么都不做**（这条是"双击不闪且零延迟"的前提，两个档位都一样）
var t3 = run(makeCtx(), 150, W);
eq(t3.calls.length, 0, '★ 中间 1/3 点击**不得切图** —— 双击的两下 click 都落在这里，' +
  '中间若也切图就会"先切一张再放大"；留空才能零延迟区分单击与双击');
var t4 = run(makeCtx('mouse'), 150, W);
eq(t4.calls.length, 0, '桌面档中间 1/3 也不切图');

// 边界：刚过 1/3 就该算中间（不许把 100 算进左区）
var t5 = run(makeCtx(), 101, W);
eq(t5.calls.length, 0, '刚越过 1/3 应落入中间区（不切图）');
// 边界：刚好到 2/3 仍算中间
var t6 = run(makeCtx(), 199, W);
eq(t6.calls.length, 0, '未到 2/3 应仍在中间区（不切图）');
var t7 = run(makeCtx(), 201, W);
eq(t7.calls.join(','), 'next', '越过 2/3 应切下一张');

// 视频也必须用同一套三份（不再有自己的 25% / 75%）
var t8 = run(makeCtx(), 40, W, 'VIDEO');
eq(t8.calls.join(','), 'prev', '视频左 1/3 也应切上一张（与图片统一）');
var t9 = run(makeCtx(), 150, W, 'VIDEO');
eq(t9.calls.length, 0, '视频中间 1/3 也不切图');

// 2.4.1：**手机版放大后不响应左右切图**（用户要求："放大后应该不响应左右切图，只能拖动查看"）。
//   所以这里既不归位、也不切图。
var t10 = makeCtx(); t10.v.scale = 3;
run(t10, 40, W);
eq(t10.calls.indexOf('reset'), -1, '★ 放大后点击不该归位');
eq(t10.calls.length, 0, '★★ 手机版放大后点左右**什么都不做**（用户要求：只能拖动查看）');
// 桌面版不受影响：鼠标精确，放大后点击仍直接切图
var t10m = makeCtx('mouse'); t10m.v.scale = 3;
run(t10m, 40, W);
eq(t10m.calls.join(','), 'prev', '桌面版放大后点左 1/3 仍应直接切上一张');

// 拖动过（>8px）不进任何点击逻辑 —— 这是"拖动只平移、不误切"的保险
var t11 = makeCtx(); t11.swipe.dx = 20;
run(t11, 260, W);
eq(t11.calls.length, 0, '拖动过 8px 的 click 必须被距离守卫拦掉（否则拖动松手会误切图）');

// suppressClick 必须被消费掉，且这一次不切图
var t12 = makeCtx(); t12.swipe.suppressClick = true;
run(t12, 260, W);
eq(t12.calls.length, 0, 'suppressClick 为真时这一次 click 不该切图');
eq(t12.swipe.suppressClick, false, 'suppressClick 必须被消费（否则下一次点击会被吞）');

// 多指手势后的 400ms 抑制窗
var t13 = makeCtx(); t13.tgesture.muteClickUntil = Date.now() + 400;
run(t13, 40, W);
eq(t13.calls.length, 0, '多指手势刚结束时补发的 click 必须被抑制');

// ★ 2.4.13 的取舍：左右**快速双击 = 连切两张**（每一下点击都算数）。
//   这正是"零延迟"的必然结果，也是 1.8.x 花几个版本治好的"连点被吃掉"的反面。
var t14 = makeCtx();
run(t14, 40, W, 'IMG', 1);
run(t14, 40, W, 'IMG', 2);      // 第二下 detail=2：**不再被特殊处理**
eq(t14.calls.join(','), 'prev,prev', '★★ 左右双击的第二下也要切图（两下都算数，不许吞掉）');
eq(t14.v.index, 0, '连点两次左 1/3 应退两张（索引 2 → 0）');
var t15 = makeCtx();
run(t15, 260, W, 'IMG', 1);
run(t15, 260, W, 'IMG', 2);
eq(t15.calls.join(','), 'next,next', '★★ 右侧双击也应进两张');
eq(t15.v.index, 4, '连点两次右 1/3 应进两张（索引 2 → 4）');

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
var body = grabAnon("vWrap.addEventListener('dblclick', function (e) ");
// 分区必须是**源码里的那一份** stageZoneX（2.4.13 起双击放大也走它）
// 抠出**具名函数**的完整源码（含签名与函数体），用来把真身喂进探针
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
var zoneFn = grab('stageZoneX');

function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// stage 与图片**同矩形**（400x300，left/top = 0）：三份边界在 133.3 / 266.7。
// 两者一致时"按屏幕分区"与"按图片分区"结果相同，可以专心验分区与焦点两件事。
function makeCtx() {
  var rect = { left: 0, top: 0, width: 400, height: 300 };
  var ctx = {
    VIEWER_CTL_SEL: 'button, a, input',
    v: { scale: 1, zoomed: false, actual: false, tx: 0, ty: 0, media: null },
    applied: 0,
    upgraded: 0,
    resetView: function () { ctx.v.scale = 1; ctx.v.zoomed = false; ctx.v.tx = 0; ctx.v.ty = 0; },
    updateFitBtn: function () {},
    applyTransform: function () { ctx.applied++; },
    maybeUpgradeToFull: function () { ctx.upgraded++; },
    _rect: rect
  };
  ctx.$ = function () { return { getBoundingClientRect: function () { return rect; } }; };
  ctx.v.media = {
    tagName: 'IMG',
    getBoundingClientRect: function () { return rect; }
  };
  return ctx;
}
function dbl(ctx, clientX, clientY, target) {
  var fn = new Function('e', 'var $ = this.$; ' + zoneFn + '; with (this) { ' + body + ' }');
  fn.call(ctx, {
    clientX: clientX, clientY: clientY,
    target: target || { tagName: 'IMG', closest: function () { return null; } }
  });
  return ctx;
}

// ★ 2.4.13：**只有中间 1/3 放大**，左/右两侧不再响应双击放大
var z1 = dbl(makeCtx(), 60, 150);
eq(z1.v.scale, 1, '★★ 双击左 1/3 **不得放大**（2.4.13 按用户要求）');
eq(z1.applied, 0, '左 1/3 双击不该产生任何 transform');
var z2 = dbl(makeCtx(), 340, 150);
eq(z2.v.scale, 1, '★★ 双击右 1/3 **不得放大**');
eq(z2.applied, 0, '右 1/3 双击不该产生任何 transform');

// 中间：以双击点为中心放大（2.3.0 的核心算法，别被改动弄坏）
var c1 = dbl(makeCtx(), 200, 150);
eq(c1.v.scale, 3, '双击应放大到 3 倍（2.4.0 按用户要求）');
eq(c1.v.tx, 0, '双击正中心时不平移');
eq(c1.v.ty, 0, '双击正中心时不平移');
ok(c1.upgraded > 0, '放大后应触发原图升级（maybeUpgradeToFull）');

// 双击在中心右侧 60px、下方 40px → 平移量应为 (-60, -40)
var c2 = dbl(makeCtx(), 260, 190);
eq(c2.v.scale, 3, '双击应放大到 3 倍（2.4.0 按用户要求）');
eq(c2.v.tx, -60, '★ 以双击点为中心：双击点相对图片中心的偏移取负作为平移量（x）');
eq(c2.v.ty, -40, '★ 以双击点为中心：双击点相对图片中心的偏移取负作为平移量（y）');
ok(c2.v.zoomed === true, '双击后应标记为已放大');
ok(c2.applied > 0, '双击后必须应用 transform');

// 已放大时中间双击 → 还原（且要应用 transform，否则界面不动）
var c3 = makeCtx();
c3.v.scale = 3; c3.v.zoomed = true; c3.v.tx = -60; c3.v.ty = -40;
dbl(c3, 260, 190);
eq(c3.v.scale, 1, '已放大时中间双击应还原到 1 倍');
eq(c3.v.tx, 0, '还原时平移量应清零');
ok(c3.applied > 0, '还原也必须应用 transform');

// 已放大时**点左/右 1/3 双击** → 不参与缩放（保持原状，交给 click 去切图）
var c4 = makeCtx();
c4.v.scale = 3; c4.v.zoomed = true; c4.v.tx = -60; c4.v.ty = -40;
dbl(c4, 60, 150);
eq(c4.v.scale, 3, '★ 左 1/3 双击不该把放大状态还原（那里只切图、不缩放）');
eq(c4.applied, 0, '左 1/3 双击不该产生 transform');

// 视频：不支持缩放（transform 只对 <img> 做）
var c5 = makeCtx();
c5.v.media = { tagName: 'VIDEO', getBoundingClientRect: function () { return c5._rect; } };
dbl(c5, 200, 150);
eq(c5.v.scale, 1, '视频中间双击不得缩放');

// 右下角控件上的双击不参与（vWrap 本身不含控件，这里守 closest 这条兜底）
var c6 = dbl(makeCtx(), 200, 150, {
  tagName: 'BUTTON',
  closest: function () { return {}; }
});
eq(c6.v.scale, 1, '控件上的双击不得缩放');

// 没有当前媒体（轨道已销毁）→ 什么都不做，且不能抛异常
var c7 = makeCtx();
c7.v.media = null;
dbl(c7, 200, 150);
eq(c7.v.scale, 1, 'v.media 为空时双击不得缩放也不得抛异常');

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
func Test2230ViewerPushesHistoryOnce(t *testing.T) {
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
  // 2.4.47：查看器压栈收敛到 pushNav（唯一出口），所以按同样的契约提供桩，
  //   同时把 path/photo 一并记下来 —— 才能钉住"URL 上必须看得见 &p="。
  var pushNav = function (st, path, photo) { env.pushed.push({ state: st, path: path, photo: photo }); return true; };
  var curPath = function () { return '/vol1'; };
  var syncExitTips = function () {};
  var api = new Function(
    'PROFILE', 'history', 'v', 'viewer', 'state', 'vWrap', 'cfg', 'singleMode',
    'stopAllMediaIn', 'applyViewerAnimation', 'suspendGridRequests', 'showCurrent', 'pauseThumbGen',
    'pushNav', 'curPath', 'syncExitTips',
    code
  )(
    profile, env.history, v, viewer, state, vWrap, cfg, false,
    function () {}, function () {}, function () {}, function () {}, function () {},
    pushNav, curPath, syncExitTips
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
eq(e1.pushed[0].photo, '/vol1/a.jpg', '★★ 查看器条目必须把 &p= 写进 URL —— ' +
  'URL 上看不出"在看哪张"，就还是靠 history.state 记账，落点一偏界面就会和 URL 脱节');

// 桌面档：**同样压一条**（2.4.46 起目录层也是 pushState，桌面浏览器后退键如果不接管，
// 在查看器里按后退会直接退掉一层目录，而不是关大图）
var e2 = makeEnv('mouse');
e2.api.open({ kind: 'file', path: '/vol1/a.jpg', name: 'a.jpg' });
eq(e2.pushed.length, 1, '★ 桌面档打开查看器同样要压一条历史（SPA 下目录层也是 pushState）');

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
// 2.4.13：click 走 stageZoneX 分区，把源码里的真身一起喂进来
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
var zoneFn = grab('stageZoneX');
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
  // stageZoneX 通过 $('vStage') 取矩形
  ctx.$ = function () { return { getBoundingClientRect: ctx.getBoundingClientRect }; };
  var fn = new Function('e', 'var $ = this.$; ' + zoneFn + '; with (this) { ' + body + ' }');
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
