// 2.4.43：从竖图流返回网格时，**已经在屏上的那一行不许动**。
//
// 用户报的现象：手机一屏两列八张（四行），点任意一张进竖图流，返回后网格总会把
// 那一行摆到"视口上方 1/4 处"（原意是滑了很远回来还能看到上下文），于是整屏内容
// 被推走 —— 点的那张跳到"第二排那个位置"。一屏之内点开根本不需要定位。
//
// 三层验证：
//
//	① 纯函数（node 跑真实的 scrollToItem）：完整可见→不动 / 只露一半→最小滚动 / 很远→1/4；
//	② 静态闸门：判"完整可见"不许带 pad，scrollTop 赋值必须在 !visible 分支里；
//	③ 端到端（真页面 + 真 flow + headless Edge）：点一张→进流→返回，
//	   网格 scrollTop 与每个格子的屏幕位置一个像素都不许变；同时"滑很远再回来"仍要定位。
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScrollToItem 把 app.js 里真实的 scrollToItem 抠出来，在给定几何下跑一次，返回 scrollTop。
func runScrollToItem(t *testing.T, items, cols, cell, gap, vh, top, idx int) int {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	appPath, _ := filepath.Abs(filepath.Join("web", "app.js"))
	if _, err := os.Stat(appPath); err != nil {
		t.Skipf("找不到 web/app.js：%v", err)
	}
	harness := `
const fs = require('fs');
const src = fs.readFileSync(process.env.MV_APPJS, 'utf8');
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
const o = JSON.parse(process.env.MV_CASE);
const body =
  'var state = { items: new Array(' + o.items + '), cols: ' + o.cols +
  ', cell: ' + o.cell + ', gap: ' + o.gap + ' };' +
  'var scroller = { clientHeight: ' + o.vh + ', scrollTop: ' + o.top + ' };' +
  'function requestRender() {}' +
  'function requestAnimationFrame() {}' +
  'var cellPool = {};' +
  grab('scrollToItem') +
  'scrollToItem(' + o.idx + ', false);' +
  'return scroller.scrollTop;';
console.log(JSON.stringify({ y: new Function(body)() }));
`
	c, _ := json.Marshal(map[string]int{
		"items": items, "cols": cols, "cell": cell, "gap": gap, "vh": vh, "top": top, "idx": idx,
	})
	cmd := exec.Command(node, "-e", harness)
	cmd.Env = append(os.Environ(), "MV_APPJS="+appPath, "MV_CASE="+string(c))
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node 失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("node 失败：%v", err)
	}
	var res struct {
		Y int `json:"y"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &res); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	return res.Y
}

// Test2443ScrollToItemThreeTiers 纯函数：三档行为。
//
// 几何取真机常见值：两列、cell=185、gap=8（行距 193）、可视高 780（4 行刚好铺满）。
func Test2443ScrollToItemThreeTiers(t *testing.T) {
	const cell, gap = 185, 8
	const pitch = cell + gap // 193

	// ① 完整可见（第 3 行：0..185 / 193..378 / 386..571 / 579..764 都 ≤ 780）→ 一格都不动
	for _, idx := range []int{0, 2, 4, 6} {
		if y := runScrollToItem(t, 40, 2, cell, gap, 780, 0, idx); y != 0 {
			t.Errorf("★★ 第 %d 张完整可见却滚到了 %d —— 这就是用户看到的「点哪张都跳到第二排」", idx, y)
		}
	}

	// ② 只露出下半截（第 4 行 579..764，视口只有 700）→ 最小滚动，刚好整行露出来
	//    want = 579 + 185 + pad(23) - 700 = 87
	if y := runScrollToItem(t, 40, 2, cell, gap, 700, 0, 6); y != 87 {
		t.Errorf("★ 只露一半时应做最小滚动（期望 87，实际 %d）—— 既不能不动，也不该甩到 1/4", y)
	}
	// 上方被裁掉同理（第 1 行 0..185，视口从 300 开始）：want = 0 - pad < 0 ⇒ 0
	if y := runScrollToItem(t, 40, 2, cell, gap, 700, 300, 0); y != 0 {
		t.Errorf("★ 目标在上方被裁掉时应回到 0，实际 %d", y)
	}

	// ③ 完全在屏外（真的滑了很远）→ 仍放到"视口上方 1/4 处"，保留上下文
	far := 78 // 第 40 行：39*193 = 7527
	if y := runScrollToItem(t, 80, 2, cell, gap, 780, 0, far); y != 39*pitch-195 {
		t.Errorf("★★ 目标远在屏外时必须定位（期望 %d，实际 %d）—— 否则滑了几十张回来就找不到了",
			39*pitch-195, y)
	}
	if y := runScrollToItem(t, 80, 2, cell, gap, 780, 6000, 0); y != 0 {
		t.Errorf("★ 目标远在上方时应回到顶部，实际 %d", y)
	}
}

// Test2443ScrollToItemStatic 静态闸门：这几条一旦被改回去，行为测试也会红，但这里把意图写下来。
func Test2443ScrollToItemStatic(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	body := segmentBetween(js, "function scrollToItem(idx, flash) {", "\n  // flowModule 取竖图流模块")
	if body == "" {
		t.Fatal("找不到 scrollToItem")
	}

	// 判"完整可见"必须**不带 pad**：带 pad 会让"刚好贴边的那一行"被判成不可见，又变成一动就跳
	if !strings.Contains(body, "var visible = rowTop >= top && (rowTop + rowH) <= (top + vh);") {
		t.Error("★★ 「完整可见」判定必须严格按 [top, top+vh] 比，不许掺 pad —— " +
			"掺了会让贴边那一行也被判成不可见，于是照样跳")
	}
	// 不许在 !visible 分支之外赋值 scrollTop
	iBranch := strings.Index(body, "if (!visible) {")
	iSet := strings.Index(body, "scroller.scrollTop =")
	if iBranch < 0 || iSet < 0 {
		t.Fatalf("结构变了（iBranch=%d iSet=%d）", iBranch, iSet)
	}
	if iSet < iBranch {
		t.Error("★★ scroller.scrollTop 必须在 if (!visible) 里面 —— 放到外面就是" +
			"「不管可见与否都滚」，用户报的跳正是它")
	}
	// 远距离的"1/4 处"必须保留（本意是好的，只是不该用在屏内）
	if !strings.Contains(body, "vh / 4") {
		t.Error("★ 远距离定位仍要用 vh/4 留上下文，别为了不跳把定位整个删掉")
	}
}

// Test2443ReturnFromFlowKeepsVisibleGridRealBrowser 端到端。
//
// 两段：
//
//	① 点一张**完整可见**的图进竖图流再返回 ⇒ scrollTop 不变、所有格子屏幕位置不变、仍有 flash；
//	② 在流里滚到末尾再返回 ⇒ 网格**必须**滚过去（防止"为了不跳把定位关掉"）。
func Test2443ReturnFromFlowKeepsVisibleGridRealBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := edgeForTest(t)

	script, _ := filepath.Abs(filepath.Join("..", "tools", "grid_locate_e2e.js"))
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("找不到探针：%v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("探针失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("探针失败：%v", err)
	}

	type cellPos struct {
		P string  `json:"p"`
		T float64 `json:"t"`
		H float64 `json:"h"`
	}
	type snap struct {
		Scroll int       `json:"scroll"`
		VH     int       `json:"vh"`
		Cells  []cellPos `json:"cells"`
	}
	var res struct {
		APick *struct {
			Idx  int    `json:"idx"`
			Path string `json:"path"`
		} `json:"a_pick"`
		AOpen   bool `json:"a_open"`
		ABack   bool `json:"a_back"`
		ABefore snap `json:"a_before"`
		AAfter  snap `json:"a_after"`
		AFlash  bool `json:"a_flash"`
		BPick   *struct {
			Idx  int    `json:"idx"`
			Path string `json:"path"`
		} `json:"b_pick"`
		BFlowIdx int  `json:"b_flowIdx"`
		BBefore  snap `json:"b_before"`
		BAfter   snap `json:"b_after"`
		BFlash   bool `json:"b_flash"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}

	// ---- ① 一屏之内，一格都不许动 ----
	if res.APick == nil || !res.AOpen || !res.ABack {
		t.Fatalf("前置不成立（pick=%v open=%v back=%v）", res.APick, res.AOpen, res.ABack)
	}
	if len(res.ABefore.Cells) < 6 {
		t.Fatalf("首屏只有 %d 个格子，样本不足", len(res.ABefore.Cells))
	}
	if res.AAfter.Scroll != res.ABefore.Scroll {
		t.Errorf("★★ 返回后网格 scrollTop %d → %d：目标本来就在屏上，不该滚（用户看到的就是「内容被推走」）",
			res.ABefore.Scroll, res.AAfter.Scroll)
	}
	byP := map[string]float64{}
	for _, c := range res.ABefore.Cells {
		byP[c.P] = c.T
	}
	moved := 0
	for _, c := range res.AAfter.Cells {
		if t0, ok := byP[c.P]; ok && (c.T-t0 > 0.5 || t0-c.T > 0.5) {
			moved++
		}
	}
	if moved > 0 {
		t.Errorf("★★ 返回后有 %d 个格子的屏幕位置变了 —— 一屏之内点开不该动任何东西", moved)
	}
	if !res.AFlash {
		t.Error("★ 不再滚动之后仍然要闪一下高亮，否则用户不知道回到的是哪张")
	}

	// ---- ② 滑很远再回来，定位必须还生效 ----
	if res.BFlowIdx < 20 {
		t.Fatalf("探针没能在流里滑远（flowIdx=%d），这一段等于没测", res.BFlowIdx)
	}
	if res.BAfter.Scroll <= res.BBefore.Scroll {
		t.Errorf("★★ 滑到第 %d 张再返回，网格却没滚（%d → %d）—— 定位被整个关掉了",
			res.BFlowIdx, res.BBefore.Scroll, res.BAfter.Scroll)
	}
	if !res.BFlash {
		t.Error("★ 远距离定位后也要闪一下高亮")
	}
}
