// 2.4.16 新增：「只显示我添加的文件夹」的后端契约测试。
//
// 这个功能的核心不是"少画几个格子"，而是**服务端也不再读**范围外的路径。
// 所以断言分三层：
//
//	① 归一化 / 校验（纯函数）：脏数据进不来，重复与相对路径被处理掉；
//	② 范围判定：开启后范围内可读、范围外 403、未开启时行为不变；
//	③ 首页与预生成：首页只由列表构成（不再枚举盘位）、一键生成不从 "/" 起跑。
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withTestSettings 临时替换全局设置，并在测试结束时还原。
// 同时把 settingsFile 清空：否则保存流程会把测试配置写进真实配置文件。
func withTestSettings(t *testing.T, s Settings) {
	t.Helper()
	settingsMu.Lock()
	oldSettings := settings
	oldFile := settingsFile
	settings = s
	settingsFile = ""
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings = oldSettings
		settingsFile = oldFile
		settingsMu.Unlock()
	})
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------- ① 归一化 ----------

func Test2416CleanRoots(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")

	dup := a
	if runtime.GOOS == "windows" {
		dup = strings.ToUpper(a) // Windows 上大小写不同也是同一个目录
	}

	got := cleanRoots([]string{
		"  " + a + "  ",                // 前后空白
		a + string(filepath.Separator), // 结尾分隔符
		dup,                            // 同一目录换个写法
		"relative/path",                // 相对路径：不能当范围依据
		"",                             // 空串
		"   ",                          // 全空白
		b,
	})
	if len(got) != 2 {
		t.Fatalf("应只剩 2 个有效目录（a、b），实际 %d 个: %v", len(got), got)
	}
	if !samePath(got[0], a) || !samePath(got[1], b) {
		t.Errorf("归一化结果不对: %v", got)
	}
}

func Test2416CleanRootsCap(t *testing.T) {
	base := t.TempDir()
	in := make([]string, 0, maxCustomRoots+5)
	for i := 0; i < maxCustomRoots+5; i++ {
		in = append(in, filepath.Join(base, "r"+string(rune('a'+i%26))+"-"+string(rune('0'+i/26))))
	}
	got := cleanRoots(in)
	if len(got) > maxCustomRoots {
		t.Errorf("条目数必须被限制在 %d 以内，实际 %d", maxCustomRoots, len(got))
	}
}

// ---------- ② 范围判定 ----------

func Test2416RootScopeAllows(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "照片")
	sub := filepath.Join(root, "2026")
	other := filepath.Join(base, "别的目录")
	mkdirs(t, sub, other)

	// 未开启：恒通过（不能影响存量用户）
	withTestSettings(t, Settings{OnlyRoots: false, Roots: nil})
	if !rootScopeAllows(other) {
		t.Error("★ 未开启时不应对任何路径设限")
	}

	// 开启但一个都没加：一律拒绝 —— 这就是"什么都不加载"
	withTestSettings(t, Settings{OnlyRoots: true, Roots: nil})
	if rootScopeAllows(root) {
		t.Error("★★ 开启且列表为空时必须全拒（否则就是「看着空、其实还能读」）")
	}

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{root}})
	if !rootScopeAllows(root) {
		t.Error("目录本身应可通过")
	}
	if !rootScopeAllows(sub) {
		t.Error("★ 目录的**子目录**必须可通过（否则点进去就 403）")
	}
	if rootScopeAllows(other) {
		t.Error("★★ 列表之外的目录必须被拒绝")
	}
	if rootScopeAllows(filepath.Join(base, "照片2")) {
		t.Error("★ 前缀相同但不同名（照片 / 照片2）不能被当成范围内")
	}
}

// Test2416ResolveScopedVsBroad 收窄版拒绝范围外，Broad 版照旧放行。
//
// Broad 是三个"必须绕过"的场景（目录选择器 / 缩略图目录 / 系统缩略图接管）的实现基础：
// 一旦它也收窄，用户就再也加不了新文件夹了。
func Test2416ResolveScopedVsBroad(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	outside := filepath.Join(base, "outside")
	mkdirs(t, inside, outside)
	// 让普通白名单认可这两个目录（真实部署里由 /vol* 或 TRIM_DATA_ACCESSIBLE_PATHS 提供）
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", base)

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{inside}})

	if _, sc := resolveMediaPath(inside); sc != 0 {
		t.Errorf("范围内应放行，实际 status=%d", sc)
	}
	if _, sc := resolveMediaPath(outside); sc != http.StatusForbidden {
		t.Errorf("★★ 范围外必须 403，实际 status=%d", sc)
	}
	// Broad：目录选择器要能浏览到"还没添加"的目录
	if _, sc := resolveMediaPathBroad(outside); sc != 0 {
		t.Errorf("★ Broad 版必须放行范围外目录（否则无法添加新文件夹），实际 status=%d", sc)
	}
}

func Test2416ValidateCustomRoots(t *testing.T) {
	base := t.TempDir()
	ok1 := filepath.Join(base, "a")
	ok2 := filepath.Join(base, "b")
	mkdirs(t, ok1, ok2)
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", base)

	got, msg := validateCustomRoots([]string{ok1, ok2, ok1})
	if msg != "" {
		t.Fatalf("合法列表不应报错: %s", msg)
	}
	if len(got) != 2 {
		t.Errorf("重复项应被去重，期望 2 个，实际 %v", got)
	}

	// 相对路径：直接拒，而且**报错原因必须点明是"不是绝对路径"**。
	// 只断言"报错了"是不够的：相对路径通常也不存在，于是"因为不存在而报错"会让
	// 这条断言在没有绝对路径校验时照样通过（实测变异存活）。
	if _, msg := validateCustomRoots([]string{"relative/dir"}); !strings.Contains(msg, "绝对路径") {
		t.Errorf("★ 相对路径必须以「需要绝对路径」拒绝，实际提示: %q", msg)
	}
	// 不存在：拒
	if _, msg := validateCustomRoots([]string{filepath.Join(base, "不存在")}); msg == "" {
		t.Error("★ 不存在的目录必须被拒绝")
	}
	// 白名单之外：拒（这里临时把 TRIM 指到别处来模拟）
	other := t.TempDir()
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", other)
	if _, msg := validateCustomRoots([]string{ok1}); msg == "" {
		t.Error("★ 白名单之外的目录必须被拒绝")
	}
}

// ---------- ③ 首页与预生成 ----------

func Test2416AggregateRootOnlyRoots(t *testing.T) {
	base := t.TempDir()
	r1 := filepath.Join(base, "照片")
	r2 := filepath.Join(base, "视频")
	missing := filepath.Join(base, "没挂载")
	mkdirs(t, r1, r2)

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{r1, r2, missing}})
	resp := aggregateRoot("name")
	if resp.Path != "/" || resp.Parent != "" {
		t.Errorf("首页 path/parent 不对: %q/%q", resp.Path, resp.Parent)
	}
	if len(resp.Dirs) != 2 {
		t.Fatalf("应只列出 2 个存在的已添加目录（缺失的跳过），实际 %d: %v", len(resp.Dirs), resp.Dirs)
	}
	paths := []string{resp.Dirs[0].Path, resp.Dirs[1].Path}
	if !containsPath(paths, r1) || !containsPath(paths, r2) {
		t.Errorf("首页条目必须就是已添加的目录，实际 %v", paths)
	}
	if containsPath(paths, missing) {
		t.Error("★ 不存在的目录不应出现在首页（点不开的格子）")
	}
	if resp.Total != 2 {
		t.Errorf("total 应等于条目数，实际 %d", resp.Total)
	}

	// 关闭：首页不能再由这份列表构成
	withTestSettings(t, Settings{OnlyRoots: false, Roots: []string{r1, r2}})
	resp2 := aggregateRoot("name")
	for _, d := range resp2.Dirs {
		if samePath(d.Path, r1) || samePath(d.Path, r2) {
			t.Errorf("★ 关闭后首页不该再直接列出自定义目录: %s", d.Path)
		}
	}
}

func containsPath(list []string, p string) bool {
	for _, x := range list {
		if samePath(x, p) {
			return true
		}
	}
	return false
}

func Test2416PregenStartDirs(t *testing.T) {
	base := t.TempDir()
	r1 := filepath.Join(base, "照片")
	r2 := filepath.Join(base, "视频")
	mkdirs(t, r1, r2)

	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{r1, r2, filepath.Join(base, "没挂载")}})
	got, sc := pregenStartDirs("")
	if sc != 0 {
		t.Fatalf("首页起点不该报错，status=%d", sc)
	}
	if len(got) != 2 || !containsPath(got, r1) || !containsPath(got, r2) {
		t.Errorf("★ 首页一键生成应从已添加的目录起跑，实际 %v", got)
	}
	got2, _ := pregenStartDirs("/")
	if len(got2) != 2 {
		t.Errorf("path=/ 与空 path 应等价，实际 %v", got2)
	}

	// ★ 关键回归：任何情况下都**不得**把文件系统根当成起点
	//   （旧实现在首页传字面量 "/"，会把 /usr、/proc 一起走一遍）
	withTestSettings(t, Settings{OnlyRoots: false})
	got3, _ := pregenStartDirs("")
	for _, d := range got3 {
		if samePath(d, "/") || samePath(d, `\`) {
			t.Fatalf("★★ 一键生成绝不能从文件系统根起跑（实际 %v）", got3)
		}
	}
}

// ---------- ④ 老前端保存不丢数据 ----------

func Test2416MergeAbsentKeepsPrev(t *testing.T) {
	prev := Settings{OnlyRoots: true, Roots: []string{"/vol1/1000/照片"}}

	// 请求里只带了别的字段（老版本 app.js 的行为）
	var in Settings
	in.mergeAbsent(map[string]json.RawMessage{"thumbEnabled": json.RawMessage("true")}, prev)
	if !in.OnlyRoots {
		t.Error("★★ 字段缺失时 onlyRoots 必须保留原值（否则升级后保存一次就被关掉）")
	}
	if len(in.Roots) != 1 {
		t.Error("★★ 字段缺失时 roots 必须保留原值（否则用户添加的文件夹被清空）")
	}

	// 请求里明确带了这两个字段：按请求走（用户就是想关掉 / 清空）
	var in2 Settings
	in2.mergeAbsent(map[string]json.RawMessage{
		"onlyRoots": json.RawMessage("false"),
		"roots":     json.RawMessage("[]"),
	}, prev)
	if in2.OnlyRoots {
		t.Error("显式传 false 时应遵循请求")
	}
	if len(in2.Roots) != 0 {
		t.Error("显式传空列表时应遵循请求")
	}
}
