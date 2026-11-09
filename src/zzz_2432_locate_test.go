package main

// 2.4.32：修「竖图流不管点第几张都从第一张开始」。
//
// 根因：open() 里先把 scrollTop 设成 pre[idx]，**然后才** updateSpacers()。
// 设的那一刻两个 spacer 高度都还是 0（close 里归零过），滚动高度为 0，
// 浏览器把 scrollTop 静默 clamp 成 0（不报错），随后 render() 按"视口 35%"
// 把当前索引重算成 0 —— 于是无论点第几张，打开都在最顶上。
//
// 钉两件事：
//   ① 顺序：spacer 必须先撑起来，再设 scrollTop；且设完要**读回来自检**（clamp 是静默的）；
//   ② 刚打开那一帧不许被"焦点线"覆盖（2.4.44 起焦点线是**视口中线**，见 render；
//      首/末张在几何上顶不到中线，靠按滚动位置释放的锁保住 index）。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func Test2432OpenLocatesToClickedIndex(t *testing.T) {
	flow := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "flow.js")))

	// ---------- ① 顺序 + 自检 ----------
	iSpacer := strings.Index(flow, "updateSpacers();\n    scrollToIndex(st.idx);")
	if iSpacer < 0 {
		// gofmt 不会动 js，但仍允许有空格差异：退化成"两者相对位置"判断
		a := strings.Index(flow, "updateSpacers();")
		b := strings.Index(flow, "scrollToIndex(st.idx);")
		if a < 0 || b < 0 || a > b {
			t.Error("★ open() 里必须先 updateSpacers() 再 scrollToIndex()：spacer 没撑起来时 scrollTop 会被静默 clamp 成 0")
		}
	}
	if strings.Contains(flow, "scrollEl.scrollTop = st.pre[st.idx];\n    updateSpacers();") {
		t.Error("★ 又变回\"先设 scrollTop 再撑 spacer\"了 —— 这正是定位失效的根因")
	}
	if !strings.Contains(flow, "function scrollToIndex(") {
		t.Fatal("★ 缺 scrollToIndex（定位 + clamp 自检）")
	}
	// clamp 是静默的：必须读回来比对
	if !strings.Contains(flow, "Math.abs(scrollEl.scrollTop - want) <= 1") {
		t.Error("★ scrollToIndex 必须读回 scrollTop 自检（越界会被浏览器静默截断，不报错）")
	}
	if !strings.Contains(flow, "requestAnimationFrame(function () {") {
		t.Error("★ scrollToIndex 布局未就绪时要在下一帧再对一次")
	}

	// ---------- ② 打开那一帧不许被 35% 焦点覆盖 ----------
	renderBody := segmentBetween(flow, "function render() {", "function updateHud()")
	if renderBody == "" {
		t.Fatal("★ 找不到 render 函数体（结构变了，闸门失效）")
	}
	if !strings.Contains(renderBody, "if (st.pin)") {
		t.Error("★ render 必须尊重 st.pin：否则横图（半屏高）打开时 HUD/索引会跳到下一张")
	}
	if !strings.Contains(flow, "pin: false") {
		t.Error("★ st 初始化里缺 pin 字段")
	}
}

// Test2432OpenLocateRealRender 用真实浏览器验证"点第 5 张打开就在第 5 张"。
func Test2432OpenLocateRealRender(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := os.Getenv("MV_EDGE")
	if edge == "" && runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				edge = p
				break
			}
		}
	}
	if edge == "" {
		t.Skip("没有 Edge，跳过真实渲染验证")
	}

	cmd := exec.Command(node, filepath.Join("..", "tools", "flow_probe.js"))
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("探针运行失败：%v", err)
	}
	var res struct {
		OpenAt4 bool        `json:"openAt4"`
		At4     flowMetrics `json:"at4"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}
	if !res.OpenAt4 {
		t.Fatal("MVFlow.open(4) 返回 false")
	}
	if res.At4.Idx != 4 {
		t.Errorf("★ 点第 5 张却停在索引 %d（应为 4）—— 定位又失效了", res.At4.Idx)
	}
	if !strings.HasPrefix(res.At4.Hud, "5 /") {
		t.Errorf("★ HUD 应为「5 / N」，实际 %q", res.At4.Hud)
	}
	// 2.4.44：打开时目标张要摆在**视口中部**（顶边对齐会让上方一点上下文都没有，
	//   看起来像"打开就在最上面"）。这条只对"比视口矮、且不在首尾"的图成立 ——
	//   首张/末张被几何 clamp，长图则只能顶对齐，都由 targetCanCenter 区分。
	if !res.At4.TargetCanCenter {
		t.Fatalf("探针里的第 5 张本该能居中（高 %d / 视口 %d），夹具变了", res.At4.TargetTop, res.At4.Vh)
	}
	if off := res.At4.TargetCenterOff; off < -2 || off > 2 {
		t.Errorf("★ 打开后目标张没有居中：图中心相对视口中心 %+d px（应 ≈ 0）", off)
	}
	if res.At4.TargetTop < 0 {
		t.Errorf("★ 目标张顶边跑到视口外（%d px）—— 定位跑到上面去了", res.At4.TargetTop)
	}
}
