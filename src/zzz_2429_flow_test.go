package main

// 2.4.29：手机端「竖图连续浏览」的回归测试。
//
// 这一版的特点是"大部分逻辑在一个独立前端文件里"，所以测试分三层：
//   ① 静态闸门：设置项 4 处 + 接线 6 处 + 样式 3 条，**任何一处漏了都要红**
//      （2.4.28 的教训：断言全绿但前端漏一处，等于功能没生效）；
//   ② 纯函数行为：把 flow.js 里的布局函数抠出来喂参数真跑（node）；
//   ③ 真实渲染：headless Edge 里按 390×844 打开探针，验证"宽度铺满 + 相邻间隙为 0"。
//      没有 Edge 的机器自动 skip —— 这条只在能跑的机器上提供证据。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------- ① 静态闸门 ----------

func Test2429SettingsWiring(t *testing.T) {
	cfgSrc := readSourceOrSkip(t, "config.go")
	if !strings.Contains(cfgSrc, `MobileFlowView bool `+"`json:\"mobileFlowView\"`") {
		t.Error("★ Settings 缺 MobileFlowView 字段（json 名必须是 mobileFlowView）")
	}
	// ⚠️ 不能写死空格数：gofmt 会按字段名宽度重新对齐（改成 7 个空格也会让断言假红）
	defTrue := false
	for _, line := range strings.Split(cfgSrc, "\n") {
		if strings.Contains(line, "MobileFlowView:") && strings.Contains(line, "true") {
			defTrue = true
			break
		}
	}
	if !defTrue {
		t.Error("★ defaultSettings 必须默认 true（用户拍板：默认开）")
	}
	if !strings.Contains(cfgSrc, `present["mobileFlowView"]`) {
		t.Error("★ mergeAbsent 必须保护这个字段：老前端保存时不带它，不能被悄悄关成 false")
	}

	html := readSourceOrSkip(t, filepath.Join("web", "index.html"))
	if !strings.Contains(html, `id="setMobileFlow"`) {
		t.Error("★ 设置面板缺 id=\"setMobileFlow\" 的开关")
	}
	if !strings.Contains(html, `id="rowMobileFlow"`) {
		t.Error("★ 设置面板缺 id=\"rowMobileFlow\" 的行容器（桌面要隐藏它）")
	}

	// ⚠️ 先去掉行注释再断言：否则"代码里删了、注释里还留着"会让闸门假绿
	app := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "app.js")))
	checks := []struct{ needle, msg string }{
		{"mobileFlowView: true", "★ app.js cfg 默认值缺 mobileFlowView（默认必须开）"},
		{"cfg.mobileFlowView = s.mobileFlowView !== false", "★ applySettings 必须容忍缺字段（缺 = 开）"},
		{"cfg.mobileFlowView !== false", "★ fillSettingsForm 必须回填开关"},
		{"mobileFlowView: $('setMobileFlow')", "★ saveSettings 必须提交这个字段"},
		{"flow.js?v=__MV_VERSION__", "★ flow.js 必须在触屏档动态注入（带版本号防缓存）"},
		{"window.__mvFlowHost", "★ 必须给 flow.js 注入 host 契约（URL/设置/回网格）"},
		{"flowOpened = mf.open(v.list, target.path)", "★ 打开查看器必须分流到竖图流，且按 **path** 定位"},
		{"window.MVFlow.close()", "★ 关闭查看器必须收掉竖图流"},
		{"state.items[fi].path === flowPath", "★ 回网格定位必须按 path 反查（流内部会重排，下标已不同坐标）"},
		{"scrollToItem(gi, true)", "★ 退出流后必须滚回并高亮那一张"},
		{"function scrollToItem(", "★ 缺 scrollToItem 实现"},
		{"typeof flowOpen === 'function' && flowOpen()", "★ 手势入口必须短路（否则点一下会切图）"},
		{"!flowOpen()", "★ 捏合必须对 flow 报'查看器没开'（否则双指会抢滚动）"},
	}
	for _, c := range checks {
		if !strings.Contains(app, c.needle) {
			t.Error(c.msg)
		}
	}
	// 注入必须在 PROFILE === 'touch' 分支里（桌面不许下载这个文件）
	i := strings.Index(app, "flow.js?v=__MV_VERSION__")
	j := strings.LastIndex(app[:i], "PROFILE === 'touch'")
	if i < 0 || j < 0 || i-j > 400 {
		t.Error("★ flow.js 的注入必须紧跟在 PROFILE === 'touch' 分支内（桌面不该下载）")
	}

	css := readSourceOrSkip(t, filepath.Join("web", "style.css"))
	for _, needle := range []string{".vf-root", ".vf-item", ".vf-full.vf-ok", "#rowMobileFlow", "@media (pointer: coarse)"} {
		if !strings.Contains(css, needle) {
			t.Errorf("★ style.css 缺 %q", needle)
		}
	}
	if strings.Contains(css, "#rowMobileFlow { display: flex; }") &&
		!strings.Contains(css, "#rowMobileFlow { display: none; }") {
		t.Error("★ #rowMobileFlow 必须默认隐藏、只在 pointer:coarse 下显示")
	}

	flow := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "flow.js")))
	for _, needle := range []string{
		"window.MVFlow", "vfItemHeight", "vfIndexAt", "vfWindow",
		// ⚠️ 这几个必须用"代码字面量"而不是裸词：flow.js 的**块注释**里也写了
		//    &prefetch=1 / decode() / maxdim，裸词断言会被注释骗过（实测假绿过一次）。
		"'&prefetch=1'",                          // 预载必须带后端既有的预取标记（不抢前台槽）
		"HOST.rawURL(it.path, HOST.pickMaxdim()", // 画质沿用大图浏览那套设置
		"img.decode()",                           // 解码完成再露面
		"el.style.height = st.h[i]",              // 整数高度（无缝）
		// 2.4.30：比例的来源必须是大图那套（/api/meta），不再靠缩略图当探针
		"HOST.ensureSize(", "requestSize(",
	} {
		if !strings.Contains(flow, needle) {
			t.Errorf("★ flow.js 缺关键实现 %q", needle)
		}
	}

	// ---------- 2.4.30：图片分支不许再碰缩略图 ----------
	// 2.4.29 曾拿 /api/thumb 同时当"占位画面"和"比例探针"，真机上表现为
	// 「看到的是缩略图」「比例全是占位那个 3:4 → 图被裁」。缩略图只允许出现在视频封面帧那一支。
	videoBranch := segmentBetween(flow, "if (it.kind === 'video') {", "} else {")
	if !strings.Contains(videoBranch, "HOST.thumbURL") {
		t.Error("★ 视频封面帧应该用缩略图（视频没有静态大图可取）—— 尺寸来源被误删了？")
	}
	imgBranch := segmentBetween(flow, "var img = document.createElement('img');", "insertInOrder(i, el);")
	if imgBranch == "" {
		t.Fatal("★ 找不到图片分支（flow.js 结构变了，闸门失效）")
	}
	for _, bad := range []string{"HOST.thumbURL", "HOST.thumbSize", "vf-lqip"} {
		if strings.Contains(imgBranch, bad) {
			t.Errorf("★ 图片分支里出现了 %q —— 竖图流必须只用大图（rawURL），缩略图会糊/比例会错", bad)
		}
	}

	// 样式：object-fit 必须是 contain（cover 会把图裁成占位比例 —— 用户报的"都截成 4:3"）
	if !strings.Contains(css, "object-fit: contain") {
		t.Error("★ .vf-item img 必须 object-fit: contain（cover 会裁图）")
	}
	if strings.Contains(css, ".vf-lqip") || strings.Contains(css, "filter: blur(10px)") {
		t.Error("★ .vf-lqip / 模糊占位必须删掉（那正是\"看到的是缩略图\"的来源）")
	}
}

// ---------- ② 纯函数行为（node 真跑） ----------

func Test2429FlowMathBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var src = fs.readFileSync('web/flow.js', 'utf8');

// 从源码里抠出一个函数声明（按大括号配平，注释里的括号也能吃掉）
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = src.indexOf('{', i), depth = 0;
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
  console.error('FAIL 大括号不配平 ' + name); process.exit(1);
}

var DEFAULT_RATIO = 0.75;
eval(grab('vfItemHeight'));
eval(grab('vfIndexAt'));
eval(grab('vfWindow'));

function eq(a, b, m) {
  var sa = JSON.stringify(a), sb = JSON.stringify(b);
  if (sa !== sb) { console.error('FAIL ' + m + ' | expect ' + sb + ' got ' + sa); process.exit(1); }
}

// 高度：390 宽下各比例（无缝的前提是"整数"且与 spacer 一致）
eq(vfItemHeight(0.75, 390), 520, '3:4 → 390/0.75=520');
eq(vfItemHeight(16/9, 390), 219, '16:9 → 219');
eq(vfItemHeight(1, 390), 390, '1:1 → 390');
eq(vfItemHeight(2/3, 390), 585, '2:3 → 585');
eq(vfItemHeight(0, 390), 520, '比例未知 → 用 3:4 占位');
eq(vfItemHeight(0.75, 0), 1, '宽度 0 不能除以 0 出 Infinity/NaN');

// 索引：pre 是每张的顶边（含末尾 total）
var pre = [0, 520, 739, 1129, 1714, 2407];
eq(vfIndexAt(0, pre, 5), 0, 'y=0 → 第 0 张');
eq(vfIndexAt(519, pre, 5), 0, 'y 在首张内');
eq(vfIndexAt(520, pre, 5), 1, 'y 正好落在第二张顶边 → 第 1 张');
eq(vfIndexAt(1128, pre, 5), 2, '边界前一像素仍是第 2 张');
eq(vfIndexAt(1e9, pre, 5), 4, '超出末尾 → 最后一张（不能越界）');
eq(vfIndexAt(-5, pre, 5), 0, '负数 → 第 0 张');

// 窗口：可见 [first,last] 前后各留 before/after
eq(vfWindow(0, 0, 8, 1, 2), [0, 3], '首屏：上不越界，下留 2 张');
eq(vfWindow(6, 7, 8, 1, 2), [5, 8], '末尾：下不越界');
eq(vfWindow(3, 5, 8, 1, 2), [2, 8], '中间');
eq(vfWindow(0, 0, 0, 1, 2), [0, 0], '空列表');

console.log('OK');
`
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未通过：\n%s", out)
	}
}

// ---------- ③ 真实渲染（headless Edge，可选） ----------

func Test2429FlowRealRender(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := os.Getenv("MV_EDGE")
	if edge == "" {
		if runtime.GOOS == "windows" {
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
	}

	cmd := exec.Command(node, filepath.Join("..", "tools", "flow_probe.js"))
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("探针运行失败：%v", err)
	}
	var res struct {
		Open   bool        `json:"open"`
		At0    flowMetrics `json:"at0"`
		At1500 flowMetrics `json:"at1500"`
		TotalH int         `json:"totalHeight"`
		Closed int         `json:"closedIdx"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}
	if !res.Open {
		t.Fatal("MVFlow.open 返回 false")
	}
	// 宽度：每张都必须正好等于滚动容器宽度（"约束到手机宽度、左右不留白"）
	for _, m := range []flowMetrics{res.At0, res.At1500} {
		if m.ClientW <= 0 {
			t.Fatalf("容器宽度量不到：%+v", m)
		}
		for _, w := range m.Widths {
			if w != float64(m.ClientW) {
				t.Errorf("★ 图宽 %.1f ≠ 容器宽 %d（左右留白了）", w, m.ClientW)
			}
		}
		// 无缝：相邻两张 rect 的 gap 必须严格为 0
		for _, g := range m.Gaps {
			if g != 0 {
				t.Errorf("★ 相邻两张有 %.2fpx 的缝（必须为 0）", g)
			}
		}
		// 2.4.30：盒子比例必须等于图片自身比例 —— 不等就是被裁/被拉伸
		//（2.4.29 用 3:4 占位 + object-fit:cover，把 16:9 和 9:16 全裁成了 4:3）
		if len(m.BoxRatios) != len(m.ImgRatios) {
			t.Fatalf("比例数组长度不一致：%+v", m)
		}
		for k := range m.BoxRatios {
			br, ir := m.BoxRatios[k], m.ImgRatios[k]
			if ir <= 0 {
				t.Errorf("★ 第 %d 张拿不到图片自身比例（naturalWidth=0？）", k)
				continue
			}
			if d := br - ir; d > 0.01 || d < -0.01 {
				t.Errorf("★ 第 %d 张被裁了：盒子比例 %.4f ≠ 图片比例 %.4f（应 contain 原样）", k, br, ir)
			}
		}
		if len(m.Fits) > 0 {
			for _, f := range m.Fits {
				if f != "contain" {
					t.Errorf("★ object-fit 应为 contain，实际 %q（会裁图）", f)
				}
			}
		}
		if m.Mounted == 0 || len(m.Indexes) == 0 {
			t.Error("没有挂载任何图")
		}
	}
	// 窗口化：滚动之后挂载的序号必须换过（不是一次性全建出来）
	if len(res.At0.Indexes) == 0 || len(res.At1500.Indexes) == 0 {
		t.Fatal("索引为空")
	}
	if res.At0.Indexes[0] == res.At1500.Indexes[0] {
		t.Error("★ 滚动后挂载窗口没变 —— 窗口化没生效（长目录会堆爆 DOM）")
	}
	// 关闭必须回报"看到第几张"
	if res.Closed < 0 {
		t.Errorf("★ close() 没有回报索引：%d", res.Closed)
	}
}

type flowMetrics struct {
	ClientW   int       `json:"clientW"`
	Mounted   int       `json:"mounted"`
	Indexes   []int     `json:"indexes"`
	Widths    []float64 `json:"widths"`
	Heights   []float64 `json:"heights"`
	BoxRatios []float64 `json:"boxRatios"`
	ImgRatios []float64 `json:"imgRatios"`
	Fits      []string  `json:"fits"`
	Gaps      []float64 `json:"gaps"`
	Idx       int       `json:"idx"`
	Hud       string    `json:"hud"`
	TargetTop int       `json:"targetTop"`
	// 2.4.44：目标张是否居中（正的 = 图中心偏下），以及这张在几何上能不能居中
	Vh              int  `json:"vh"`
	TargetCenterOff int  `json:"targetCenterOff"`
	TargetCanCenter bool `json:"targetCanCenter"`
}
