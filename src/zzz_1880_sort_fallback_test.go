package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Test1880SortFallbackUnified 1.8.183：网格与大图的排序来源**必须分开**。
//
//	网格（force 空）→ **只**用 state.sort（用户选的）。不能读飞牛偏好 ——
//	      1.8.181~182 读了，导致"排序按钮全部失效"（偏好一来就覆盖用户选择）。
//	大图（force=true）→ 读飞牛偏好，读不到回退 name:asc。
func Test1880SortFallbackUnified(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(b))
	// 网格路径必须先短路返回 state.sort，且在读偏好之前
	iShort := strings.Index(s, "if (!force) return Promise.resolve(state.sort);")
	iPref := strings.Index(s, "window.FnOsSort.getDirSort(")
	if iShort < 0 {
		t.Error("resolveSortForDir 缺少网格短路：`if (!force) return Promise.resolve(state.sort);`")
	} else if iPref > 0 && iShort > iPref {
		t.Error("网格短路在读取偏好**之后** —— 偏好会先覆盖用户选择（这正是按钮失效的原因）")
	}
	if iPref < 0 {
		t.Error("大图路径必须读飞牛偏好（window.FnOsSort.getDirSort）")
	}
	if !strings.Contains(s, "'name:asc'") {
		t.Error("大图路径缺少 name:asc 回退")
	}
}

// Test1880NoForceBranchInFallback 防止有人把两侧再合并成同一个来源。
func Test1880NoForceBranchInFallback(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(b))
	i := strings.Index(s, "function resolveSortForDir")
	if i < 0 {
		t.Fatal("找不到 resolveSortForDir")
	}
	tail := s[i:]
	if j := strings.Index(tail, "\n  function "); j > 0 {
		tail = tail[:j]
	}
	if !strings.Contains(tail, "state.sort") || !strings.Contains(tail, "'name:asc'") {
		t.Error("resolveSortForDir 必须同时保留两个来源：网格 state.sort、大图 name:asc")
	}
}

// Test1880GridSortControlReloads 排序控件改完必须重新加载目录，否则"点了没反应"。
func Test1880GridSortControlReloads(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(b))
	for _, anchor := range []string{"applySort(this.value, true);", "applySort(sortStr(p.field, !p.desc), true);"} {
		i := strings.Index(s, anchor)
		if i < 0 {
			t.Errorf("找不到排序控件调用点：%s", anchor)
			continue
		}
		tail := s[i:]
		if len(tail) > 200 {
			tail = tail[:200]
		}
		if !strings.Contains(tail, "loadDir(state.path)") {
			t.Errorf("排序控件 %s 之后没有 loadDir(state.path) —— 点排序不会重排网格", anchor)
		}
	}
}

// Test1883SortTypeCaseInsensitive 1.8.183：飞牛写的方向是**大写** DESC/ASC，解析必须大小写不敏感。
//
// 真实故障：只比小写 'desc' ⇒ 方向永远被当成 asc ⇒
// "文件管理器里按名称降序，大图却按升序切换"（顺序整个反了）。
func Test1883SortTypeCaseInsensitive(t *testing.T) {
	b, err := os.ReadFile("web/fnos_sort.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(b))
	if !strings.Contains(s, "String(r.sorting.sortField || '').toLowerCase()") {
		t.Error("fnos_sort.js 未对 sortField 做小写归一 —— 大写 MTIM 会被当作未知字段而忽略")
	}
	if !strings.Contains(s, ".sortType == null ? '' : r.sorting.sortType).toLowerCase()") {
		t.Error("fnos_sort.js 未对 sortType 做小写归一 —— 大写 DESC 会被当成 asc")
	}
	if strings.Contains(s, "sorting.sortType === 'desc'") {
		t.Error("仍在直接比较小写 'desc' —— 飞牛写的是大写 DESC，方向会永远变成 asc")
	}
}

// Test1883SortTypeExec 用 node 真实执行解析逻辑，验证 DESC/ASC 两种大小写都能被认出来。
func Test1883SortTypeExec(t *testing.T) {
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
function rec(field, type) {
  return [{ location: '/vol1/1000/pics', scope: 'my-files', surface: 'directory',
            sorting: { sortField: field, sortType: type } }];
}
function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL', msg, 'expect', JSON.stringify(b), 'got', JSON.stringify(a)); process.exit(1); }
}
// ★ 核心：大写 DESC 必须解析成 desc（旧代码在这里会得到 asc）
eq(parse(rec('mtim', 'DESC'), '/vol1/1000/pics'), 'mtime:desc', '大写 DESC 必须认');
eq(parse(rec('mtim', 'ASC'), '/vol1/1000/pics'), 'mtime:asc', '大写 ASC');
eq(parse(rec('mtim', 'desc'), '/vol1/1000/pics'), 'mtime:desc', '小写 desc 兼容');
eq(parse(rec('name', 'DESC'), '/vol1/1000/pics'), 'name:desc', '按名称降序');
eq(parse(rec('btim', 'DESC'), '/vol1/1000/pics'), 'btime:desc', 'btim→btime');
eq(parse(rec('MTIM', 'DESC'), '/vol1/1000/pics'), 'mtime:desc', '字段名大写也认');
eq(parse(rec('size', 'ASC'), '/vol1/1000/pics'), 'size:asc', 'size 升序');
eq(parse(rec('unknown', 'DESC'), '/vol1/1000/pics'), null, '未知字段回退');
console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("fnos_sort 解析探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
