package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Test1888ResolveSortBehaviorExec 用 node **真实执行** resolveSortForDir。
//
// 为什么必须"执行"而不是 grep 源码（这次真被咬到了）：
//
//	1.8.183 的测试只检查字符串存在与位置，于是
//	  · "大图不读偏好"（去掉 getDirSort 那两行）→ 字符串检查仍通过 → 抓不住；
//	  · "大图改用 state.sort"（把 fb 改成 state.sort）→ 同样抓不住。
//	这两处都是用户真实踩过的坑（1.8.187 / 1.8.184），必须有行为断言钉住。
//
// 期望行为（1.8.188 = 1.8.183 的语义，用户验证过"别的目录都正常"）：
//
//	大图（force=true）→ 读 IndexedDB 偏好并归一（mtim→mtime）；偏好为空则 name:asc
//	网格（force 空）  → state.sort，且**不调用** getDirSort
func Test1888ResolveSortBehaviorExec(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	// 拼接用 ';' 而不是换行 —— 本脚本是 Go 的 raw string（反引号），
	// 里面写反斜杠+n 只会得到两个字符，嵌进 JS 源码就成了非法 token。
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
  var code = grab('sortParts') + ';' + grab('sortStr') + ';' +
             grab('normalizeSortParam') + ';' + grab('resolveSortForDir') +
             ';return resolveSortForDir;';
  return new Function('window', 'state', code)(win, st);
}
function eq(a, b, msg) {
  if (a !== b) {
    console.error('FAIL', msg, 'expect', JSON.stringify(b), 'got', JSON.stringify(a));
    process.exit(1);
  }
}
var st = { sort: 'size:asc' };
var seen = [];
var withPref = build({ FnOsSort: { getDirSort: function (p) {
  seen.push(p); return Promise.resolve('mtim:desc'); } } }, st);
var prefNull = build({ FnOsSort: { getDirSort: function () {
  return Promise.resolve(null); } } }, st);
var noReader = build({}, st);
var steps = [];
steps.push(withPref('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'mtime:desc', '大图必须用偏好（mtim→mtime 归一）—— 若改成 state.sort 或忽略偏好，这里会失败');
}));
steps.push(prefNull('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'name:asc', '偏好为空时大图回退 name:asc');
}));
steps.push(noReader('/vol1/1000/pics', true).then(function (v) {
  eq(v, 'name:asc', '读取器缺失时大图回退 name:asc');
}));
steps.push(prefNull('/vol1/1000/pics', false).then(function (v) {
  eq(v, 'size:asc', '网格必须用 state.sort');
}));
Promise.all(steps).then(function () {
  if (seen.length !== 1 || seen[0] !== '/vol1/1000/pics') {
    console.error('FAIL 大图必须恰好调用一次 getDirSort，实际=' + JSON.stringify(seen));
    process.exit(1);
  }
  console.log('OK');
}, function (e) { console.error('ERR', e); process.exit(1); });
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("resolveSortForDir 行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
