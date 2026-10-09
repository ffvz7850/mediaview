package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1.8.70：列表排序词表对齐飞牛文件管理器
//
// 目标（用户诉求）：大图左右切换、大图预加载、缩略图预生成的顺序，都要和
// 飞牛文件管理器里看到的顺序一致。三样东西共用同一个来源 —— /api/list 返回的
// files 顺序，而它由 parseSort + listDir 决定。所以核心断言都落在这一层。
// ---------------------------------------------------------------------------

func Test1870ParseSortNormalization(t *testing.T) {
	cases := []struct {
		in    string
		field string
		desc  bool
	}{
		{"", "name", false},
		{"name", "name", false},
		{"name:asc", "name", false},
		{"name:desc", "name", true},
		{"NAME_DESC", "name", true}, // 大小写 + 下划线分隔都认
		{"mtim", "mtime", true},     // 只给字段：修改时间沿用旧默认（倒序）
		{"mtim:asc", "mtime", false},
		{"mtime:desc", "mtime", true},
		{"date", "mtime", true}, // 旧值 date = 修改时间倒序
		{"date:asc", "mtime", false},
		{"size", "size", true}, // 只给字段：大小沿用旧默认（倒序）
		{"size:asc", "size", false},
		{"size:desc", "size", true},
		{"type", "type", false},
		{"type:desc", "type", true},
		{"btim", "btime", false}, // 只给字段：创建时间默认升序
		{"btim:desc", "btime", true},
		{"ctime:asc", "btime", false}, // ctime 别名 → 创建时间
		{"created:desc", "btime", true},
		{"bogus:desc", "name", false}, // 无法识别 → 退回默认 name asc，绝不乱序
		{"   ", "name", false},
	}
	for _, c := range cases {
		got := parseSort(c.in)
		if got.field != c.field || got.desc != c.desc {
			t.Errorf("parseSort(%q) = %s/%v，期望 %s/%v", c.in, got.field, got.desc, c.field, c.desc)
		}
	}
}

func Test1870NaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		less bool
	}{
		{"img_2.jpg", "img_10.jpg", true}, // 数字按数值：2 < 10
		{"img_10.jpg", "img_2.jpg", false},
		{"img_002.jpg", "img_2.jpg", false}, // 数值相同，前导零少的在前 → "2" < "02"
		{"img_2.jpg", "img_002.jpg", true},
		{"2.jpg", "a.jpg", true}, // 数字在字母前
		{"a.jpg", "2.jpg", false},
		{"a", "ab", true},                     // 前缀短的在前
		{"photo_1.jpg", "photo_1.jpg", false}, // 完全相同
	}
	for _, c := range cases {
		if got := naturalLess(c.a, c.b); got != c.less {
			t.Errorf("naturalLess(%q, %q) = %v，期望 %v", c.a, c.b, got, c.less)
		}
	}
}

// 建一个真实临时目录，放 4 个不同名/大小/时间的媒体文件，用于 listDir 排序断言。
func makeSortDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, size int, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("IMG_2.jpg", 10, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	write("IMG_10.jpg", 30, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	write("b.png", 20, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC))
	write("a.mp4", 40, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC))
	return dir
}

func namesOf(files []FileItem) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

func Test1870ListDirSortOrder(t *testing.T) {
	dir := makeSortDir(t)

	cases := []struct {
		sort  string
		order []string
	}{
		{"name:asc", []string{"a.mp4", "b.png", "IMG_2.jpg", "IMG_10.jpg"}},
		{"name:desc", []string{"IMG_10.jpg", "IMG_2.jpg", "b.png", "a.mp4"}},
		{"size:desc", []string{"a.mp4", "IMG_10.jpg", "b.png", "IMG_2.jpg"}},
		{"size:asc", []string{"IMG_2.jpg", "b.png", "IMG_10.jpg", "a.mp4"}},
		{"mtime:desc", []string{"a.mp4", "b.png", "IMG_2.jpg", "IMG_10.jpg"}},
		{"mtime:asc", []string{"IMG_10.jpg", "IMG_2.jpg", "b.png", "a.mp4"}},
		// 同扩展名（.jpg 两张）时**保持 os.ReadDir 的字节序**（IMG_10 在 IMG_2 前）——
		// 与飞牛文件管理器一致：它在字段相等时返回 0，靠稳定排序保持后端返回的原始顺序。
		// 若改成按文件名自然排序兜底，同一秒拍摄/批量导入的照片就会与文件管理器错位。
		{"type:asc", []string{"IMG_10.jpg", "IMG_2.jpg", "a.mp4", "b.png"}},
		{"type:desc", []string{"b.png", "a.mp4", "IMG_10.jpg", "IMG_2.jpg"}},
	}
	for _, c := range cases {
		resp, err := listDir(dir, c.sort)
		if err != nil {
			t.Fatalf("listDir(%q) 出错：%v", c.sort, err)
		}
		got := namesOf(resp.Files)
		if strings.Join(got, ",") != strings.Join(c.order, ",") {
			t.Errorf("sort=%q：得到 %v，期望 %v", c.sort, got, c.order)
		}
		if resp.Total != len(c.order) {
			t.Errorf("sort=%q：Total=%d，期望 %d", c.sort, resp.Total, len(c.order))
		}
	}
}

// 目录恒在文件前（飞牛文件管理器同款），且目录段也按当前列排序。
func Test1870ListDirDirsComeFirstAndSorted(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"zzz", "aaa", "mmm"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "1.jpg"), []byte("x"), 0o644)

	resp, err := listDir(dir, "name:asc")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Dirs) != 3 {
		t.Fatalf("目录数 = %d，期望 3", len(resp.Dirs))
	}
	// 目录按名字升序
	want := []string{"aaa", "mmm", "zzz"}
	for i, d := range resp.Dirs {
		if d.Name != want[i] {
			t.Errorf("目录顺序[%d] = %q，期望 %q", i, d.Name, want[i])
		}
	}
}

// 回传实际生效的排序字段 + 方向，前端据此同步控件、排查不用靠猜。
func Test1870ListDirEchoesEffectiveSort(t *testing.T) {
	dir := makeSortDir(t)
	resp, err := listDir(dir, "date:desc")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Sort != "mtime" || !resp.SortDesc {
		t.Errorf("回传 Sort=%q SortDesc=%v，期望 mtime/true（date 别名已归一）", resp.Sort, resp.SortDesc)
	}
	resp2, _ := listDir(dir, "bogus")
	if resp2.Sort != "name" || resp2.SortDesc {
		t.Errorf("未知排序回传 Sort=%q SortDesc=%v，期望 name/false（退回默认）", resp2.Sort, resp2.SortDesc)
	}
}

// 按创建时间排序会触发 fileBirthTime；Windows 上返回真实 NTFS 创建时间，不崩、非负。
func Test1870FileBirthTimeNoCrash(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bt.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := fileBirthTime(p)
	if v < 0 {
		t.Errorf("fileBirthTime = %d，不应为负", v)
	}
	// 按创建时间排序整条链路不 panic、结果确定
	dir := makeSortDir(t)
	if _, err := listDir(dir, "btime:desc"); err != nil {
		t.Fatalf("listDir btime 排序出错：%v", err)
	}
}

// 后端接线：/api/list 把 ?sort= 原样交给 listDir（在 handleList 内），
// 排序解析与比较都收敛到 sortkey.go，不允许在 media.go 里另起一套。
func Test1870SortWiringBackend(t *testing.T) {
	m := readSourceOrSkip(t, "media.go")
	if !strings.Contains(m, `listDir(path, q.Get("sort"))`) {
		t.Error("handleList 没有把 ?sort= 传给 listDir —— 前端传的排序会被丢弃")
	}
	sk := readSourceOrSkip(t, "sortkey.go")
	// 同键时必须用文件名兜底，否则连拍/批量导出（时间或大小相同）顺序随机，大图切换会跳
	if !strings.Contains(sk, "naturalLess(la, lb)") {
		t.Error("排序比较缺少文件名兜底 —— 时间/大小相同的项顺序不确定")
	}
}

// 前端接线：五项下拉 + 方向按钮 + 排序解析（1.8.178 起没有"跟随"开关了）。
func Test1870FrontendSortUI(t *testing.T) {
	idx := readSourceOrSkip(t, "web/index.html")
	for _, want := range []string{
		`<option value="mtime:desc">按修改时间</option>`,
		`<option value="name:asc">按名称</option>`,
		`<option value="size:desc">按大小</option>`,
		`<option value="type:asc">按类型</option>`,
		`<option value="btime:desc">按创建时间</option>`,
		`id="sortDirBtn"`,
	} {
		if !strings.Contains(idx, want) {
			t.Errorf("index.html 缺少 %s", want)
		}
	}
	// 1.8.178：开关已删除 —— 排序统一由右上角排序按钮决定（网格与大图同一来源）
	if strings.Contains(idx, "setFollowFnOsSort") {
		t.Error("index.html 仍有 setFollowFnOsSort —— 该开关已删除（它只影响网格，会重新制造不一致）")
	}

	js := readSourceOrSkip(t, "web/app.js")
	for _, want := range []string{
		"function normalizeSortParam",
		"function resolveSortForDir",
		"function applySort",
		"state.sortManual",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js 缺少 %s", want)
		}
	}

	// 1.8.181：偏好读取器 fnos_sort.js **必须存在** —— 它是"大图排序"的来源。
	//
	// 1.8.177 曾把它整个删除，理由是"飞牛没把目录排序持久化、这份 IndexedDB 恒返回 null"。
	// **那个判断只对了一半**：
	//   · 对的部分：飞牛确实**没有把排序写进它的 SQLite**（folder_views 只在改视图模式/
	//     图标大小时被顺带写一次，后端读它的 SQLite / 连它的 WebSocket 都是死路）。
	//   · 错的部分：**前端 IndexedDB 一直在写**。已在飞牛前端 bundle
	//     /usr/trim/www/assets/openDefaultApps-*.js 里确认：
	//         createObjectStore('file-view-preferences',
	//             {keyPath:['userId','scope','surface','location']})
	//     同文件里 sortField 出现 6 次、sortType 10 次、sorting 37 次。
	// ⇒ 这份偏好是可读的，1.8.165 的"准"就来自它。
	if _, err := os.Stat("web/fnos_sort.js"); err != nil {
		t.Error("web/fnos_sort.js 缺失 —— 大图排序要靠它读飞牛前端写的 IndexedDB 偏好")
	}
}
