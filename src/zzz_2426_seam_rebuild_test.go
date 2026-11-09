package main

// 2.4.26 回归：
//
//	① 封面拼缝改固定色 coverSeamColor（白稍暗 #f0f0f0），与主题/请求参数彻底无关；
//	② 工具栏重建按钮按当前目录内容分流：子文件夹→逐个重建封面（/dircov/rebuild），
//	  媒体文件→清当前层缩略图（/thumb/clear&shallow=1，非递归），两者都有→都做。

import (
	"bytes"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- ① 缝色固定 ----------

func Test2426SeamColorFixed(t *testing.T) {
	red := color.RGBA{200, 20, 20, 255}
	green := color.RGBA{20, 180, 20, 255}
	blue := color.RGBA{20, 20, 200, 255}
	white := color.RGBA{240, 240, 240, 255}

	data, err := composeFromImages([]image.Image{solidImg(red), solidImg(green), solidImg(blue), solidImg(white)}, 320)
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	at := func(x, y int) color.RGBA {
		return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	}

	// 左上图块中心 = 红（图块本身没被缝色污染）
	if got := at(80, 80); diff(got, red) > 40 {
		t.Errorf("左上图块应是红，got %v", got)
	}
	// 四块交界处的缝 = 固定色 #f0f0f0（白稍暗）—— 不随主题、不随请求参数变
	seam := at(160, 160)
	want := coverSeamColor()
	if want.R != 240 || want.G != 240 || want.B != 240 {
		t.Fatalf("★ coverSeamColor 必须是 #f0f0f0（白稍暗），got %v", want)
	}
	if diff(seam, want) > 40 {
		t.Errorf("缝色应是固定 %v，got %v", want, seam)
	}
	// 右下图块是纯白 240：缝色 240 与它同值，改用左上/右下交界验证——
	// 竖缝中点 (160, 80) 也必须是缝色（那里两侧分别是红/绿，混不出来的）
	if got := at(160, 80); diff(got, want) > 40 {
		t.Errorf("竖缝中点应是固定 %v，got %v", want, got)
	}

	// 静态闸门：常量定义存在，且前端不再传 bg
	src := readSourceOrSkip(t, "dircov.go")
	if !strings.Contains(src, "func coverSeamColor()") {
		t.Error("★ dircov.go 必须有 coverSeamColor 固定缝色（2.4.26）")
	}
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if strings.Contains(js, "coverBG") {
		t.Error("★ 前端不应再有 coverBG（缝色固定，与主题无关）")
	}
	i := strings.Index(js, "function coverURL(")
	if i < 0 {
		t.Fatal("找不到 coverURL")
	}
	if strings.Contains(js[i:i+300], "&bg=") {
		t.Error("★ coverURL 不应再拼 &bg=（缝色固定在服务端）")
	}
}

// ---------- ② 重建按钮分流 ----------

// 前端按钮必须按内容分流：子文件夹→/dircov/rebuild，媒体文件→/thumb/clear&shallow=1。
func Test2426RebuildButtonBranching(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "btnRebuildThumbs")
	if i < 0 {
		t.Fatal("找不到 btnRebuildThumbs")
	}
	seg := js[i:]
	if j := strings.Index(seg, "settingsClose"); j > 0 {
		seg = seg[:j]
	}
	for _, want := range []string{
		"it.kind === 'dir'",              // 按内容分流
		"'/dircov/rebuild?path='",        // 子文件夹 → 重建封面
		"'&shallow=1'",                   // 媒体文件 → 非递归清当前层缩略图
		"encodeURIComponent(state.path)", // shallow 清的是**当前目录**
		"bumpThumbCacheBust()",           // 完成后强制浏览器重新拉
	} {
		if !strings.Contains(seg, want) {
			t.Errorf("★ 重建按钮缺少分流关键结构 %q", want)
		}
	}
	// 分流语义：有子文件夹才发 rebuild、有文件才发 shallow 清理（两个条件都要在）
	if !strings.Contains(seg, "doCover") || !strings.Contains(seg, "doThumb") {
		t.Error("★ 分流必须用 doCover / doThumb 两个开关（两者都有时都做）")
	}
}

// shallow 清理的纯函数行为：只删当前层文件，子目录（下层缩略图）原样保留。
func Test2426ShallowClearKeepsSubdirs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "320", "rel")
	sub := filepath.Join(dir, "子目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "IMG_a.jpg")
	b := filepath.Join(sub, "IMG_b.jpg")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if n := clearShallowThumbFiles(dir); n != 1 {
		t.Fatalf("★ shallow 应只删当前层 1 个文件，实际删了 %d", n)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Error("★ 当前层的缩略图文件应被删除")
	}
	if _, err := os.Stat(b); err != nil {
		t.Error("★ 子目录里的缩略图必须原样保留（shallow 绝不能递归碰下层）")
	}
	// 幂等：再跑一次不报错、删 0 个
	if n := clearShallowThumbFiles(dir); n != 0 {
		t.Errorf("第二次应删 0 个，got %d", n)
	}
}

// handler 层：shallow=1 请求被接受（异步删除行为由上面的纯函数测试覆盖）。
func Test2426ShallowClearHandler(t *testing.T) {
	_ = setupTempRoot(t)
	legit := filepath.Join(workDir, "照片")
	os.MkdirAll(legit, 0o755)
	w := httptest.NewRecorder()
	handleClearThumbCache(w, httptest.NewRequest("POST", "/api/thumb/clear?path="+legit+"&shallow=1", nil))
	if w.Code != http.StatusOK {
		t.Errorf("shallow 清理被拒：%d %s", w.Code, w.Body.String())
	}
	// 静态闸门：shallow 模式绝不能顺手把当前目录的封面也重抽了
	src := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	i := strings.Index(src, "func handleClearThumbCache")
	if i < 0 {
		t.Fatal("找不到 handleClearThumbCache")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j > 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "if !shallow {") {
		t.Error("★ shallow 必须有独立分支（只清当前层文件）")
	}
	if !strings.Contains(body, "if !shallow {\n\t\t\tif dc := purgeDirCover") {
		t.Error("★ shallow 模式不得 purgeDirCover（重建图片缩略图不该动封面）")
	}
}
