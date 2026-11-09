package main

// 2.4.41：竖图连续浏览必须**完整跟随五项排序 + 升降序**。
//
// 问题的形状（2.4.33 的审查报告已定案）：流自己不排序，只按传入数组的顺序铺；
// 而那个数组可能是
//   · 改完排序、列表还没回来时的**旧顺序**（网格还是旧的，state.items 没变）；
//   · 手机整页导航后先铺上的**旧快照**（快照里没记排序）。
// 两种情况的默认值都恰好是「修改时间降序」，所以观感就是"永远按修改时间降序"。
//
// 修法三条（缺一不可）：
//   ① 比较器只留一份，5 个字段 × 升降序都要能排（`resortLikeFnOs`）；
//   ② 流打开时**自己按当前规则排一遍**，并按 **path** 定位（重排后下标已不同坐标）；
//   ③ 关流返回 **path**，网格按 path 反查 —— 否则"点哪张都错位"。
// 另有 ④ 快照记排序：不是当前排序的快照直接不用（顺序正确优先于"秒开"）。

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func Test2441SortWiringStatic(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))
	flow := stripLineComments(mustReadWebFile(t, "flow.js"))

	// ① 比较器必须覆盖 5 个字段，不能再有"非 name/type 直接原样返回"的早退
	sortBody := segmentBetween(app, "function resortLikeFnOs(list, sort) {", "\n  function normalizeSortParam(")
	if sortBody == "" {
		t.Fatal("找不到 resortLikeFnOs")
	}
	if strings.Contains(sortBody, "p.field !== 'name' && p.field !== 'type'") {
		t.Error("★★ resortLikeFnOs 仍对非 name/type 直接返回原列表 —— 大小/时间/创建时间三类排序就不会生效，" +
			"竖图流拿到什么顺序就铺什么顺序（这就是用户说的「永远按修改时间降序」）")
	}
	for _, need := range []string{"'size'", "'mtime'", "'btime'"} {
		if !strings.Contains(sortBody, need) {
			t.Errorf("★ 比较器缺字段 %s —— 五项排序里有一项不生效", need)
		}
	}
	if !strings.Contains(sortBody, "valid * 2 < list.length") {
		t.Error("★ btime 必须有「有效值不足一半就跳过」的保护：创建时间只在按它排序时才去 statx 取，" +
			"刚改完排序时列表里多数项没有 btime，全当 0 排会排成一坨乱序")
	}
	if !strings.Contains(sortBody, "return a - b;") {
		t.Error("★ 同键必须保持原顺序（返回 a-b）：后端是 sort.SliceStable，加了名称兜底会让网格与流排出两个顺序")
	}

	// ② host 必须暴露两个只读方法，且"当前规则"取 state.sort（**不是** state.currentSort）
	hostBody := segmentBetween(app, "window.__mvFlowHost = {", "window.dispatchEvent(new Event('mv-flow-host'))")
	for _, need := range []string{"sortList:", "currentSort:"} {
		if !strings.Contains(hostBody, need) {
			t.Errorf("★ host 缺 %s —— 流无法按当前规则自排", need)
		}
	}
	if !strings.Contains(hostBody, "return state.sort;") {
		t.Error("★ currentSort 必须返回 state.sort（工具栏上的值，也是网格这次请求实际用的值）；" +
			"state.currentSort 是「上一次请求的产物」，列表还在飞时它代表旧请求")
	}

	// ③ flow 自排 + 按 path 定位 + close 返回 path
	if !strings.Contains(flow, "HOST.sortList(") {
		t.Error("★★ flow 打开时必须调用 HOST.sortList 自己排一遍 —— 不排就只能相信传入数组的顺序")
	}
	prepBody := segmentBetween(flow, "function prepareList(list, start) {", "  function open(list, start) {")
	if !strings.Contains(prepBody, "typeof start === 'string'") {
		t.Error("★ prepareList 必须支持按 **path** 定位（重排之后下标已经不是同一套坐标）")
	}
	if !strings.Contains(prepBody, "media[k].path === start") {
		t.Error("★ prepareList 必须真的按 path 找到那一项")
	}
	if !strings.Contains(flow, "close() {") || !strings.Contains(flow, "return outPath;") {
		t.Error("★ close() 必须返回 **path** —— 返回下标的话，app 侧按旧坐标去滚网格，会「点哪张都错位」")
	}

	// ④ app 侧：按 path 开流、按 path 收回
	if !strings.Contains(app, "mf.open(v.list, target.path)") {
		t.Error("★ 打开竖图流必须传 target.path")
	}
	if strings.Contains(app, "state.items.indexOf(v.list[flowIdx])") {
		t.Error("★★ 还在用「v.list 下标 → state.items 下标」换算 —— 流内部重排后两边下标已不同坐标，必须改按 path 反查")
	}
	if !strings.Contains(app, "state.items[fi].path === flowPath") {
		t.Error("★ 关流后必须按 path 反查网格位置")
	}

	// ⑤ 快照记排序 + 校验
	saveBody := segmentBetween(app, "function saveSnap(", "  // 离开目录前把滚动位置写回它自己的快照")
	if !strings.Contains(saveBody, "sort: sort || ''") {
		t.Error("★ saveSnap 必须存排序：不存的话旧快照会把旧顺序先铺上来，流再从它里面取列表 —— 又是旧顺序")
	}
	loadBody := segmentBetween(app, "function loadSnap(p, sort) {", "  function saveSnap(")
	if !strings.Contains(loadBody, "o.sort !== sort") {
		t.Error("★ loadSnap 必须校验排序：排序不一致的快照不许用")
	}
	if strings.Contains(app, "loadSnap(path)") || strings.Contains(app, "loadSnap('/')") ||
		strings.Contains(app, "loadSnap(dirParam)") {
		t.Error("★ 还有 loadSnap 调用没带当前排序 —— 那条路会用到旧排序的快照")
	}

	// ⑥ 虚拟滚动容器必须关掉浏览器的滚动锚定
	css := mustReadWebFile(t, "style.css")
	scrollRule := segmentBetween(css, ".vf-scroll {", "}")
	if !strings.Contains(scrollRule, "overflow-anchor: none") {
		t.Error("★★ .vf-scroll 必须 `overflow-anchor: none`：上下 spacer 的高度是按当前窗口" +
			"实时改写的，浏览器会把上方高度变化当成\"内容变了\"并自己补偿 scrollTop —— " +
			"实测打开第 7 张时被顶到第 8 张（点第 N 张打开在第 N±1 张）")
	}
}

// Test2441ComparatorFiveFields 用 node 真跑比较器：5 字段 × 升降序 + 稳定性 + btime 降级。
func Test2441ComparatorFiveFields(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	readSourceOrSkip(t, filepath.Join("web", "app.js"))

	script := `
var fs = require('fs');
var src = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = src.indexOf('{', i), depth = 0;
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
var code = 'var _zhCollator = null;' + grab('sortParts') + ';' + grab('sortStr') + ';' +
           grab('zhCollator') + ';' + grab('resortLikeFnOs') + ';return resortLikeFnOs;';
var resort = new Function('Intl', code)(Intl);
function eq(a, b, msg) {
  var x = JSON.stringify(a), y = JSON.stringify(b);
  if (x !== y) { console.error('FAIL ' + msg + '\n  expect ' + y + '\n  got    ' + x); process.exit(1); }
}
function names(l) { return l.map(function (x) { return x.name; }); }

// 数据集：名称序 a1..a6，mtime 与名称**正好相反**（a6 最新），size 也与名称相反
var D = [
  { name: 'a1.jpg', path: '/a1.jpg', size: 600, mtime: 1000, btime: 500 },
  { name: 'a2.jpg', path: '/a2.jpg', size: 500, mtime: 2000, btime: 400 },
  { name: 'a3.jpg', path: '/a3.jpg', size: 400, mtime: 3000, btime: 300 },
  { name: 'a4.jpg', path: '/a4.jpg', size: 300, mtime: 4000, btime: 200 },
  { name: 'a5.jpg', path: '/a5.jpg', size: 200, mtime: 5000, btime: 100 },
  { name: 'a6.jpg', path: '/a6.jpg', size: 100, mtime: 6000, btime: 50 }
];
var SHUF = [D[3], D[0], D[5], D[1], D[4], D[2]];   // 故意打乱

eq(names(resort(SHUF, 'name:asc')), ['a1.jpg','a2.jpg','a3.jpg','a4.jpg','a5.jpg','a6.jpg'], 'name:asc');
eq(names(resort(SHUF, 'name:desc')), ['a6.jpg','a5.jpg','a4.jpg','a3.jpg','a2.jpg','a1.jpg'], 'name:desc');
eq(names(resort(SHUF, 'mtime:asc')), ['a1.jpg','a2.jpg','a3.jpg','a4.jpg','a5.jpg','a6.jpg'], 'mtime:asc');
eq(names(resort(SHUF, 'mtime:desc')), ['a6.jpg','a5.jpg','a4.jpg','a3.jpg','a2.jpg','a1.jpg'], 'mtime:desc');
eq(names(resort(SHUF, 'size:asc')), ['a6.jpg','a5.jpg','a4.jpg','a3.jpg','a2.jpg','a1.jpg'], 'size:asc');
eq(names(resort(SHUF, 'size:desc')), ['a1.jpg','a2.jpg','a3.jpg','a4.jpg','a5.jpg','a6.jpg'], 'size:desc');
eq(names(resort(SHUF, 'btime:asc')), ['a6.jpg','a5.jpg','a4.jpg','a3.jpg','a2.jpg','a1.jpg'], 'btime:asc');
eq(names(resort(SHUF, 'btime:desc')), ['a1.jpg','a2.jpg','a3.jpg','a4.jpg','a5.jpg','a6.jpg'], 'btime:desc');

// 类型：扩展名
var T = [{name:'b.png'},{name:'a.jpg'},{name:'c.gif'},{name:'d.avif'}];
eq(names(resort(T, 'type:asc')), ['d.avif','c.gif','a.jpg','b.png'], 'type:asc');
eq(names(resort(T, 'type:desc')), ['b.png','a.jpg','c.gif','d.avif'], 'type:desc');

// 稳定性：同 mtime / 同 size / 同扩展名 必须保持原顺序（与后端 SliceStable 一致）
var S = [{name:'x.jpg',mtime:7,size:1},{name:'y.jpg',mtime:7,size:1},{name:'z.jpg',mtime:7,size:1}];
eq(names(resort(S, 'mtime:desc')), ['x.jpg','y.jpg','z.jpg'], '同 mtime 保持原序');
eq(names(resort(S, 'size:asc')), ['x.jpg','y.jpg','z.jpg'], '同 size 保持原序');
var T2 = [{name:'a.jpg'},{name:'b.jpg'},{name:'c.jpg'}];
eq(names(resort(T2, 'type:desc')), ['a.jpg','b.jpg','c.jpg'], '同扩展名保持原序');

// btime 缺值降级：多数项没有 btime ⇒ 保持原顺序（不排成一坨 0）
var NB = [{name:'p',mtime:1},{name:'q',mtime:2},{name:'r',mtime:3},{name:'s',btime:9}];
eq(names(resort(NB, 'btime:asc')), ['p','q','r','s'], 'btime 有效值不足一半时保持原序');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("比较器探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2441PrepareListSortsAndLocates 用 node 真跑 flow 的 prepareList：
// 传入**旧顺序**的数组也必须按当前规则排好，并且定位到"用户点的那一张"。
func Test2441PrepareListSortsAndLocates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	readSourceOrSkip(t, filepath.Join("web", "flow.js"))

	script := `
var fs = require('fs');
var src = fs.readFileSync('web/flow.js', 'utf8');
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = src.indexOf('{', i), depth = 0;
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
function eq(a, b, msg) {
  var x = JSON.stringify(a), y = JSON.stringify(b);
  if (x !== y) { console.error('FAIL ' + msg + '\n  expect ' + y + '\n  got    ' + x); process.exit(1); }
}
function mkPrep(host) {
  return new Function('HOST', grab('prepareList') + ';return prepareList;')(host);
}
function names(l) { return l.map(function (x) { return x.name; }); }

var OLD = [   // 旧顺序（mtime 降序：a6 最新）
  {name:'a6.jpg',path:'/a6.jpg'},{name:'a5.jpg',path:'/a5.jpg'},{name:'a4.jpg',path:'/a4.jpg'},
  {name:'a3.jpg',path:'/a3.jpg'},{name:'a2.jpg',path:'/a2.jpg'},{name:'a1.jpg',path:'/a1.jpg'}
];

// ① 当前规则是 name:asc：自排后必须是 a1..a6，且按 path 定位到被点的那张（a6 → 最后一个）
var prep = mkPrep({
  sortList: function (list, sort) {
    var out = list.slice().sort(function (x, y) { return x.name < y.name ? -1 : (x.name > y.name ? 1 : 0); });
    return sort === 'name:desc' ? out.reverse() : out;
  },
  currentSort: function () { return 'name:asc'; }
});
var r = prep(OLD, '/a6.jpg');
eq(names(r.media), ['a1.jpg','a2.jpg','a3.jpg','a4.jpg','a5.jpg','a6.jpg'], '必须按当前规则自排，不信传入顺序');
eq(r.idx, 5, '必须定位到用户点的那一张（a6 在 name:asc 里是最后一个）');

// ② 一个中等的：点 a3 → 下标 2
eq(prep(OLD, '/a3.jpg').idx, 2, 'path 定位（a3）');

// ③ path 不在列表里（列表刚好变了）⇒ 退回 0，不能崩、不能 -1
eq(prep(OLD, '/gone.jpg').idx, 0, '找不到 path 时退回 0');

// ④ 数值下标仍然认（探针/旧调用）
eq(prep(OLD, 2).idx, 2, '数字下标仍然可用');

// ⑤ host 没提供 sortList（老宿主/探针）⇒ 保持原顺序，仍然能开
var bare = mkPrep(null);
eq(names(bare(OLD, '/a5.jpg').media), ['a6.jpg','a5.jpg','a4.jpg','a3.jpg','a2.jpg','a1.jpg'], '无 sortList 时保持原序');
eq(bare(OLD, '/a5.jpg').idx, 1, '无 sortList 时按原顺序定位');

// ⑥ 越界下标要夹住
eq(prep(OLD, 99).idx, 5, '越界下标夹到末尾');
eq(prep(OLD, -3).idx, 0, '负数下标夹到 0');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("prepareList 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2441FlowFollowsToolbarSortRealBrowser 端到端：**改完排序立刻点图**（最常见的触发方式）。
//
// 探针 tools/flow_sort_e2e.js 自带假后端（/api/list 故意延迟 600ms），跑的是真实页面：
//
//	① 目录里 12 张图，名称 a1..a12，但 mtime 与名称**相反**（a12 最新）；
//	② 默认 mtime:desc 打开 ⇒ 网格 a12..a1；
//	③ 切到「按名称」后 **60ms** 就点第 6 个格子（旧顺序里是 a7，此时列表还没回来）；
//	④ 竖图流必须以 name:asc 铺开（第一张 a1），并且**停在 a7**（用户点的那张，新顺序里第 7 位）。
//
// 这一条同时钉住两件事：流自己会按当前规则排（否则第一张是 a12）、
// 以及按 path 定位（否则会按旧下标落到别人身上）。
func Test2441FlowFollowsToolbarSortRealBrowser(t *testing.T) {
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
			`C://Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C://Program Files\Microsoft\Edge\Application\msedge.exe`,
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

	cmd := exec.Command(node, filepath.Join("..", "tools", "flow_sort_e2e.js"))
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "MV_EDGE="+edge)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("探针运行失败：%v", err)
	}
	var res struct {
		Profile    string   `json:"profile"`
		GridBefore []string `json:"gridBefore"`
		Clicked    string   `json:"clicked"`
		FlowOpen   bool     `json:"flowOpen"`
		FlowIdx    int      `json:"flowIdx"`
		Current    string   `json:"current"`
		FirstAtTop string   `json:"firstAtTop"`
		GridAfter  []string `json:"gridAfter"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}

	// 前置：必须真的走到竖图流（pointer: coarse），否则后面全是空转
	if !strings.Contains(res.Profile, "\"coarse\":true") {
		t.Fatalf("探针没进触屏档，竖图流根本没开：%s", res.Profile)
	}
	if !res.FlowOpen {
		t.Fatal("竖图流没有打开")
	}
	// 前置：点击那一刻网格必须还是**旧顺序**（否则没复现到竞态，测了等于没测）
	if len(res.GridBefore) < 12 || res.GridBefore[0] != "a12.jpg" {
		t.Fatalf("点击前的网格不是旧顺序（mtime:desc）：%v", res.GridBefore)
	}
	if res.Clicked != "a7.jpg" {
		t.Fatalf("探针点错了格子：%s", res.Clicked)
	}

	// ① 流必须按当前排序铺开：第一张是 a1
	if res.FirstAtTop != "a1.jpg" {
		t.Errorf("★★ 竖图流第一张是 %q（应为 a1.jpg）—— 流没有按当前排序自排，"+
			"铺的还是旧顺序（mtime 降序 = a12 开头）", res.FirstAtTop)
	}
	// ② 必须停在用户点的那张（a7 在 name:asc 里是第 7 位 ⇒ 下标 6）
	if res.Current != "a7.jpg" {
		t.Errorf("★★ 打开后停在 %q（应为 a7.jpg）—— 用户点哪张就该看哪张", res.Current)
	}
	if res.FlowIdx != 6 {
		t.Errorf("★★ index=%d（应为 6）—— 定位按旧下标算会落到隔壁那张", res.FlowIdx)
	}
	// ③ 列表回来之后网格也要是新顺序
	if len(res.GridAfter) < 12 || res.GridAfter[0] != "a1.jpg" || res.GridAfter[11] != "a12.jpg" {
		t.Errorf("列表回来后网格顺序不对：%v", res.GridAfter)
	}
}
