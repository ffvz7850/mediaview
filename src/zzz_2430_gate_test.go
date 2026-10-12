package main

// 2.4.3 行为探针：isAggregateRoot 必须**真的跑对**。
//
// 为什么要行为测试而不是字符串断言：M4/M5 两个变异体（删掉 ?dir=/ 分支、
// 删掉"没有 dir"分支）都能让"字符串仍存在"的断言通过 —— 因为别的分支里也有同样的片段。
// 只有把函数抠出来喂进各种 URL 真跑，才抓得住。
//
// 同时补：closeViewer 里必须**自己**调用了 syncExitTips（不能靠别处的同名调用蒙混）。

import (
	"os/exec"
	"strings"
	"testing"
)

func Test2430IsAggregateRootBehavior(t *testing.T) {
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
var body = grab('isAggregateRoot');

function run(search, root, path) {
  var ctx = {
    state: { root: root, path: path },
    location: { search: search }
  };
  var fn = new Function('e', 'with (this) { return (' + body + ').call(this); }');
  return fn.call(ctx, {});
}

// 聚合根层的 URL 是**固定**的 —— 这几条必须判对
eq(run('', false, null), true, '从桌面图标进入（无参数）⇒ 聚合根');
eq(run('?dir=%2F', false, null), true, '显式 ?dir=/ ⇒ 聚合根（URL 里 dir 的值就是 /）');
eq(run('?_v=1', false, null), true, '只有无关参数 ⇒ 聚合根');
// 用户目录里 ⇒ 不是聚合根
eq(run('?dir=%2Fvol1%2F1000%2F%E7%85%A7%E7%89%87', false, '/vol1/1000/照片'), false,
   '用户目录里（?dir=/vol1/...）⇒ 不是聚合根');
eq(run('?dir=%2Fvol2%2F1000', false, '/vol2/1000'), false, '用户目录本身 ⇒ 不是聚合根');
// 单文件模式（独立看图窗口）⇒ 不是聚合根
eq(run('?path=%2Fvol1%2Fa.jpg', false, null), false, '单文件模式（?path=）⇒ 不是聚合根');
// 内存状态已就绪时以它为准
eq(run('?dir=%2Fvol1%2F1000', true, null), true, '内存状态说在根层 ⇒ 以它为准确认聚合根');
eq(run('', false, '/vol1/1000/x'), false, '内存状态说在子目录 ⇒ 不是聚合根');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("isAggregateRoot 行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

func Test2430CloseViewerResyncsGate(t *testing.T) {
	app := stripLineComments(mustReadWebFile(t, "app.js"))
	body := jsFuncBody(app, "function closeViewer(")
	if body == "" {
		t.Fatal("取不到 closeViewer 的函数体")
	}
	// ⚠️ 必须查**函数体内**：别处也有同样的调用，只做全文 Contains 会让
	//    "closeViewer 里删掉它"这种变异逃逸（M6 第一次就是这样）。
	if !strings.Contains(body, "syncExitTips()") {
		t.Error("★★ closeViewer 里必须**自己**调 syncExitTips() —— " +
			"关掉查看器后要重新判定闸门（若此时停在聚合根层，闸门要重新挂上）")
	}
}
