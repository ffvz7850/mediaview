package main

// 2.4.31：修「进/退文件夹会闪一下」。
//
// 两条链路各有一处根因：
//   ① 桌面档（内部换目录）：layout() 删完格子只 requestRender()，重建要等**下一帧**的
//      rAF ⇒ 中间那一帧是"格子全没了"的空画面（真实 app 实测 n=0 跨 1 帧）。
//      修法：layout 末尾同步 renderNow()。
//   ② 手机档（整页导航，见 navigateTo）：新页面首屏必须等 /api/list 才有内容。
//      修法：sessionStorage 目录快照 —— boot 里不等 settings 先铺，loadDir 里比对
//      `sameItems`，一致就**完全不重建**。
//
// 这里钉的是"修法还在"，以及 sameItems 的语义（它一旦写错，快照会盖住真实变化）。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test2431RenderSameTaskAndSnap(t *testing.T) {
	app := stripLineComments(readSourceOrSkip(t, filepath.Join("web", "app.js")))

	// ---------- ① layout 必须同步重建 ----------
	if !strings.Contains(app, "renderNow();") {
		t.Error("★ layout 末尾必须调 renderNow()：只排 rAF 会让重建跨到下一帧，中间那帧就是空白闪")
	}
	if !strings.Contains(app, "function renderNow()") {
		t.Error("★ 缺 renderNow 实现（同步重建）")
	}
	if !strings.Contains(app, "cancelAnimationFrame(rafId)") {
		t.Error("★ renderNow 必须先撤掉已排队的那一帧，否则同一份内容会被跑两遍")
	}
	// 提防有人"顺手"改回 requestRender：layout 里必须没有它
	layoutBody := segmentBetween(app, "function layout(keepScroll) {", "function requestRender()")
	if layoutBody == "" {
		t.Fatal("★ 找不到 layout 函数体（结构变了，闸门失效）")
	}
	if strings.Contains(layoutBody, "requestRender()") {
		t.Error("★ layout 里不许用 requestRender()（会跨帧 → 闪）；要用 renderNow()")
	}

	// ---------- ② 目录快照 ----------
	for _, need := range []string{
		"function loadSnap(", "function saveSnap(", "function sameItems(",
		"'mv_snap:' + p",                  // 键名带目录（否则相邻目录互相污染）
		"SNAP_MAX_DIRS", "SNAP_MAX_ITEMS", // 有上限（不能把 sessionStorage 撑爆）
	} {
		if !strings.Contains(app, need) {
			t.Errorf("★ 缺 %q", need)
		}
	}
	// loadDir 里：命中快照要先铺、真实列表回来一致就不重建
	// 2.4.38：变了也**不再** layout()（那会清池整屏重建），改走增量 applyItems
	if !strings.Contains(app, "applyItems(null, keepTop);") {
		t.Error("★ 列表回来必须走增量 applyItems(null, keepTop)（不能整池重建，否则还是闪）")
	}
	if strings.Contains(app, "if (!unchanged) layout();") {
		t.Error("★ 不许退回 layout()（它会把 cellPool 整池销毁）—— 用 applyItems 做增量")
	}
	if strings.Contains(layoutBody, "removeChild") {
		t.Error("★ layout/applyItems 里不许整池 removeChild：整屏内容在同一帧里\"全消失再全出现\"就是闪")
	}
	if !strings.Contains(app, "var unchanged = sameItems(state.items, nextItems);") {
		t.Error("★ 必须用 sameItems 判断内容是否真的变了")
	}
	// boot 的 ?dir= 分支：不等 settings 先铺快照（这是"接口没回画面已在"的关键）
	// 全局只此一处（只有 boot 的 ?dir= 分支会这么写）
	if !strings.Contains(app, "loadSnap(dirParam, state.sort)") {
		t.Error("★ boot 的 ?dir= 分支必须先用 loadSnap 铺内容（等 settings 就慢一个 RTT）")
	}

	// ---------- ③ loading 阈值 ----------
	if strings.Contains(app, "}, 180);") {
		t.Error("★ 加载指示的阈值不能是 180ms（NAS 上常在 200~350ms 返回 → 转圈闪一下）")
	}
	if !strings.Contains(app, "}, 400);") {
		t.Error("★ 加载指示阈值应为 400ms")
	}
}

func Test2431SameItemsBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
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
  console.error('FAIL 大括号不配平'); process.exit(1);
}
eval(grab('sameItems'));

function eq(a, b, m) {
  if (JSON.stringify(a) !== JSON.stringify(b)) {
    console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a));
    process.exit(1);
  }
}

var a = [{ path: '/p/1.jpg', mtime: 100, size: 10 }, { path: '/p/2.jpg', mtime: 200, size: 20 }];
eq(sameItems(a, a.slice()), true, '同一份内容（新数组）必须判一致');
eq(sameItems(a, [{ path: '/p/1.jpg', mtime: 100, size: 10 }]), false, '长度不同 → 不一致');
eq(sameItems(a, [
  { path: '/p/1.jpg', mtime: 100, size: 10 },
  { path: '/p/2.jpg', mtime: 201, size: 20 }
]), false, 'mtime 变了必须判不一致（否则快照会盖住新文件）');
eq(sameItems(a, [
  { path: '/p/1.jpg', mtime: 100, size: 10 },
  { path: '/p/2.jpg', mtime: 200, size: 21 }
]), false, 'size 变了必须判不一致');
eq(sameItems(a, [
  { path: '/p/2.jpg', mtime: 200, size: 20 },
  { path: '/p/1.jpg', mtime: 100, size: 10 }
]), false, '顺序不同必须判不一致（排序变了要重建）');
eq(sameItems(null, a), false, 'null 安全');
eq(sameItems([], []), true, '两个空列表判一致');

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
