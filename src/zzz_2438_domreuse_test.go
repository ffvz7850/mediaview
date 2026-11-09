package main

// 2.4.38：网格 DOM 复用 —— "文件夹打开 / 回退不闪"的核心机制。
//
// 2.4.37 之前的 layout() 第一件事是把 cellPool 整池 removeChild，然后同步重建 20+ 个 <img>
// （src 从 1×1 占位重新开始）。即使 HTTP 缓存命中，也要重新解码、重新 paint ——
// 整屏内容在同一帧里"全部消失再全部出现"，用户看到的就是闪一下。
//
// 现在改成：
//   · layout() → applyItems()，**不清池**；
//   · renderVisible() 按 el.dataset.mvPath / mvKey 判断「复用 / 就地刷新 / 单格替换」；
//   · cellKey 刻意**不含 mtime**（/api/list 每次回来的精度可能不同，算进来会让
//     "内容没变"被判成"全变了"，于是每次刷新都整屏重建）。
//
// 本测试用**真实的 renderVisible / cellKey** 在最小 DOM 桩里跑，数 createCell 的调用次数。

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- ① 静态闸门 ----------

func Test2438ReuseStatic(t *testing.T) {
	src := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "app.js")))

	if !strings.Contains(src, "function cellKey(it) {") {
		t.Fatal("★ 缺 cellKey（单元格内容指纹）")
	}
	keyBody := segmentBetween(src, "function cellKey(it) {", "function applyItems(")
	if strings.Contains(keyBody, "mtime") {
		t.Error("★★ cellKey 不许包含 mtime：/api/list 的 mtime 精度可能变化，" +
			"算进来会让\"内容没变\"被判成\"全变了\"，触发整屏重建 —— 那就是闪烁")
	}
	if !strings.Contains(keyBody, "name") || !strings.Contains(keyBody, "size") {
		t.Error("★ cellKey 至少要含 name 与 size")
	}

	if !strings.Contains(src, "function applyItems(") {
		t.Fatal("★ 缺 applyItems（增量应用）")
	}
	applyBody := segmentBetween(src, "function applyItems(", "function requestRender()")
	if strings.Contains(applyBody, "removeChild") {
		t.Error("★★ applyItems 不许整池 removeChild —— 那正是\"整屏内容全消失再全出现\"的来源")
	}
	if !strings.Contains(applyBody, "renderNow()") {
		t.Error("★ applyItems 必须同步 renderNow()（排到 rAF 会跨帧 = 闪）")
	}

	// createCell 必须打上内容身份证
	if !strings.Contains(src, "el.dataset.mvPath = item.path;") {
		t.Error("★ createCell 必须写 el.dataset.mvPath（renderVisible 靠它判断能否复用）")
	}
	if !strings.Contains(src, "el.dataset.mvKey = cellKey(item);") {
		t.Error("★ createCell 必须写 el.dataset.mvKey")
	}

	// renderVisible 的三条分支：新建 / 单格替换 / 复用
	rv := segmentBetween(src, "function renderVisible() {", "function scrollToItem(")
	for _, need := range []string{
		"el.dataset.mvPath !== item.path",
		"replaceChild(fresh, el)",
		"refreshCellThumb(el, item)",
		"loadCellImage(el, eager)",
	} {
		if !strings.Contains(rv, need) {
			t.Errorf("★ renderVisible 缺分支 %q", need)
		}
	}

	// 2.4.40：换目录改回真页面导航 —— 旧文档整个被丢弃，浏览器自己会 abort 掉在途请求。
	//   "先取消、再补发"这套只为「同一个文档里换内容」存在，必须删净：
	//   半留半删比不删更危险（两套机制并存，谁也说不出哪条在生效）。
	for _, dead := range []string{"cancelPendingGridRequests", "resumeCanceledThumbs"} {
		if strings.Contains(src, dead) {
			t.Errorf("★★ %s 是 SPA 时代为「同一文档内换目录」准备的，真导航后已无意义 —— 请删净", dead)
		}
	}
}

// ---------- ② 行为：用真实 renderVisible 数重建次数 ----------

func Test2438ReuseBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	readSourceOrSkip(t, filepath.Join("web", "app.js"))

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

// 最小 DOM 桩：只实现 renderVisible 用到的那几个方法
function mkEl() {
  var el = { dataset: {}, style: {}, parentNode: null, querySelector: function () { return null; } };
  return el;
}
var content = {
  children: [],
  appendChild: function (el) { el.parentNode = content; content.children.push(el); return el; },
  replaceChild: function (nv, ov) {
    var i = content.children.indexOf(ov);
    if (i < 0) { return ov; }
    content.children[i] = nv; nv.parentNode = content; ov.parentNode = null;
    return ov;
  },
  removeChild: function (el) {
    var i = content.children.indexOf(el);
    if (i >= 0) content.children.splice(i, 1);
    el.parentNode = null;
    return el;
  }
};

var state = { items: [], cols: 3, cell: 100, gap: 8 };
var scroller = { scrollTop: 0, clientHeight: 600 };
var cellPool = {};
var created = 0;
var loaded = 0;
var refreshed = 0;
var CELLKEY = null;   // 由 new Function 返回后赋值（createCell 只在 render 之后才被调用）

function createCell(item, idx) {
  created++;
  var el = mkEl();
  el.dataset.mvPath = item.path;
  el.dataset.mvKind = item.kind || '';
  el.dataset.mvKey = CELLKEY(item);
  return el;
}
function loadCellImage() { loaded++; }
function refreshCellThumb() { refreshed++; }

var body = grab('cellKey') + '\n' + grab('renderVisible') + '\n' +
  'return { render: renderVisible, key: cellKey };';
var api = new Function('state', 'scroller', 'cellPool', 'content',
  'createCell', 'loadCellImage', 'refreshCellThumb', body)(
  state, scroller, cellPool, content, createCell, loadCellImage, refreshCellThumb);
CELLKEY = api.key;

function mkItems(n, tag, mtime) {
  var a = [];
  for (var i = 0; i < n; i++) {
    a.push({ kind: 'image', name: 'p' + i + '.jpg', path: '/' + tag + '/p' + i + '.jpg', size: 1000 + i, mtime: mtime });
  }
  return a;
}

function run(items) {
  var c0 = created, r0 = refreshed;
  state.items = items;
  api.render();
  return { created: created - c0, refreshed: refreshed - r0 };
}

var r1 = run(mkItems(9, 'a', 111));            // 首次：全建
var r2 = run(mkItems(9, 'a', 111));            // 完全相同：零建
var r3 = run(mkItems(9, 'a', 999));            // 只 mtime 变：零建（cellKey 不含 mtime）
var r4 = run(mkItems(9, 'b', 111));            // 换目录：全换
// 换目录后把原目录换回来，验证池子仍然能正确复用（不会因为旧元素残留而错位）
var r5 = run(mkItems(9, 'a', 111));

console.log(JSON.stringify({
  first: r1.created, same: r2.created, mtimeOnly: r3.created,
  otherDir: r4.created, backAgain: r5.created,
  domCount: content.children.length,
  keyIgnoresMtime: api.key({ kind: 'image', name: 'x', size: 1, mtime: 1 }) ===
                   api.key({ kind: 'image', name: 'x', size: 1, mtime: 2 })
}));
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("DOM 复用模拟失败：%v\n%s", err, out)
	}
	var got struct {
		First           int  `json:"first"`
		Same            int  `json:"same"`
		MtimeOnly       int  `json:"mtimeOnly"`
		OtherDir        int  `json:"otherDir"`
		BackAgain       int  `json:"backAgain"`
		DomCount        int  `json:"domCount"`
		KeyIgnoresMtime bool `json:"keyIgnoresMtime"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("解析失败：%v\n%s", err, out)
	}

	if got.First != 9 {
		t.Errorf("首次渲染应建 9 个格子，实际 %d", got.First)
	}
	if got.Same != 0 {
		t.Errorf("★★ 内容完全没变却重建了 %d 个格子 —— 这就是\"打开/切回来闪一下\"", got.Same)
	}
	if got.MtimeOnly != 0 {
		t.Errorf("★★ 只有 mtime 变化就重建了 %d 个格子 —— cellKey 又把 mtime 算进去了", got.MtimeOnly)
	}
	if got.OtherDir != 9 {
		t.Errorf("换了目录应当替换 9 格，实际 %d", got.OtherDir)
	}
	if got.BackAgain != 9 {
		t.Errorf("切回原目录应替换 9 格，实际 %d", got.BackAgain)
	}
	if !got.KeyIgnoresMtime {
		t.Error("★ cellKey 必须忽略 mtime")
	}
	if got.DomCount != 9 {
		t.Errorf("★ DOM 数量应恒为 9（虚拟滚动窗口），实际 %d —— 有泄漏", got.DomCount)
	}
}
