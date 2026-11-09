package main

// 2.4.25 引入、2.4.26 仍在用的回归：重建单个目录封面端点（POST /api/dircov/rebuild）。
//
//	① 只碰当前目录这一个封面（refresh=true 强制重抽），不递归、不清缩略图缓存；
//	② 工具栏重建按钮按内容分流调用它（分流断言见 zzz_2426）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 路由必须挂上 /api/dircov/rebuild
func Test2425RebuildRouteRegistered(t *testing.T) {
	src := readSourceOrSkip(t, "main.go")
	if !strings.Contains(src, `"/api/dircov/rebuild"`) {
		t.Error("★ main.go 必须注册 /api/dircov/rebuild 路由")
	}
}

// 空目录：重建成功（source=none），不报错、不 500。
func Test2425RebuildEmptyDir(t *testing.T) {
	_ = setupTempRoot(t)
	dir := filepath.Join(workDir, "空目录")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleRebuildDirCover(w, httptest.NewRequest("POST", "/api/dircov/rebuild?path="+dir+"&size=320", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("空目录重建应 200，got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("响应应含 ok:true，got %s", w.Body.String())
	}
}

// 非目录路径 → 404 not_dir
func Test2425RebuildNotDir(t *testing.T) {
	_ = setupTempRoot(t)
	f := filepath.Join(workDir, "a.jpg")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleRebuildDirCover(w, httptest.NewRequest("POST", "/api/dircov/rebuild?path="+f+"&size=320", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("文件路径应 404 not_dir，got %d: %s", w.Code, w.Body.String())
	}
}

// GET 方法 → 405
func Test2425RebuildMethodNotAllowed(t *testing.T) {
	_ = setupTempRoot(t)
	w := httptest.NewRecorder()
	handleRebuildDirCover(w, httptest.NewRequest("GET", "/api/dircov/rebuild", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，got %d", w.Code)
	}
}

// 作用域静态闸门：重建只做单目录 cover，绝不能递归清缩略图/清别的封面。
func Test2425RebuildStaticScope(t *testing.T) {
	src := stripLineComments(readSourceOrSkip(t, "dircov.go"))
	// 强制重抽：refresh=true 只出现在重建端点里（按需生成用变量、一键生成用 false）
	if !strings.Contains(src, "buildCover(dir, size, jpgPath, metaPath, true)") {
		t.Error("★ 重建必须用 buildCover(..., refresh=true) 强制重抽，否则坏封面不会真正重做")
	}
	// 隔离出 handleRebuildDirCover 的函数体，确认它不递归清封面/清缩略图
	i := strings.Index(src, "func handleRebuildDirCover")
	if i < 0 {
		t.Fatal("找不到 handleRebuildDirCover")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	if strings.Contains(body, "purgeDirCover") {
		t.Error("★ 重建单目录封面不得调用 purgeDirCover（那会碰别的封面）")
	}
	if strings.Contains(body, "handleClearThumbCache") {
		t.Error("★ 重建单目录封面不得清缩略图缓存（那会递归碰下层文件夹/图片）")
	}
}
