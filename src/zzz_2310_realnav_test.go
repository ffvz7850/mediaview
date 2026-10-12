package main

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 2.3.1：触屏档的"进目录"必须是**真 URL 导航**（从 2.1.x 移植）
//
// 为什么：宿主的页面栈只有一层，且**不感知我们的 pushState**（2.1.9-probe 实测）。
// 只有让进目录变成真的页面导航，宿主才会跟着记账，返回手势才有"上一页"可退。
//
//	参数名必须是 dir —— `?path=` 已被"单文件模式"占用（会把目录当文件去 openSingle）。
//
// ---------------------------------------------------------------------------
func Test2310RealNavigationOnTouch(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))

	// 静态：两个函数必须在，且触屏档走 location.replace('?dir=…')
	// 2.4.1：href → **replace** —— href 每进一层都压一条真实 URL，守卫删不掉已有条目，
	// 于是"退到聚合根后继续返回"会恢复旧的 /vol{n}/1000/xxx 整页（用户实测报的就是这个）。
	if !strings.Contains(app, "function navigateTo(") || !strings.Contains(app, "function navigateRoot(") {
		t.Fatal("缺少 navigateTo / navigateRoot —— 触屏档的目录切换必须是真导航")
	}
	if !strings.Contains(app, "location.replace(GW + '/?dir=' + encodeURIComponent(path))") {
		t.Error("navigateTo 必须导航到 `GW + '/?dir=' + encodeURIComponent(path)` —— " +
			"参数名必须是 dir：`?path=` 已被单文件模式占用")
	}
	if !strings.Contains(app, "location.replace(GW + '/?dir=/')") {
		t.Error("navigateRoot 必须显式导航到 `?dir=/` —— " +
			"不带参数时 boot 会走 last 分支、从 localStorage 跳回上次目录（回不到最上层）")
	}
	// 三处入口都要换过去（否则真导航只做了一半）
	if !strings.Contains(app, "navigateTo(item.path)") {
		t.Error("点击目录格子必须走 navigateTo")
	}
	if !strings.Contains(app, "if (path === null) navigateRoot(); else navigateTo(path);") {
		t.Error("面包屑必须走 navigateTo / navigateRoot")
	}
	if !strings.Contains(app, "if (!up || up === '/') { navigateRoot(); return; }") ||
		!strings.Contains(app, "navigateTo(up);") {
		t.Error("goUpLevel 必须走 navigateTo / navigateRoot（否则进目录真导航、回上级内部切换，两条路会不一致）")
	}
	// boot 必须认 ?dir=
	if !strings.Contains(app, "var dirParam = qs.get('dir');") {
		t.Error("boot 必须支持 ?dir= —— 否则真导航落地后没人读它，页面会回到 last 目录")
	}
	if !strings.Contains(app, "if (dirParam === '/')") {
		t.Error("boot 必须把 ?dir=/ 当作「最上层」，不能落到 last 分支")
	}
	// 桌面档不许被牵连
	if !strings.Contains(app, "if (PROFILE === 'touch') {") {
		t.Error("真导航必须限定在触屏档（桌面档保持内部切换）")
	}

	// 2.3.2：左上角箭头必须**按状态分流** —— 查看器开着要关它，而不是走 goUpLevel→真导航。
	//   此前它直接绑 goUpLevel，于是「在大图里点箭头」会真导航到上层目录（整页重载），
	//   而查看器里只有系统返回才走 popstate → closeViewer。
	//   这正是"大图没加载完成时返回就退出软件"的来源。
	if !strings.Contains(app, "if (!viewer.classList.contains('hidden')) {") {
		t.Error("箭头必须判断查看器是否开着 —— 开着就关它（closeViewer），不能直接 goUpLevel")
	}
	if strings.Contains(app, "navBackBtn.onclick = goUpLevel;") {
		t.Error("箭头不许再直接绑 goUpLevel —— 那会让「在大图里点箭头」变成真导航换目录（整页重载）")
	}
	if !strings.Contains(app, "if (navBackBtn) navBackBtn.onclick = function () {") {
		t.Error("箭头必须绑定一个按状态分流的函数")
	}

	// 2.3.2：closeViewer 主动退历史时，必须让紧接着的 popstate **不再退层**。
	//   否则"点一次箭头 = 关查看器 + 回上级目录"，一次退两层
	//   （真 Edge 实测：URL 从 ?dir=/vol1 直接变成 ?dir=/）——
	//   这正是"大图没加载完返回就退出软件"的根源。
	if !strings.Contains(app, "var mvSelfBack = false;") {
		t.Error("必须有 mvSelfBack 标记 —— 用来区分'我们自己发的 back'与'宿主发的返回'")
	}
	if !strings.Contains(app, "if (mvSelfBack) { mvSelfBack = false; return; }") {
		t.Error("mvOnPop 必须在最前面识别 mvSelfBack —— 那时查看器已经关好了，只能补守卫、不能再退层")
	}
	if !strings.Contains(app, "mvSelfBack = true;") {
		t.Error("closeViewer 主动退历史前必须置 mvSelfBack")
	}

	// 行为：真跑 navigateTo，验触屏档导航、桌面档 loadDir
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
var code = grab('navigateTo') + ';' + grab('navigateRoot') + ';' +
  'return { to: navigateTo, root: navigateRoot };';
function makeEnv(profile) {
  // 2.4.1：真导航改用 location.replace（replace 不压历史条目 —— href 每层都压一条，
  //   守卫删不掉已有条目，于是"退到聚合根后继续返回"会恢复旧的 /vol{n}/... 整页）。
  var env = { hrefs: [], loads: [], roots: 0, replaces: 0,
    loc: {
      set href(v) { env.hrefs.push(v); },
      get href() { return env.hrefs[env.hrefs.length - 1]; },
      replace: function (v) { env.replaces++; env.hrefs.push(v); }
    } };
  var api = new Function('PROFILE', 'GW', 'location', 'loadDir', 'buildRoot', code)(
    profile, '/app/mediaview', env.loc,
    function (p) { env.loads.push(p); }, function () { env.roots++; });
  env.api = api;
  return env;
}
// 触屏档：必须真导航，且**不能**再调 loadDir（否则一次点击加载两遍）
var e1 = makeEnv('touch');
e1.api.to('/vol1/1000');
eq(e1.hrefs.join(','), '/app/mediaview/?dir=%2Fvol1%2F1000', '触屏档进目录必须是真导航（?dir= 编码后的路径）');
eq(e1.replaces, 1, '★★ 触屏档必须用 location.replace —— 用 href 会每层压一条历史，' +
  '于是退到聚合根后继续返回会恢复旧的 /vol{n}/... 整页（用户实测报的就是这个）');
eq(e1.replaces, 1, '★★ 触屏档必须用 location.replace —— 用 href 会每层压一条历史，' +
  '于是退到聚合根后继续返回会恢复旧的 /vol{n}/... 整页（用户实测报的就是这个）');
eq(e1.loads.length, 0, '触屏档已真导航，不得再走内部 loadDir');
e1.api.root();
eq(e1.hrefs[1], '/app/mediaview/?dir=/', '回最上层必须显式带 ?dir=/（否则 boot 会跳回上次目录）');

// 桌面档：保持内部切换，**不许**导航
var e2 = makeEnv('mouse');
e2.api.to('/vol1/1000');
eq(e2.hrefs.length, 0, '桌面档不得导航（多窗口/内部切换在桌面上是有意义的）');
eq(e2.loads.join(','), '/vol1/1000', '桌面档应走 loadDir');
e2.api.root();
eq(e2.roots, 1, '桌面档回最上层应走 buildRoot');
eq(e2.hrefs.length, 0, '桌面档全程不得导航');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("真导航探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
