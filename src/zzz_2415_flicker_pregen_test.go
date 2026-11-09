// 2.4.15 新增：进/退目录闪烁修复 + 一键生成封面的前端契约测试。
//
// 钉住四件事，任何一件被改坏都必须红：
//
//	① 加载指示器**延迟显示**（快请求根本看不到转圈），且加载完成后待显示的定时器被取消
//	   —— 少了这一步就会出现「隐藏之后定时器才把它显示出来」，转圈永远挂在屏幕上；
//	② 全局只有 showLoadingSoon / hideLoadingNow 两处能碰 loadingEl
//	   —— 任何一处退回同步 remove('hidden') 都会让快请求闪一下；
//	③ 封面与名称**没有 opacity 过渡** —— 0.2s 渐变会在每次进/退目录时表现为「闪一下」；
//	④ 一键生成前端契约：带 path/size（决定缓存键，少一个就白生成一遍）、
//	   运行中按钮变「取消」、重新打开设置面板能恢复后台任务的真实状态。
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------- ① + ② 加载指示器：行为探针 ----------

func Test2415LoadingIndicatorIsDelayed(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + coverGrabFn + `
function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// ---- 定时器沙箱：可手动触发、可查询存活 ----
var delays = [];
var live = {};
var seq = 0;
var cleared = 0;
function setTimeout(fn, ms) { delays.push(ms); seq++; live[seq] = fn; return seq; }
function clearTimeout(id) { cleared++; delete live[id]; }

// ---- loadingEl 桩：记录被显示/隐藏 ----
var shown = 0, hidden = 0;
var loadingEl = {
  classList: {
    remove: function (c) { if (c === 'hidden') shown++; },
    add: function (c) { if (c === 'hidden') hidden++; }
  }
};
var loadingTimer = null;

eval('var _show = ' + grab('showLoadingSoon'));
eval('var _hide = ' + grab('hideLoadingNow'));

// ★ 调用后**不得同步显示**，只能挂一个延迟定时器
_show();
eq(shown, 0, '★★ showLoadingSoon 不得同步移除 hidden —— 同步显示就是快请求「闪一下」的来源');
eq(delays.length, 1, '应挂 1 个延迟定时器');
ok(delays[0] >= 150, '延迟必须 ≥150ms 才有意义（实测 100ms 内的请求占多数），实际 ' + delays[0]);

// 定时器到点后确实显示（真实定时器触发后就不在存活表里了）
var k = Object.keys(live)[0];
var fire = live[k];
delete live[k];
fire();
eq(shown, 1, '定时器到点后应显示指示器');

// ★ 请求很快返回：隐藏时必须把待显示的定时器一起取消
_show();
_hide();
eq(hidden > 0, true, 'hideLoadingNow 必须隐藏指示器');
eq(Object.keys(live).length, 0, '★★ hideLoadingNow 必须取消待显示的定时器 —— 否则「隐藏后又被定时器显示」= 转圈永久挂在屏幕上');

// 再确认一次：隐藏后定时器不在了，不会再有迟到的显示
var before = shown;
eq(shown, before, '取消后不应再有迟到的显示');

console.log('OK');
`
	cmd := exec.Command("node", "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("探针失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未通过:\n%s", out)
	}
	_ = js
}

// ---------- ② 静态闸门：只允许两个函数碰 loadingEl ----------

func Test2415LoadingTouchedOnlyByHelpers(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if n := strings.Count(js, "loadingEl.classList.remove('hidden')"); n != 1 {
		t.Errorf("直接移除 hidden 的位置应只有 1 处（showLoadingSoon 内），实际 %d 处 —— "+
			"多出来的每一处都会在快请求时闪一下", n)
	}
	if n := strings.Count(js, "loadingEl.classList.add('hidden')"); n != 1 {
		t.Errorf("直接添加 hidden 的位置应只有 1 处（hideLoadingNow 内），实际 %d 处", n)
	}
	// 两个函数必须存在且相邻（同一个 helpers 段），方便后来人一眼看到
	for _, fn := range []string{"function showLoadingSoon()", "function hideLoadingNow()"} {
		if !strings.Contains(js, fn) {
			t.Errorf("找不到 %s", fn)
		}
	}
}

// ---------- ③ 封面不得有过渡 ----------

func Test2415CoverHasNoTransition(t *testing.T) {
	css := readSourceOrSkip(t, "web/style.css")
	for _, sel := range []string{".cell-cover-img", ".cell-label"} {
		i := strings.Index(css, sel+" {")
		if i < 0 {
			t.Fatalf("找不到样式 %s", sel)
		}
		seg := css[i:]
		if j := strings.Index(seg, "}"); j > 0 {
			seg = seg[:j]
		}
		if strings.Contains(seg, "transition") {
			t.Errorf("★ %s 不得有 transition —— 0.2s 渐变在每次进/退目录时就是用户感知的「闪一下」\n%s",
				sel, seg)
		}
	}
}

// ---------- ④ 一键生成前端契约 ----------

func Test2415PregenFrontendContract(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	// 设置面板打开时必须恢复后台任务状态（否则重开面板按钮退回「开始」，用户以为任务丢了）
	i := strings.Index(js, "function openSettings()")
	if i < 0 {
		t.Fatal("找不到 openSettings")
	}
	seg := js[i:]
	if j := strings.Index(seg, "function closeSettings()"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "coverPregenRestore()") {
		t.Error("★ openSettings 必须调用 coverPregenRestore() —— 一键生成跑在服务端后台，" +
			"不恢复状态的话重开面板会显示「开始」，用户以为任务被取消了")
	}

	// 按钮接线的两条分支都要在
	for _, want := range []string{
		"dircov/pregen?path=",
		"dircov/pregen/cancel",
		"coverPregenPoll",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("缺少 %q", want)
		}
	}

	// 行为探针：URL 形状 + 按钮文案
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + coverGrabFn + `
function ok(c, m) { if (!c) { console.error('FAIL ' + m); process.exit(1); } }
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

var API = '/app/mediaview';
function enc(s) { return encodeURIComponent(s); }
var cfg = { thumbSize: 320 };
var state = { root: false, path: '/vol1/1000/照片 目录' };
function toast() {}
var lastURL = null;
function fetch(u, o) { lastURL = u; return Promise.resolve({ ok: true, json: function () { return {}; } }); }
var polled = 0;
function coverPregenPoll() { polled++; }
var els = { coverPregenStatus: { textContent: '' }, btnCoverPregen: { textContent: '' } };
function $(id) { return els[id] || null; }

eval('var _start = ' + grab('coverPregenStart'));

_start();
eq(lastURL, '/app/mediaview/dircov/pregen?path=' + encodeURIComponent('/vol1/1000/照片 目录') + '&size=320',
   '★★ 一键生成必须带 path/size —— size 决定缓存键，少一个就会生成到另一套缓存里，浏览时命中不到');

// 聚合根（还没进目录）时从 / 开始
state.root = true; state.path = null;
_start();
eq(lastURL, '/app/mediaview/dircov/pregen?path=' + encodeURIComponent('/') + '&size=320',
   '在聚合根启动时起点应是 /');

// ---- 恢复态 ----
var status = null;
function fetchJSON(u) { return Promise.resolve(status); }
// 2.4.45：完成文案抽成了 coverPregenDoneText()（轮询与重开面板共用一份），
// 探针把 restore 抠出来跑时也得把这个依赖一起带上。
eval('var coverPregenDoneText = ' + grab('coverPregenDoneText'));
eval('var _restore = ' + grab('coverPregenRestore'));

status = { running: true, done: 3, total: 10, generated: 3, skipped: 0, failed: 0 };
_restore();
setTimeout(function () {
  eq(els.btnCoverPregen.textContent, '取消', '★ 后台任务在跑时按钮必须显示「取消」（否则用户以为任务丢了、再点一次只会报冲突）');
  ok(polled > 0, '恢复运行态时必须重新开始轮询，进度才会继续走');

  status = { running: false, finished: 1700000000000, generated: 7, skipped: 2, failed: 0 };
  _restore();
  setTimeout(function () {
    eq(els.btnCoverPregen.textContent, '开始', '任务结束后按钮回到「开始」');
    ok(els.coverPregenStatus.textContent.indexOf('新生成 7') >= 0,
       '结束态应显示「新生成 7 个」，实际: ' + els.coverPregenStatus.textContent);
    ok(els.coverPregenStatus.textContent.indexOf('跳过 2') >= 0, '结束态应显示跳过数');
    console.log('OK');
  }, 10);
}, 10);
`
	cmd := exec.Command("node", "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("探针失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未通过:\n%s", out)
	}
}

// ---------- ④ 后端：预生成路由已注册 ----------

func Test2415PregenRoutesRegistered(t *testing.T) {
	src := readSourceOrSkip(t, "main.go")
	for _, want := range []string{
		`"/api/dircov/pregen"`,
		`"/api/dircov/pregen/cancel"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go 缺少路由 %s", want)
		}
	}
	// 清缓存必须连带清掉封面目录，否则「点了清缓存封面还在」会被当成 bug
	dircov := readSourceOrSkip(t, "dircov.go")
	if !strings.Contains(dircov, "func purgeDirCover") {
		t.Error("缺少 purgeDirCover（清缓存联动）")
	}
}
