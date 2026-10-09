package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Test1889FrontendResortLikeFnOs 1.8.190：前端必须用 Intl.Collator("zh") 按飞牛规则重排。
//
// 根因（实测）：飞牛对名称用 a.name.localeCompare(b.name, "zh")，ICU 的字符层级是
//
//	标点 < 数字 < 汉字（按拼音）< 小写 < 大写 < ...
//
// 而后端 naturalLess 是字节序，会把**所有汉字排到最后**：
//
//	NO.072（76 个）后端 ['DSC06619.jpg', …]（汉字第 76 位）
//	               飞牛 ['小吉的快乐野餐封面.jpg', 'DSC06619.jpg', …]（第 1 位）
//
// 为什么不在后端修：Go 的 x/text/collate(zh) 与 ICU 不一致（实测
// "A" vs "中"：Go 判 A 在前、Node 判 中 在前），且 ICU 层级细到标点 16 类 +
// 全角/希腊/西里尔/韩文/假名各有位置，手写复刻不现实。
func Test1889FrontendResortLikeFnOs(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	s := stripJSComments(js)
	for _, want := range []string{
		"new Intl.Collator('zh', { numeric: true })",
		"function resortLikeFnOs(",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js 缺少 %s —— 中文与拉丁的混排会与飞牛不一致", want)
		}
	}
	// 两个调用点都必须接上，否则网格与大图的顺序会不一致。
	// 断言**精确的调用串**而不是数次数：少接一处时次数仍然够（定义 + 另一处 = 3），
	// 数次数会漏（变异 M5 就是这么漏的）。
	if !strings.Contains(s, "dirs = resortLikeFnOs(dirs, effSort);") ||
		!strings.Contains(s, "files = resortLikeFnOs(files, effSort);") {
		t.Error("网格（loadDir）没有接上 resortLikeFnOs —— 网格顺序仍与飞牛不一致")
	}
	if !strings.Contains(s, "files = resortLikeFnOs(files, usedSort);") {
		t.Error("大图（openSingle）没有接上 resortLikeFnOs —— 大图与网格顺序会不一致")
	}
}

// Test1889ResortBehaviorExec 用 node 真实执行重排，验证与飞牛 localeCompare(zh) 一致。
func Test1889ResortBehaviorExec(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var src = fs.readFileSync('web/app.js', 'utf8');
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('miss ' + name);
  var depth = 0, j = src.indexOf('{', i);
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
  throw new Error('unbalanced ' + name);
}
var code = 'var _zhCollator = null;' + grab('sortParts') + ';' + grab('sortStr') + ';' +
           grab('zhCollator') + ';' + grab('resortLikeFnOs') + ';return resortLikeFnOs;';
var resort = new Function('Intl', code)(Intl);

function names(list) { return list.map(function (x) { return x.name; }); }
function eq(a, b, msg) {
  var x = JSON.stringify(a), y = JSON.stringify(b);
  if (x !== y) { console.error('FAIL', msg, '\n  expect', y, '\n  got   ', x); process.exit(1); }
}
function mk(arr) { return arr.map(function (n) { return { name: n }; }); }

// ★ 核心：汉字必须排在**数字之后、拉丁之前**（飞牛 ICU 的层级）
var mix = ['DSC06619.jpg', '小吉的快乐野餐封面.jpg', 'abc.jpg', '00069.png', '玛奇玛.jpg', 'Z.jpg', '阿.jpg'];
var want = ['00069.png', '阿.jpg', '玛奇玛.jpg', '小吉的快乐野餐封面.jpg', 'abc.jpg', 'DSC06619.jpg', 'Z.jpg'];
eq(names(resort(mk(mix), 'name:asc')), want, '混排顺序必须与飞牛 localeCompare(zh) 一致');

// 汉字按拼音（不是码点）
eq(names(resort(mk(['中', '小', '玛', '国', '波', '阿']), 'name:asc')),
   ['阿', '波', '国', '玛', '小', '中'], '汉字必须按拼音排');
// 大小写不敏感、同字母小写优先
eq(names(resort(mk(['B', 'a', 'A', 'b']), 'name:asc')), ['a', 'A', 'b', 'B'], '大小写规则');
// ★ 1.8.191：数字必须按**值**排（用户要求；飞牛后端是字符串序，这里刻意不同）
eq(names(resort(mk(['IMG_2.jpg', 'IMG_10.jpg']), 'name:asc')),
   ['IMG_2.jpg', 'IMG_10.jpg'], '数字必须按值排（2 在 10 前）');
eq(names(resort(mk(['1 (1).png', '1 (2).png', '1 (3).png', '1 (10).png', '1 (11).png', '1 (12).png']), 'name:asc')),
   ['1 (1).png', '1 (2).png', '1 (3).png', '1 (10).png', '1 (11).png', '1 (12).png'],
   '括号内数字按值（1 (1) 1 (2) … 1 (10)）');
eq(names(resort(mk(['1', '10', '100', '101', '102', '2', '20']), 'name:asc')),
   ['1', '2', '10', '20', '100', '101', '102'], '纯数字按值（1 2 10 20 100 101 102）');
eq(names(resort(mk(['Dsc033-1.jpg', 'Dsc033-2.jpg', 'Dsc033-9.jpg', 'Dsc033-10.jpg', 'Dsc033-85.jpg']), 'name:asc')),
   ['Dsc033-1.jpg', 'Dsc033-2.jpg', 'Dsc033-9.jpg', 'Dsc033-10.jpg', 'Dsc033-85.jpg'],
   'Dsc033 系列按值');
// 降序
eq(names(resort(mk(mix), 'name:desc')),
   ['Z.jpg', 'DSC06619.jpg', 'abc.jpg', '小吉的快乐野餐封面.jpg', '玛奇玛.jpg', '阿.jpg', '00069.png'],
   '降序必须生效');
// 非 name/type 字段不重排（保持后端顺序）
eq(names(resort(mk(['b', 'a']), 'size:asc')), ['b', 'a'], 'size 不重排');
// type：按扩展名
eq(names(resort(mk(['x.png', 'y.jpg', 'z.avif']), 'type:asc')), ['z.avif', 'y.jpg', 'x.png'],
   'type 按扩展名排');
// 单元素/空列表不炸
eq(names(resort(mk(['a']), 'name:asc')), ['a'], '单元素');
eq(names(resort([], 'name:asc')), [], '空列表');
console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("重排行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
