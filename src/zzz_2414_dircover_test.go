// 2.4.14 新增：文件夹封面（/api/dircov）的前端契约测试。
//
// 钉住三件事，任何一件被改坏都必须红：
//
//	① dir 格子渲染成「封面 + 底部名称」结构（不再是 📁 内联 HTML）；
//	② coverURL 与 thumbURL 同构（含 size/_t/_v），清缓存/换目录都能跟着失效；
//	③ coverEnabled 设置项前后端接线完整（cfg / applySettings / saveSettings / fillSettingsForm）。
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// 抠出**具名函数**的完整源码（含签名与函数体），node 探针里把真身喂进去。
const coverGrabFn = `
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
`

// ---------- ① 静态闸门：dir 格子渲染结构 ----------

func Test2414DirCellRendersCover(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "if (item.kind === 'dir') {")
	if i < 0 {
		t.Fatal("找不到 dir 分支")
	}
	// 取到下一个 else-if 之前，就是 dir 分支的完整代码段
	seg := js[i:]
	if j := strings.Index(seg, "} else if (!cfg.thumbEnabled) {"); j > 0 {
		seg = seg[:j]
	}

	for _, want := range []string{
		"className = 'cell-cover'",
		"className = 'cell-fallback'",
		"className = 'cell-label'",
		"className = 'cell-thumb cell-cover-img'",
		"coverURL(item.path",
		"cfg.coverEnabled",
	} {
		if !strings.Contains(seg, want) {
			t.Errorf("dir 分支缺少关键结构 %q —— 封面格子退化回旧 📁 或漏接线都会命中这里", want)
		}
	}
	// 兜底：不能还残留旧的内联 📁 HTML
	if strings.Contains(seg, "font-size:42px;line-height:1;") {
		t.Error("dir 分支还残留旧的「📁 + 名称」内联 HTML —— 应改为 cell-cover 结构")
	}
	// 名称必须叠在封面底部（cell-label），而不是消失
	if !strings.Contains(seg, "cell-label-name") {
		t.Error("dir 分支缺少底部名称 cell-label-name —— 名称必须叠在封面底部")
	}
}

// ---------- ② 静态闸门：coverURL 形状 ----------

func Test2414CoverURLShape(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function coverURL(")
	if i < 0 {
		t.Fatal("找不到 coverURL 函数")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  }"); j > 0 {
		seg = seg[:j]
	}
	for _, want := range []string{
		"API + '/dircov?path=' + enc(p)",
		"'&size=' + (size || 320)",
		"'&_t=' + (mtime || 0)",
		"'&_v=' + coverCacheBust", // 2.4.48：封面独立计数器，清缩略图不再牵连封面
	} {
		if !strings.Contains(seg, want) {
			t.Errorf("coverURL 缺少 %q —— 少了它清缓存/换目录就无法让封面失效", want)
		}
	}
	// 2.4.26：缝色固定在服务端（coverSeamColor），URL 不再带 bg
	if strings.Contains(seg, "bg") {
		t.Error("★ coverURL 不应带 bg（缝色固定，与主题/请求参数无关）")
	}
}

// ---------- ③ 静态闸门：coverEnabled 前后端接线 ----------

func Test2414CoverSettingWired(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	// 默认值
	if !strings.Contains(js, "coverEnabled: true,") {
		t.Error("cfg 缺 coverEnabled 默认值 true")
	}
	// 读回（后端无此字段时保持开，避免升级即关闭）
	if !strings.Contains(js, "cfg.coverEnabled = s.coverEnabled !== false;") {
		t.Error("applySettings 没读 coverEnabled（用 !== false 兜底，升级用户不被静默关掉）")
	}
	// 表单回填
	if !strings.Contains(js, "$('setCover').checked = cfg.coverEnabled;") {
		t.Error("fillSettingsForm 没回填 setCover 复选框")
	}
	// 提交
	if !strings.Contains(js, "coverEnabled: $('setCover').checked,") {
		t.Error("saveSettings 提交体没带 coverEnabled")
	}
}

// ---------- ④ 行为：coverURL 的 URL 形状（2.4.26 起无 bg，缝色固定在服务端） ----------

func Test2414CoverURLBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + coverGrabFn + `
var coverURL = grab('coverURL');

// 环境桩：coverURL 依赖 API/enc/cfg/thumbCacheBust
var API = '/app/mediaview';
function enc(s) { return encodeURIComponent(s); }
var cfg = { thumbSize: 320 };
// 2.4.48：封面用 coverCacheBust，缩略图用 thumbCacheBust，两者必须解耦。
// 故意给缩略图一个不同的值，确保 coverURL 拿的是封面那个。
var thumbCacheBust = 9;
var coverCacheBust = 3;

// grab 返回的是「function 名(...) {...}」完整声明，直接 eval 声明即可
eval(coverURL);

function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// coverURL 完整形状：path 编码 + size + _t + _v（2.4.26 起无 bg，缝色固定在服务端）
var u = coverURL('/vol1/1000/图片 目录', 320, 123456);
eq(u, '/app/mediaview/dircov?path=' + encodeURIComponent('/vol1/1000/图片 目录') + '&size=320&_t=123456&_v=3', 'coverURL 形状（_v 必须取 coverCacheBust）');

// 清缩略图（thumbCacheBust++）不该改变封面 URL —— 封面没被删，就不该全量重下
thumbCacheBust++;
eq(coverURL('/vol1/1000/图片 目录', 320, 123456), u, '★ 清缩略图后封面 URL 必须保持不变');

console.log('OK');
`
	runNodeCoverProbe(t, node, script)
}

func runNodeCoverProbe(t *testing.T, node string, script string) {
	t.Helper()
	cmd := exec.Command(node, "-e", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 探针失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("node 探针输出不含 OK: %s", out)
	}
}
