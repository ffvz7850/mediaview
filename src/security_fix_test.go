package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 把缩略图根目录指向一个临时目录，并返回缩略图根目录。
// 同时把 workDir 也设成临时目录，这样白名单里有"内"也有"外"，测试与平台无关。
func setupTempRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	prevWD := workDir
	workDir = tmp
	t.Cleanup(func() { workDir = prevWD })

	s := getSettings()
	s.ThumbDir = tmp
	settingsMu.Lock()
	settings = s
	settingsMu.Unlock()
	applyThumbRoot()
	return currentThumbRoot()
}

// outsideWhitelist 返回一个「绝对路径、但不在白名单内」的路径
func outsideWhitelist(t *testing.T) string {
	t.Helper()
	// t.TempDir() 每次给一个全新目录；取父目录下的独立子目录，保证不在 workDir 之下
	return filepath.Join(t.TempDir(), "not-in-whitelist")
}

// ① 删除接口必须拒绝相对路径、以及白名单之外的绝对路径
func TestClearThumbCache_RejectsTraversal(t *testing.T) {
	setupTempRoot(t)
	cases := []struct {
		name string
		q    string
		want int
	}{
		{"相对路径向上穿越", "?path=../../etc", http.StatusBadRequest},
		{"相对路径穿越到存储空间", "?path=../../../../vol1/1000", http.StatusBadRequest},
		{"绝对路径但不在白名单", "?path=" + outsideWhitelist(t), http.StatusForbidden},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		handleClearThumbCache(w, httptest.NewRequest("POST", "/api/thumb/clear"+c.q, nil))
		if w.Code != c.want {
			t.Errorf("%s：path=%s 期望 %d，实际 %d", c.name, c.q, c.want, w.Code)
		}
	}
}

// ② 白名单内的合法媒体目录必须仍然可用（别把功能一起改坏）
func TestClearThumbCache_AllowsLegitMediaDir(t *testing.T) {
	_ = setupTempRoot(t)
	legit := filepath.Join(workDir, "照片")
	os.MkdirAll(legit, 0o755)
	w := httptest.NewRecorder()
	handleClearThumbCache(w, httptest.NewRequest("POST", "/api/thumb/clear?path="+legit, nil))
	if w.Code != http.StatusOK {
		t.Errorf("合法媒体目录被拒：%d %s", w.Code, w.Body.String())
	}
}

// ③ 媒体接口必须拒绝白名单之外的路径（root 任意读防护）
func TestResolveMediaPath(t *testing.T) {
	_ = setupTempRoot(t)
	inside := filepath.Join(workDir, "a.jpg")
	outside := filepath.Join(outsideWhitelist(t), "a.jpg")

	if _, sc := resolveMediaPath("../../etc/passwd"); sc != http.StatusBadRequest {
		t.Errorf("相对路径应被拒(400)，实际 %d", sc)
	}
	if _, sc := resolveMediaPath(inside); sc != 0 {
		t.Errorf("白名单内路径应放行，实际 %d", sc)
	}
	if _, sc := resolveMediaPath(outside); sc != http.StatusForbidden {
		t.Errorf("白名单外路径应 403，实际 %d", sc)
	}
}

// ④ 清空缓存只允许删「纯数字尺寸」目录，用户数据目录必须留下
func TestPurgeCache_KeepsForeignDirectories(t *testing.T) {
	root := setupTempRoot(t)
	mine := filepath.Join(root, "320")
	foreign := filepath.Join(root, "我的照片")
	for _, d := range []string{mine, foreign} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "x.jpg"), []byte("x"), 0o644)
	}
	if _, err := purgeCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("自己的 size 目录应被删除，实际仍存在")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("用户目录「我的照片」被误删了！err=%v", err)
	}
}

// ⑥ 目录浏览不得二次解码：含 "+" 的目录名必须能正确解析
func TestHandleBrowse_NoDoubleDecode(t *testing.T) {
	_ = setupTempRoot(t)
	sub := filepath.Join(workDir, "IMG+RAW")
	if err := os.MkdirAll(filepath.Join(sub, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 前端 encodeURIComponent("IMG+RAW") => "IMG%2BRAW"
	w := httptest.NewRecorder()
	handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(sub), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("含 + 的目录被拒：%d %s", w.Code, w.Body.String())
	}
	var got struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != sub {
		t.Errorf("二次解码破坏了含 + 的路径：得到 %q，期望 %q", got.Path, sub)
	}
}

// ⑤ 根目录级位置不得被设为缩略图目录
func TestIsForbiddenThumbDir(t *testing.T) {
	_ = setupTempRoot(t)
	cases := []struct {
		in   string
		want bool
	}{
		{"/", true},
		{workDir, true},
		{filepath.Join(workDir, "缓存"), false},
	}
	// /vol* 形态只在类 Unix 上才是绝对路径（Windows 上 IsAbs("/vol1") == false）
	if runtime.GOOS != "windows" {
		cases = append(cases,
			struct {
				in   string
				want bool
			}{"/vol1", true},
			struct {
				in   string
				want bool
			}{"/vol1/1000", true},
			struct {
				in   string
				want bool
			}{"/vol3/1000/缓存", false},
		)
	}
	for _, c := range cases {
		if got := isForbiddenThumbDir(c.in); got != c.want {
			t.Errorf("isForbiddenThumbDir(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}
