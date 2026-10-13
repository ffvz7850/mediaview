package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// =============================================================
// 2.4.12 —— 从飞牛文件管理器双击打开大图"点第 1 张显示 100/100"根治
//
// 用户真实环境诊断数据：
//   · 飞牛全局默认：my-files name:desc（location 为空）
//   · 目标目录【国模套图】钛合金TiTi... 无目录级记录
//   · 用户在飞牛里按"文件名升序"浏览（第 1 张 = 0001.jpg），双击进大图
//   · 大图却按 name:desc（全局默认）排 ⇒ 0001.jpg 排最后 ⇒ 显示 100/100
//
// 修复：
//   1) normDir 必须 URL 解码（看图窗口 path 参数是 %2Fvol2%2F... 编码串，
//      不解码永远匹配不上 IndexedDB 里解码后的 location）
//   2) parseFnOsViewPreference **不再采用全局默认**（location 为空的记录），
//      只认目录级记录；无目录级记录时由调用方兜底 name:asc
// =============================================================

// Test2412NormDirUrlDecode normDir 必须把 URL 编码的路径解码成真实路径。
// 旧实现不解码：中文目录（尤其带《》等字符）在 IndexedDB 里是解码路径，
// 看图窗口 URL 是编码路径，永远匹配不上 ⇒ 只能退全局默认 ⇒ 100/100。
func Test2412NormDirUrlDecode(t *testing.T) {
	js := readSourceOrSkip(t, "web/fnos_sort.js")
	if !strings.Contains(js, "decodeURIComponent") {
		t.Fatal("normDir 缺少 decodeURIComponent —— 编码路径永远匹配不上 IndexedDB 的 location")
	}
	// 静态：必须同时存在反斜杠归一与合并重复斜杠
	if !strings.Contains(js, "replace(/\\\\/g, '/')") {
		t.Error("normDir 缺少反斜杠归一")
	}
	if !strings.Contains(js, "replace(/\\/+/g, '/')") {
		t.Error("normDir 缺少合并重复斜杠")
	}
}

// Test2412NormDirUrlDecodeExec 用 node 真实执行 normDir，验证解码行为。
func Test2412NormDirUrlDecodeExec(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var src = fs.readFileSync('web/fnos_sort.js', 'utf8');
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('miss ' + name);
  var depth = 0, j = src.indexOf('{', i);
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
}
var code = grab('normDir') + '\nmodule.exports = normDir;';
var m = { exports: {} };
new Function('module', 'exports', code)(m, m.exports);
var nd = m.exports;
function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL', msg, 'expect', JSON.stringify(b), 'got', JSON.stringify(a)); process.exit(1); }
}
// 用户真实目录（编码前后）
var enc = 'vol2%2F1000%2F%E5%9B%BE%E5%BA%93%2Fphoto%2F%E6%9C%AA%E5%8E%8B%E7%BC%A92%E5%90%8E%E5%8E%8B%E7%89%88';
var dec = 'vol2/1000/图库/photo/未压缩2后压版';
eq(nd(enc), dec, 'URL 编码路径必须解码');
eq(nd('/' + dec + '/'), dec, '首尾斜杠去除');
eq(nd('vol2//1000///图库'), 'vol2/1000/图库', '重复斜杠合并');
eq(nd('vol2\\1000\\图库'), 'vol2/1000/图库', '反斜杠转正斜杠');
eq(nd(dec), dec, '已解码路径原样保留');
console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("normDir 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2412ParseSkipsGlobalDefault ★核心：用户场景复现。
// 记录里只有全局默认（location 空）+ 目标目录无记录 ⇒ 必须返回 null
// （而不是旧实现返回 name:desc —— 那正是 100/100 的成因）。
func Test2412ParseSkipsGlobalDefault(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var src = fs.readFileSync('web/fnos_sort.js', 'utf8');
function grab(name) {
  var i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('miss ' + name);
  var depth = 0, j = src.indexOf('{', i);
  for (var k = j; k < src.length; k++) {
    if (src[k] === '{') depth++;
    else if (src[k] === '}') { depth--; if (depth === 0) return src.slice(i, k + 1); }
  }
}
var fi = src.indexOf('var FIELD_MAP');
var fe = src.indexOf('};', fi) + 2;
var fieldMap = src.slice(fi, fe);
var code = fieldMap + '\n' + grab('normDir') + '\n' + grab('parseFnOsViewPreference') +
           '\nmodule.exports = parseFnOsViewPreference;';
var m = { exports: {} };
new Function('module', 'exports', code)(m, m.exports);
var parse = m.exports;
function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL', msg, 'expect', JSON.stringify(b), 'got', JSON.stringify(a)); process.exit(1); }
}
var now = Date.now();
function rec(location, scope, field, type) {
  return { location: location, scope: scope, surface: 'directory',
           sorting: { sortField: field, sortType: type }, expiresAt: now + 86400000 };
}
var target = 'vol2/1000/图库/photo/未压缩2后压版/【国模套图】钛合金TiTi《黑神话悟空·铁扇公主》';

// ★ 用户场景：只有全局默认 name:desc，目标目录无记录 ⇒ null（不再采用全局默认）
eq(parse([rec('', 'my-files', 'name', 'desc')], target), null,
   '只有全局默认时必须返回 null（采用它就会 100/100）');
// 全局默认 + 其它目录记录 ⇒ 目标目录仍为 null
eq(parse([rec('', 'my-files', 'name', 'desc'),
          rec('vol2/1000/图库/photo/未压缩4', 'my-files', 'mtim', 'asc')], target), null,
   '目标目录无记录不得采用全局默认或其它目录记录');
// 目标目录有目录级记录 ⇒ 必须用它（与顺序无关）
eq(parse([rec('', 'my-files', 'name', 'desc'),
          rec(target, 'my-files', 'mtim', 'desc'),
          rec('', 'my-files', 'size', 'desc')], target), 'mtime:desc',
   '目录级记录优先，且不被全局默认覆盖');
// 编码的 dir 参数也必须命中目录级记录（normDir 解码生效）
var encTarget = 'vol2%2F1000%2F%E5%9B%BE%E5%BA%93%2Fphoto%2F%E6%9C%AA%E5%8E%8B%E7%BC%A92%E5%90%8E%E5%8E%8B%E7%89%88%2F%E3%80%90%E5%9B%BD%E6%A8%A1%E5%A5%97%E5%9B%BE%E3%80%91%E9%92%9B%E5%90%88%E9%87%91TiTi%E3%80%8A%E9%BB%91%E7%A5%9E%E8%AF%9D%E6%82%9F%E7%A9%BA%C2%B7%E9%93%81%E6%89%87%E5%85%AC%E4%B8%BB%E3%80%8B';
eq(parse([rec(target, 'my-files', 'name', 'asc')], encTarget), 'name:asc',
   '编码 dir 必须命中目录级记录');
// 大小写/别名归一仍有效
eq(parse([rec(target, 'my-files', 'MTIM', 'DESC')], target), 'mtime:desc', '大写归一');
eq(parse([rec(target, 'my-files', 'btim', 'asc')], target), 'btime:asc', 'btim→btime');
// 未知字段忽略
eq(parse([rec(target, 'my-files', 'unknown', 'desc')], target), null, '未知字段');
// 其它 scope 的目录级记录：my-files 优先，其它 scope 兜底
eq(parse([rec(target, 'app-share-files', 'mtim', 'desc'),
          rec(target, 'my-files', 'name', 'asc')], target), 'name:asc', 'my-files 优先');
eq(parse([rec(target, 'app-share-files', 'mtim', 'desc')], target), 'mtime:desc', '其它 scope 兜底');
console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("parseFnOsViewPreference 探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2412ResolveKeepsNameAscFallback 修复后大图路径仍是
// 「目录级偏好 → 读不到 name:asc」，网格仍是 state.sort。
func Test2412ResolveKeepsNameAscFallback(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(b))
	if !strings.Contains(s, "if (!force) return Promise.resolve(state.sort);") {
		t.Error("resolveSortForDir 缺少网格短路（state.sort）")
	}
	if !strings.Contains(s, "window.FnOsSort.getDirSort(") {
		t.Error("resolveSortForDir 缺少读飞牛偏好（getDirSort）")
	}
	if !strings.Contains(s, "'name:asc'") {
		t.Error("resolveSortForDir 缺少 name:asc 兜底")
	}
}
