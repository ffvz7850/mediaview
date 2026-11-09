package main

// 2.4.40：换目录回到**真页面导航**，并钉住"返回不闪"的整套机制。
//
// 背景（三版试错的结论，别再走回头路）：
//   · 2.4.35 ~ 2.4.39 为了"换目录不重载"改成内部切换（SPA），于是整个会话只剩一条
//     历史，宿主的手势返回必然落到应用之外 —— 那五版都在"历史层加守卫"上打补丁，无一奏效。
//   · 现在换回真页面导航：每层目录 = 一条真实历史条目，返回的语义就是"回上一层页面"，
//     不需要任何守卫。"不闪"改由**目录快照**承担（sessionStorage 里存 items + 滚动位置，
//     新页面 boot 时不等任何接口先把画面铺出来）。
//
// 本文件守着三件事：
//   ① 导航真的走 location.href（不是 replace、不是内部 loadDir）；
//   ② 首条历史条目被改写成显式 ?dir=（否则从第一个文件夹返回会落回"没有目录"的页面）；
//   ③ 快照在 settings 之前就铺上、且在离开前记住滚动位置（"返回停在原处"）。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test2440NavigationIsRealPageNav(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	// ① 两个入口都必须是 location.href（不能是 replace —— 那会让整个会话只有一条历史）
	for _, fn := range []string{"function navigateTo(", "function navigateRoot("} {
		body := segmentBetween(app, fn, "  }")
		if body == "" {
			t.Fatalf("找不到 %s", fn)
		}
		if !strings.Contains(body, "location.href = GW + '/?dir=") {
			t.Errorf("★★ %s 必须用 location.href 真导航（每层一条历史）——"+
				"replace 只改当前条目，整个会话就只剩一条历史，返回必然退出应用", fn)
		}
		if strings.Contains(body, "location.replace(") {
			t.Errorf("%s 不许再出现 location.replace", fn)
		}
		if !strings.Contains(body, "rememberScroll();") {
			t.Errorf("★ %s 必须调 rememberScroll()：离开前把\"看到第几屏\"写进快照，"+
				"否则返回永远从顶部开始（比闪更难受）", fn)
		}
	}

	// ② 点击 / 面包屑 / 返回箭头 走同一条路径
	for _, need := range []string{
		"navigateTo(item.path)",
		"if (path === null) navigateRoot(); else navigateTo(path);",
		"if (!up || up === '/') { navigateRoot(); return; }",
		"navigateTo(up);",
	} {
		if !strings.Contains(app, need) {
			t.Errorf("★ 缺导航调用点 %q（进目录与回上级必须是同一条路径）", need)
		}
	}

	// ③ boot 必须认 ?dir=（真导航落地后没人读它，页面就会跳回 last 目录）
	if !strings.Contains(app, "var dirParam = qs.get('dir');") {
		t.Error("boot 必须读 `?dir=`")
	}
	if !strings.Contains(app, "if (dirParam === '/')") {
		t.Error("boot 必须把 `?dir=/` 当作「最上层」，不能落到 last 分支")
	}
	// ④ 首条条目归一化：没有 ?dir= 时必须用 replaceState 改成显式目录
	if !strings.Contains(app, "function fixEntryURL(dir) {") {
		t.Fatal("★ 缺 fixEntryURL —— 从桌面图标进来时 URL 没有 ?dir=，" +
			"那条历史条目就没有目录信息，从第一个文件夹返回会落回一个「猜目录」的页面（看起来像返回没反应）")
	}
	if !strings.Contains(app, "fixEntryURL(last);") || !strings.Contains(app, "fixEntryURL('/');") {
		t.Error("★ boot 的两个落点都要 fixEntryURL（恢复上次目录时写 last，回根层时写 /）")
	}

	// ⑤ 快照必须**早于** settings 铺上（那是"接口还没回、画面已经在"的全部秘密）
	iDir := strings.Index(app, "var dirParam = qs.get('dir');")
	if iDir < 0 {
		t.Fatal("找不到 boot 的 dirParam 分支")
	}
	rest := app[iDir:]
	iSnap := strings.Index(rest, "var earlySnap = loadSnap(dirParam, state.sort);")
	if iSnap < 0 {
		t.Fatalf("boot 的快照分支不见了（找不到 `var earlySnap = loadSnap(dirParam, state.sort);`）")
	}
	// ⚠️ 从快照那一行**往后**找 settings：`?dir=/` 分支在它之前就有一处 loadSettings，
	//    按全文件第一个出现位置比会误判（这一条是测试自己写错过一次的坑）。
	after := rest[iSnap:]
	iPaint := strings.Index(after, "applyItems(null, earlySnap.top || 0);")
	iSettings := strings.Index(after, "settingsReady = loadSettings();")
	if iSettings < 0 || iPaint < 0 {
		t.Fatalf("boot 的快照分支结构变了（iSettings=%d iPaint=%d）", iSettings, iPaint)
	}
	if iPaint > iSettings {
		t.Error("★★ 快照必须在 loadSettings() **之前**就铺上 —— 顺序反了就要等一个 RTT 才有画面，" +
			"那正是「返回闪一下」")
	}

	// ⑥ 滚回原处：快照必须存 top，rememberScroll 必须写 top
	snapBody := segmentBetween(app, "function saveSnap(", "  // 离开目录前把滚动位置写回它自己的快照")
	if !strings.Contains(snapBody, "top: top || 0") {
		t.Error("★ saveSnap 必须把滚动位置一起存（top 字段）")
	}
	rsBody := segmentBetween(app, "function rememberScroll() {", "  // sameItems")
	if !strings.Contains(rsBody, "scroller.scrollTop") || !strings.Contains(rsBody, ".top =") {
		t.Error("★ rememberScroll 必须把 scroller.scrollTop 写回该目录快照的 top")
	}
	if !strings.Contains(app, "window.addEventListener('pagehide', function (e) {\n    try { rememberScroll(); }") {
		t.Error("★ pagehide 里必须补一次 rememberScroll —— 宿主手势返回/前进不由我们发起，" +
			"不补就记不住滚动位置")
	}
}

// Test2440GoUpLevelStillWalksOut 用 node 真跑 goUpLevel：它必须只调导航，不碰 loadDir/buildRoot。
func Test2440GoUpLevelStillWalksOut(t *testing.T) {
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
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function mk(st) {
  var env = { nav: [], root: 0, loads: 0 };
  var api = new Function('state', 'navigateTo', 'navigateRoot', 'dirName', 'isCustomRoot',
    'isInsideAnyUserDir',
    grab('goUpLevel') + ';return goUpLevel;')(
    st,
    function (p) { env.nav.push(p); },
    function () { env.root++; },
    function (p) { var s = String(p).replace(/\/+$/, ''); var i = s.lastIndexOf('/'); return i <= 0 ? '/' : s.slice(0, i); },
    function () { return false; },
    function (p) { return /^\/vol\d+\/1000(\/|$)/.test(p); });
  env.api = api;
  return env;
}

// ① 聚合根层：箭头**什么都不做**（不弹框、不退出、也不重建）
var e1 = mk({ root: true, path: null });
e1.api();
eq(e1.nav.length + e1.root, 0, '根层按返回箭头必须无动作');

// ② 深层目录：回上一层目录，且走的是 navigateTo（真导航）
var e2 = mk({ root: false, path: '/vol1/1000/照片/2026/春节' });
e2.api();
eq(e2.nav.join(','), '/vol1/1000/照片/2026', '应回到上一层目录');
eq(e2.root, 0, '深层回上级不许跳到聚合根');

// ③ /vol1/1000 的上一层是 /vol1（存储空间层）⇒ 必须直接回聚合根，不许出现存储空间页
var e3 = mk({ root: false, path: '/vol1/1000' });
e3.api();
eq(e3.root, 1, '/vol1/1000 再往上必须回聚合根');
eq(e3.nav.length, 0, '/vol1/1000 再往上不许导航到 /vol1');

// ④ 再上一层：/vol1/1000/A → /vol1/1000（走 navigateTo）
var e4 = mk({ root: false, path: '/vol1/1000/A' });
e4.api();
eq(e4.nav.join(','), '/vol1/1000', '/vol1/1000/A 应回 /vol1/1000');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("goUpLevel 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
