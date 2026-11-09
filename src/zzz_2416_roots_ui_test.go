// 2.4.16 新增：「只显示我添加的文件夹」的前端契约测试。
//
// 钉住四件事：
//
//	① 设置项三个控件（开关 / 列表 / 添加按钮）都存在，且读写链路完整；
//	② 列表是**工作副本**语义：增删只改 pendingRoots，点保存才提交；
//	③ 保存后若可见范围变了，必须**回首页重新取数**（只重绘会把界面留在已失效的目录里）；
//	④ 启动时开启该选项就不恢复上次目录（否则会先吃一个 403 再回退）。
package main

import (
	"os/exec"
	"strings"
	"testing"
)

func Test2416RootsUIPresent(t *testing.T) {
	html := readSourceOrSkip(t, "web/index.html")
	for _, id := range []string{`id="setOnlyRoots"`, `id="rootList"`, `id="btnAddRoot"`, `id="rootHint"`} {
		if !strings.Contains(html, id) {
			t.Errorf("设置页缺少控件 %s", id)
		}
	}
	if !strings.Contains(html, "既不显示也不会被读取") {
		t.Error("开关的说明文案要写清「也不读取」，否则用户以为只是隐藏")
	}
}

func Test2416RootsWiring(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	cases := []struct{ frag, why string }{
		{"cfg.onlyRoots = !!s.onlyRoots", "applySettings 必须把 onlyRoots 读进 cfg"},
		{"cfg.roots = Array.isArray(s.roots) ? s.roots.slice() : []", "applySettings 必须读 roots"},
		{"cfg.rootsInfo = Array.isArray(s.rootsInfo) ? s.rootsInfo : []", "applySettings 必须读 rootsInfo（用于标出失效目录）"},
		{"$('setOnlyRoots').checked = cfg.onlyRoots", "fillSettingsForm 必须回填开关"},
		{"pendingRoots = (cfg.roots || []).slice()", "fillSettingsForm 必须初始化工作副本"},
		{"onlyRoots: $('setOnlyRoots').checked", "saveSettings 必须提交开关状态"},
		{"roots: pendingRoots.slice()", "saveSettings 必须提交工作副本"},
		{"if (dbPickTarget === 'root')", "目录选择器必须分流到「添加文件夹」"},
	}
	for _, c := range cases {
		if !strings.Contains(js, c.frag) {
			t.Errorf("缺少：%s（%s）", c.frag, c.why)
		}
	}
}

// Test2416RootsSyncOnToggle 切换选项后必须回首页重新取数。
//
// 只 redraw 是不够的：当前目录可能已经不在可见范围内，服务端会对它的请求返回 403，
// 而界面还停在那里 —— 表现就是"设置改了、界面没变"。
func Test2416RootsSyncOnToggle(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if !strings.Contains(js, "prevOnly !== cfg.onlyRoots") {
		t.Error("★ 保存后必须比较 onlyRoots 是否变化，变了就要回首页重新取数")
	}
	if !strings.Contains(js, "prevRoots !== (cfg.roots || []).join('|')") {
		t.Error("★ 保存后必须比较 roots 列表是否变化")
	}
	// 变化分支里必须是 buildRoot()，不能只是 redraw()
	i := strings.Index(js, "prevOnly !== cfg.onlyRoots")
	seg := js[i:]
	if j := strings.Index(seg, "redraw();"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "buildRoot();") {
		t.Error("★ 可见范围变化后必须 buildRoot()（回到首页重新取数），不能只重绘")
	}
}

func Test2416BootSkipsLastPath(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if !strings.Contains(js, "if (last && !cfg.onlyRoots)") {
		t.Error("★ 开启「只显示我添加的文件夹」后，启动不得恢复上次目录 —— " +
			"旧地址可能已不在范围内，会先吃一个 403 再回退")
	}
	if !strings.Contains(js, "cfg.onlyRoots ? '我的文件夹' : '存储空间'") {
		t.Error("面包屑首段应随开关切换文案（否则仍写「存储空间」，与实际范围不符）")
	}
}

// Test2416RootsHelpers 纯函数行为：baseName 与 rootExists。
func Test2416RootsHelpers(t *testing.T) {
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + coverGrabFn + `
function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}

// baseName 复用既有的 normPath + baseName 这一对（别在别处再写一份）。
// 先声明同名变量再赋值：这样 eval 出来的函数体里的自由变量 normPath 能解析到。
var normPath = null, baseName = null;
eval('normPath = ' + grab('normPath'));
eval('baseName = ' + grab('baseName'));
eq(baseName('/vol1/1000/照片'), '照片', 'POSIX 路径取最后一段');
eq(baseName('D:\\PCAPP\\a\\b'), 'b', 'Windows 路径取最后一段');
eq(baseName('/a/b/'), 'b', '结尾斜杠不应产生空段');
eq(baseName(''), '', '空串返回空');
eq(baseName('/'), '', '根路径没有最后一段');

var cfg = { rootsInfo: [ {path:'/a', exists:true}, {path:'/b', exists:false} ] };
eval('var _rootExists = ' + grab('rootExists'));
eq(_rootExists('/a'), true, '存在的目录');
eq(_rootExists('/b'), false, '不存在的目录要标出来');
eq(_rootExists('/new'), true, '刚添加、服务端还没确认的条目先按可访问显示');
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
}
