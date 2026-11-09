// 2.4.13 新增：大图「点击切换零延迟」+「双击放大只在中间 1/3」。
//
// 这两条是同一个决策的两面，必须一起钉住：
//
//	· 单击切图**当帧生效**（触屏与鼠标一致），判定窗整笔删除；
//	· 双击放大**只认中间 1/3**，左右两侧的双击按"两次点击"处理（各切一张）。
//
// 为什么中间留空就能删掉判定窗：双击的两下 click 都落在中间，而中间不切图，
// 所以"第一下被切走"这件事根本不会发生 —— 当初为了不闪而加的 220ms 判定窗
// 也就失去作用对象。一旦有人只删判定窗、却忘了"双击只在中间"，
// 左右双击就会变成"切一张 + 放大"（2.3.2 前用户实测报过的老 bug），
// 所以下面既有静态闸门，也有把两个处理器真跑一遍的行为探针。
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ① 静态闸门：click 里不许再有任何"判定窗"痕迹
// ---------------------------------------------------------------------------
func Test2413ClickHandlerHasNoTapDelayWindow(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "$('vStage').addEventListener('click'")
	if i < 0 {
		t.Fatal("找不到 vStage 的 click 监听")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  });"); j > 0 {
		seg = seg[:j]
	}

	// ① 不许有定时器：这是"零延迟"的机械判据（一出现 setTimeout 就说明判定窗回来了）
	if strings.Contains(seg, "setTimeout") {
		t.Error("★ click 处理器里**不得**出现 setTimeout —— 2.4.13 已删除 220ms 单击/双击判定窗，" +
			"切图必须当帧生效；加回来会让每一次单击都慢 220ms")
	}
	if strings.Contains(seg, "MV_TAP_DELAY_MS") || strings.Contains(seg, "mvTap") {
		t.Error("click 里仍有判定窗的残留标识（MV_TAP_DELAY_MS / mvTap）—— 这两者 2.4.13 已整体删除")
	}
	// ② 双击的第二下不再特殊处理：每一下点击都算数（左右双击 = 连切两张，已知并接受）
	if strings.Contains(seg, "e.detail") {
		t.Error("★ click 里**不得**再按 e.detail 分流 —— 判定窗删掉后没有「待取消的切图」可取消，" +
			"留着只会静默吞掉第二下点击（表现为「快点两下只走一张」）")
	}
	// ③ 分区必须走唯一来源 stageZoneX
	if !strings.Contains(seg, "stageZoneX(e.clientX)") {
		t.Error("★ click 必须用 stageZoneX(e.clientX) 分区 —— 它是「屏幕三份」的唯一来源，" +
			"就地重算 _left/_right 会让分区出现第二个来源，与双击放大判定漂移")
	}
	// ④ 两道保留的守卫不能丢
	if !strings.Contains(seg, "if (swipe.suppressClick)") {
		t.Error("click 里丢了 suppressClick 的消费（pointerup 已按点击处理过，不消费会切两次）")
	}
	if !strings.Contains(seg, "if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) return;") {
		t.Error("click 里丢了「拖动 > 8px 不算点击」的距离守卫")
	}
	if !strings.Contains(seg, "if (PROFILE === 'touch' && v.scale > 1.01) return;") {
		t.Error("click 里丢了 2.4.1 的「手机版放大后不响应左右切图」")
	}
}

// ---------------------------------------------------------------------------
// ② 静态闸门：双击放大必须挂在 vWrap 上、且只认中间 1/3
// ---------------------------------------------------------------------------
func Test2413DblclickMovedToWrapAndZoneGated(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	// ① 不许再挂在每个 <img> 上（那会让"黑边上双击"没反应，且分区基准含糊）
	if strings.Contains(js, "img.addEventListener('dblclick'") {
		t.Error("★ 双击放大**不得**再挂在 <img> 上 —— 2.4.13 改挂到 vWrap，" +
			"否则长图上下留黑边、宽图左右留黑边的地方双击没反应")
	}
	i := strings.Index(js, "vWrap.addEventListener('dblclick'")
	if i < 0 {
		t.Fatal("找不到 vWrap 的 dblclick 监听 —— 双击放大必须在 vWrap 上注册一次")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  });"); j > 0 {
		seg = seg[:j]
	}
	// ② 分区守卫（★ 这是"左右不再响应双击放大"的唯一实现，不能被删）
	if !strings.Contains(seg, "if (stageZoneX(e.clientX) !== 0) return;") {
		t.Error("★★ 双击放大缺分区守卫 `if (stageZoneX(e.clientX) !== 0) return;` —— " +
			"删了它就会回到「左右 1/3 也放大」的旧行为（用户明确不要）")
	}
	// ③ 媒体类型守卫（视频没有 transform，放大了只会让 v.zoomed 骗住切图逻辑）
	if !strings.Contains(seg, "m.tagName !== 'IMG'") {
		t.Error("双击放大缺「只对 IMG 生效」的守卫（视频不支持缩放）")
	}
	// ④ 2.3.0 的焦点算法（以双击点为中心）与 2.4.0 的 3 倍不能丢
	if !strings.Contains(seg, "v.scale = 3;") {
		t.Error("双击放大倍数应是 3 倍（2.4.0 按用户要求）")
	}
	if !strings.Contains(seg, "v.tx = -cx; v.ty = -cy;") {
		t.Error("双击放大丢了「以双击点为中心」（v.tx/v.ty = 偏移取负）")
	}
	if !strings.Contains(seg, "maybeUpgradeToFull();") {
		t.Error("双击放大后必须触发原图升级（maybeUpgradeToFull）")
	}
}

// ---------------------------------------------------------------------------
// ③ 行为：stageZoneX 的分区必须以 vStage 为基准（含 left 偏移）
// ---------------------------------------------------------------------------
func Test2413StageZoneXUsesStageRect(t *testing.T) {
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
var fnSrc = grab('stageZoneX');
function zone(x, rect) {
  var $ = function () { return { getBoundingClientRect: function () { return rect; } }; };
  var fn = new Function('$', 'return ' + fnSrc);
  return fn($)(x);
}

// stage: left=50, width=300 → 分区线在屏幕坐标 150 / 250
var r = { left: 50, top: 0, width: 300, height: 400 };

// ★ 关键：客户端坐标减去 rect.left 之后才比较 —— 若忘了减 left，左边界会整体偏移 50px
eq(zone(99, r), -1, '屏幕坐标 99（stage 内 49px）应判为左 1/3');
eq(zone(100, r), -1, '屏幕坐标 100（stage 内 50px）仍在左 1/3');
eq(zone(149, r), -1, '屏幕坐标 149（stage 内 99px）仍在左 1/3');
eq(zone(151, r), 0, '★ 屏幕坐标 151（stage 内 101px）应进入中间 1/3 —— 少减 rect.left 会算错');
eq(zone(200, r), 0, '正中应判为中间');
eq(zone(249, r), 0, '屏幕坐标 249（stage 内 199px）仍在中间');
eq(zone(251, r), 1, '★ 屏幕坐标 251（stage 内 201px）应判为右 1/3');
eq(zone(400, r), 1, '最右应判为右 1/3');

// 另一个矩形（全屏，left=0）也必须对
var r2 = { left: 0, top: 0, width: 1920, height: 1080 };
eq(zone(639, r2), -1, '1920 宽：639 仍属左 1/3');
eq(zone(641, r2), 0, '1920 宽：641 进入中间');
eq(zone(1279, r2), 0, '1920 宽：1279 仍在中间');
eq(zone(1281, r2), 1, '1920 宽：1281 进入右 1/3');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("stageZoneX 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// ---------------------------------------------------------------------------
// ④ 行为：零延迟 + 左右双击连切两张（把 click 处理器真跑一遍）
// ---------------------------------------------------------------------------
func Test2413TapSwitchIsSynchronous(t *testing.T) {
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
var body = grabAnon("$('vStage').addEventListener('click', function (e) ");
var zoneFn = grab('stageZoneX');

function makeCtx(profile) {
  var ctx = {
    timers: [], calls: [],
    PROFILE: profile,
    setTimeout: function (fn, ms) { ctx.timers.push(ms); return ctx.timers.length; },
    clearTimeout: function () {},
    tgesture: { muteClickUntil: 0 },
    swipe: { suppressClick: false, dx: 0, dy: 0 },
    VIEWER_CTL_SEL: 'button, a, input',
    SWIPE_LOCK_SCALE: 2,
    v: { scale: 1, index: 2, list: [1, 2, 3, 4, 5] },
    flashCounter: function () {},
    resetView: function () {},
    applyTransform: function () {},
    prev: function () { ctx.calls.push('prev'); if (ctx.v.index > 0) ctx.v.index--; },
    next: function () { ctx.calls.push('next'); if (ctx.v.index < ctx.v.list.length - 1) ctx.v.index++; }
  };
  ctx.$ = function () { return { getBoundingClientRect: function () { return { left: 0, top: 0, width: 300, height: 300 }; } }; };
  return ctx;
}
function click(ctx, x, detail) {
  var fn = new Function('e', 'var $ = this.$; ' + zoneFn + '; with (this) { ' + body + ' }');
  fn.call(ctx, {
    clientX: x, clientY: 150, detail: detail || 1,
    target: { tagName: 'IMG', closest: function () { return null; } }
  });
  return ctx;
}

// ★ 核心：click 返回时切图**已经发生**，且一个定时器都没挂
['touch', 'mouse'].forEach(function (p) {
  var c = click(makeCtx(p), 40, 1);
  eq(c.calls.join(','), 'prev', p + '：点击当帧必须切上一张（不得延迟）');
  eq(c.timers.length, 0, p + '：切图不得挂任何定时器');
});

// ★ 左右双击 = 连切两张（每一下都算数）
var c2 = click(click(makeCtx('touch'), 260, 1), 260, 2);
eq(c2.calls.join(','), 'next,next', '右 1/3 双击第二下也必须切图（不得被 e.detail 吃掉）');
eq(c2.v.index, 4, '右 1/3 连点两次应进两张');

// 中间单击：两下也都不切图（这是双击放大的前提）
var c3 = click(click(makeCtx('touch'), 150, 1), 150, 2);
eq(c3.calls.length, 0, '中间 1/3 连点两下都不得切图');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("零延迟探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
