package main

// 2.4.44：竖图流打开时，点的那张要摆在**视口中部**（原来顶边对齐，看起来像"打开就停在最上面"）。
//
// 三件事必须同时成立，少一件就会退化成"改了但没生效"：
//   ① 定位用 `pre[i] - 居中偏移`，且长图（比视口高）退回顶对齐；
//   ② "当前张"的判据跟着换成**中线**，否则居中后 35% 线会落到上一张 → HUD 与退出定位全错；
//   ③ 首张/末张被几何 clamp 时，得靠"按滚动位置释放的锁"保住 index，
//      并且比例异步纠正后要**重新居中**（否则最终停在偏上的位置）。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- ① 静态闸门 ----------

func Test2444FlowCenterStatic(t *testing.T) {
	flow := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "flow.js")))

	for _, need := range []string{
		"function centerOffset(",
		"function maxScrollTop(",
		"function pinTo(",
		"- centerOffset(i)",
	} {
		if !strings.Contains(flow, need) {
			t.Errorf("★ 缺 %q —— 居中定位的三个零件必须都在", need)
		}
	}
	// 不许退回"顶边对齐"
	if strings.Contains(flow, "var want = st.pre[i] || 0;") ||
		strings.Contains(flow, "scrollEl.scrollTop = st.pre[target] || 0;") {
		t.Error("★★ 又变回顶边对齐了 —— 用户点的图会贴在屏幕最上面")
	}

	// 长图必须顶对齐（居中会把头和尾都推出视口）
	centerBody := segmentBetween(flow, "function centerOffset(i) {", "// pinTo ")
	if !strings.Contains(centerBody, "h >= vh") {
		t.Error("★ centerOffset 必须对「比视口还高的长图」返回 0（顶对齐），否则长图头尾都看不到")
	}

	// 当前张判据必须换成中线，并且锁按滚动位置释放
	renderBody := segmentBetween(flow, "function render() {", "function updateHud()")
	if !strings.Contains(renderBody, "vh * 0.5") {
		t.Error("★★ 「当前张」必须取视口中线：居中定位后 35% 线会落到上一张，" +
			"HUD 显示错、退出后网格也定位到错的那张")
	}
	if strings.Contains(renderBody, "vh * 0.35") {
		t.Error("★ 35% 焦点线与居中定位不再自洽，必须删掉")
	}
	if !strings.Contains(renderBody, "st.pinY") || !strings.Contains(renderBody, "st.pin = false") {
		t.Error("★★ 锁必须按 scrollTop（st.pinY）释放，不能按帧数：首/末张被 clamp 时需要它保住 index")
	}

	// 比例纠正不许把居中的负偏移吃成 0
	ratioBody := segmentBetween(flow, "function applyRatio(i, r) {", "function refreshMountedHeights()")
	if strings.Contains(ratioBody, "Math.max(0, off)") {
		t.Error("★★ applyRatio 里不许 `Math.max(0, off)`：居中时 off 本来是负的，" +
			"clamp 成 0 会把居中的那张推回顶边（用户正看着的画面会跳一下）")
	}
	if !strings.Contains(ratioBody, "scrollToIndex(i)") {
		t.Error("★★ 打开那张的比例是异步纠正的，高度变了必须重新居中 —— 否则最终停在偏上的位置")
	}
}

// ---------- ② 纯函数：真实 scrollToIndex + 能模拟 clamp 的滚动容器 ----------

type centerCase struct {
	Top        int  `json:"top"`
	Vh         int  `json:"vh"`
	Pin        bool `json:"pin"`
	PinIdx     int  `json:"pinIdx"`
	PinY       int  `json:"pinY"`
	Idx        int  `json:"idx"`
	Pending    int  `json:"pending"`
	RenderCall int  `json:"renderCalls"`
}

func Test2444FlowCenterMath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	flowPath, _ := filepath.Abs(filepath.Join("web", "flow.js"))

	harness := `
const fs = require('fs');
const src = fs.readFileSync(process.env.MV_FLOWJS, 'utf8');
function grab(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('no ' + name);
  let d = 0;
  for (let k = src.indexOf('{', i); k < src.length; k++) {
    if (src[k] === '{') d++;
    else if (src[k] === '}') { d--; if (!d) return src.slice(i, k + 1); }
  }
  throw new Error('unbalanced ' + name);
}
const head =
  'var st = null, scrollEl = null, renderCalls = 0, rafQ = [];' +
  'function requestAnimationFrame(f) { rafQ.push(f); return rafQ.length; }' +
  'function render() { renderCalls++; }' +
  'function updateHud() {}';
const body = head + grab('maxScrollTop') + grab('centerOffset') + grab('pinTo') + grab('scrollToIndex') + ` + "`" + `
function makeScroll(total, vh, shZero) {
  return {
    clientHeight: vh, clientWidth: 390, _top: 0,
    get scrollHeight() { return shZero ? 0 : total; },
    get scrollTop() { return this._top; },
    set scrollTop(v) {
      var max = Math.max(0, this.scrollHeight - this.clientHeight);
      this._top = Math.min(Math.max(0, v), max);
    }
  };
}
function peek() {
  return { top: scrollEl.scrollTop, vh: scrollEl.clientHeight,
           pin: st.pin, pinIdx: st.pinIdx, pinY: st.pinY,
           idx: st.idx, renderCalls: renderCalls, pending: rafQ.length };
}
function run(heights, vh, idx, shZero) {
  var total = 0, pre = [0];
  for (var i = 0; i < heights.length; i++) { total += heights[i]; pre.push(total); }
  scrollEl = makeScroll(total, vh, shZero);
  st = { h: heights.slice(), pre: pre, idx: idx, pin: false, pinIdx: -1, pinY: 0, list: [] };
  renderCalls = 0; rafQ = [];
  scrollToIndex(idx);
  var r = peek(); r.pending = rafQ.length;
  return r;
}
function flush() {
  var q = rafQ.slice(); rafQ = [];
  for (var i = 0; i < q.length; i++) q[i]();
  var r = peek(); r.pending = rafQ.length;
  return r;
}
return { run: run, flush: flush };
` + "`" + `;
const api = new Function(body)();
const ten = [], tall = [];
for (let i = 0; i < 10; i++) ten.push(600);
for (let i = 0; i < 4; i++) tall.push(2000);

console.log(JSON.stringify({
  mid:    api.run(ten, 800, 3, false),
  first:  api.run(ten, 800, 0, false),
  last:   api.run(ten, 800, 9, false),
  tall:   api.run(tall, 800, 2, false),
  notReady: api.run(ten, 800, 3, true),
  notReadyAfterFlush: api.flush()
}));
`
	cmd := exec.Command(node, "-e", harness)
	cmd.Env = append(os.Environ(), "MV_FLOWJS="+flowPath)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node 运行失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("node 运行失败：%v", err)
	}
	var res map[string]centerCase
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}

	// 10 张 600 高、视口 800 ⇒ 每张居中时顶边在视口上方 100px
	const VH, H = 800, 600
	if got := res["mid"]; got.Top != 3*H-(VH-H)/2 || !got.Pin || got.PinIdx != 3 || got.PinY != got.Top {
		t.Errorf("★★ 第 4 张没有居中：top=%d（应为 %d）、pin=%v、pinIdx=%d",
			got.Top, 3*H-(VH-H)/2, got.Pin, got.PinIdx)
	} else {
		// 视口坐标系里：图顶边 = pre - top，图中心应当压在视口中心上（±1px 取整误差）
		viewTop := 3*H - got.Top
		if centerOff := (viewTop + H/2) - got.Vh/2; centerOff < -1 || centerOff > 1 {
			t.Errorf("★ 图中心偏离视口中心 %+d px（图顶边在视口 y=%d，视口高 %d）",
				centerOff, viewTop, got.Vh)
		}
	}
	// 首张：没有负滚动，只能贴顶，但 index 必须锁在它身上
	if got := res["first"]; got.Top != 0 || !got.Pin || got.PinIdx != 0 {
		t.Errorf("★ 首张应贴顶(top=0)且锁定 index=0，实际 top=%d pin=%v pinIdx=%d",
			got.Top, got.Pin, got.PinIdx)
	}
	// 末张：被 clamp 到可滚动上限，仍算成功（否则会一直重试到帧耗尽），index 必须锁在 9
	if got := res["last"]; got.Top != 10*H-VH || !got.Pin || got.PinIdx != 9 || got.Pending != 0 {
		t.Errorf("★ 末张应贴底(clamp 到 %d)且锁定 index=9、不再重试，实际 top=%d pin=%v pinIdx=%d pending=%d",
			10*H-VH, got.Top, got.Pin, got.PinIdx, got.Pending)
	}
	// 比视口高的长图：顶对齐（居中会头尾都看不到）
	if got := res["tall"]; got.Top != 2*2000 || !got.Pin || got.PinIdx != 2 {
		t.Errorf("★ 长图（2000 > 视口 800）必须顶对齐：top=%d（应为 4000）、pinIdx=%d", got.Top, got.PinIdx)
	}
	// 布局没撑起来（scrollHeight=0）：不许"假装成功"，必须挂起等下一帧
	if got := res["notReady"]; got.Pin || got.Pending != 1 {
		t.Errorf("★★ scrollHeight 还是 0 时不能认定定位成功（pin=%v pending=%d）—— "+
			"那会把 index 锁在一张根本没显示出来的图上", got.Pin, got.Pending)
	}
	if got := res["notReadyAfterFlush"]; got.Idx != 3 {
		t.Errorf("★ 补帧重试时应该把目标下标带回来（st.idx=%d，应为 3）", got.Idx)
	}
}

// ---------- ③ 真实浏览器：中间张居中、末张贴底 ----------

func Test2444FlowCenterRealRender(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := edgeForTest(t)

	cmd := exec.Command(node, filepath.Join("..", "tools", "flow_probe.js"))
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("探针失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("探针失败：%v", err)
	}
	var res struct {
		At0    flowMetrics `json:"at0"`
		At4    flowMetrics `json:"at4"`
		AtLast flowMetrics `json:"atLast"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}

	// 第 5 张（9:16，高 693 < 视口 844）：必须居中
	if !res.At4.TargetCanCenter {
		t.Fatalf("夹具变了：第 5 张本该能居中（top=%d vh=%d）", res.At4.TargetTop, res.At4.Vh)
	}
	if off := res.At4.TargetCenterOff; off < -2 || off > 2 {
		t.Errorf("★★ 第 5 张没居中：图中心相对视口中心 %+d px（应 ≈0）", off)
	}
	// 首张：贴顶（0），不能是负数（那意味着滚出了边界）
	if res.At0.TargetTop != 0 {
		t.Errorf("★ 首张应贴顶（top=0），实际 %d", res.At0.TargetTop)
	}
	// 末张：贴底 —— 图底边应当贴着视口底边（几何上顶不到中部）
	lastH := -1.0
	for k, idx := range res.AtLast.Indexes {
		if idx == res.AtLast.Idx && k < len(res.AtLast.Heights) {
			lastH = res.AtLast.Heights[k]
		}
	}
	if lastH > 0 {
		if bottom := float64(res.AtLast.TargetTop) + lastH; bottom < float64(res.AtLast.Vh)-2 ||
			bottom > float64(res.AtLast.Vh)+2 {
			t.Errorf("★ 末张应贴底：index=%d 顶边=%d 高=%.0f ⇒ 图底边在 %d（视口高 %d）",
				res.AtLast.Idx, res.AtLast.TargetTop, lastH, int(bottom), res.AtLast.Vh)
		}
	} else {
		t.Errorf("探针没量到末张的高度（indexes=%v heights=%v）", res.AtLast.Indexes, res.AtLast.Heights)
	}
	if !strings.HasPrefix(res.AtLast.Hud, "8 /") {
		t.Errorf("★ 末张的 HUD 应为「8 / 8」，实际 %q —— 贴底时 index 被焦点线带跑了", res.AtLast.Hud)
	}
	if res.AtLast.Idx != 7 {
		t.Errorf("★ 打开末张后 index=%d（应为 7）", res.AtLast.Idx)
	}
}
