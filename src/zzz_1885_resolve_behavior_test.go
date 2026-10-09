package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Test1885ResolveSortBehaviorExec 用 node **真实执行** resolveSortForDir，验证取排序的行为。
//
// 为什么必须"执行"而不是 grep 源码：只看字符串存在的话，有人在这段前面插一句
// `return Promise.resolve(fb);`（= 永远不读偏好）也能通过 —— 变异测试 M2 就是这么漏的。
//
// 断言的行为（与 1.8.165 一致，也是"文件管理器打开大图排序正确"的关键）：
//  1. 偏好读取器给出 'mtim:desc' → 大图必须用 **mtime:desc**（别名要归一）
//  2. 偏好为 null              → 大图回退 'name:asc'（资源管理器默认视图）
//  3. 偏好为 null              → 网格回退 state.sort
//  4. 读取器缺失              → 大图 'name:asc' / 网格 state.sort
//  5. 必须**真的调用**了 getDirSort，且把目录路径传进去
func Test1885ResolveSortBehaviorExec(t *testing.T) {
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
function build(win, st) {
  var code = grab('sortParts') + '\n' + grab('sortStr') + '\n' +
             grab('normalizeSortParam') + '\n' + grab('resolveSortForDir') +
             '\nreturn resolveSortForDir;';
  return new Function('window', 'state', code)(win, st);
}
function eq(a, b, msg) {
  if (a !== b) {
    console.error('FAIL', msg, 'expect', JSON.stringify(b), 'got', JSON.stringify(a));
    process.exit(1);
  }
}
var st = { sort: 'size:asc' };
var withPref = build({ FnOsSort: { getDirSort: function () {
  return Promise.resolve('mtim:desc'); } } }, st);
var prefNull = build({ FnOsSort: { getDirSort: function () {
  return Promise.resolve(null); } } }, st);
var noReader = build({}, st);
// 独立计数器：专门验证"真的调用了 getDirSort"
var seen = [];
var counter = build({ FnOsSort: { getDirSort: function (p) {
  seen.push(p); return Promise.resolve(null); } } }, st);
var steps = [];
steps.push(withPref('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'mtime:desc', '有偏好时大图必须用偏好（mtim→mtime 归一）');
}));
steps.push(prefNull('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'name:asc', '偏好为 null 时大图应回退 name:asc');
}));
steps.push(prefNull('/vol1/1000/pics', false).then(function (v) {
  eq(v, 'size:asc', '偏好为 null 时网格应用 state.sort');
}));
steps.push(noReader('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'name:asc', '读取器缺失时大图应回退 name:asc');
}));
steps.push(noReader('/vol1/1000/pics', false).then(function (v) {
  eq(v, 'size:asc', '读取器缺失时网格应用 state.sort');
}));
steps.push(counter('/only/once', true).then(function () {
  eq(seen.length, 1, '必须真的调用 getDirSort（提前 return 就会漏掉偏好）');
  eq(seen[0], '/only/once', 'getDirSort 必须收到目录路径');
}));
Promise.all(steps).then(function () { console.log('OK'); }, function (e) {
  console.error('ERR', e); process.exit(1); });
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("resolveSortForDir 行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test1885PrefReaderContract 偏好读取器必须读的是飞牛前端真正写的那份库。
//
// 已在其前端 bundle /usr/trim/www/assets/openDefaultApps-*.js 中确认：
//
//	createObjectStore('file-view-preferences',
//	    {keyPath:['userId','scope','surface','location']})
//
// 即库名 trim-file-manager、表名 file-view-preferences，字段 sortField/sortType。
func Test1885PrefReaderContract(t *testing.T) {
	js := readSourceOrSkip(t, "web/fnos_sort.js")
	s := stripJSComments(js)
	for _, want := range []string{
		"trim-file-manager",
		"file-view-preferences",
		"indexedDB",
		"sortField",
		"sortType",
		"getDirSort",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("fnos_sort.js 缺少 %s —— 读不到飞牛前端写的偏好，大图只能回退 name:asc", want)
		}
	}
}
