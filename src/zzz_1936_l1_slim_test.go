package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeJPEGFile 生成一张真实可解码的小 JPEG。
func writeJPEGFile(t *testing.T, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 5), 90, 255})
		}
	}
	p := filepath.Join(t.TempDir(), name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建文件失败: %v", err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 80}); err != nil {
		f.Close()
		t.Fatalf("编码失败: %v", err)
	}
	f.Close()
	return p
}

// TestL1AJpegHeaderOK vips 路径改用 jpegHeaderOK（只读头）后的行为。
func TestL1AJpegHeaderOK(t *testing.T) {
	good := writeJPEGFile(t, "ok.jpg", 64, 48)
	if !jpegHeaderOK(good) {
		t.Error("正常 JPEG 被判为不可用")
	}
	// 空文件 / 截断 / 非 JPEG
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jpg")
	_ = os.WriteFile(empty, nil, 0o644)
	if jpegHeaderOK(empty) {
		t.Error("空文件被判为可用")
	}
	trunc := filepath.Join(dir, "trunc.jpg")
	_ = os.WriteFile(trunc, []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644)
	if jpegHeaderOK(trunc) {
		t.Error("截断文件被判为可用")
	}
	notjpg := filepath.Join(dir, "x.jpg")
	_ = os.WriteFile(notjpg, []byte("not a jpeg at all"), 0o644)
	if jpegHeaderOK(notjpg) {
		t.Error("非 JPEG 被判为可用")
	}
	if jpegHeaderOK(filepath.Join(dir, "nope.jpg")) {
		t.Error("不存在的文件被判为可用")
	}

	// 契约：vips 路径不得再用 validateJPEG（完整解码）
	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	i := strings.Index(th, "func tryVipsImageThumb(")
	if i < 0 {
		t.Fatal("找不到 tryVipsImageThumb")
	}
	seg := th[i:]
	if j := strings.Index(seg, "\nfunc "); j > 0 {
		seg = seg[:j]
	}
	if strings.Contains(seg, "validateJPEG") {
		t.Error("tryVipsImageThumb 又用回 validateJPEG —— 会对纯软件路径白做完整解码")
	}
	if !strings.Contains(seg, "jpegHeaderOK") {
		t.Error("tryVipsImageThumb 没有做文件头校验")
	}
	// 但绿图检测必须仍在 VAAPI 路径里
	ff := stripLineComments(readSourceOrSkip(t, "ffmpeg.go"))
	if !strings.Contains(ff, "validateJPEG") {
		t.Error("VAAPI 输出校验丢了 validateJPEG —— 绿图会漏过去")
	}
}

// TestL1DBytesEqualStable 探测缓存的键必须含 mtime+size（原图变了要失效）。
func TestL1DBytesEqualStable(t *testing.T) {
	p := writeJPEGFile(t, "d.jpg", 40, 30)
	w1, h1 := imageDimCached(p)
	w2, h2 := imageDimCached(p)
	if w1 != 40 || h1 != 30 {
		t.Fatalf("imageDimCached = %d,%d，期望 40,30", w1, h1)
	}
	if w1 != w2 || h1 != h2 {
		t.Errorf("两次调用结果不一致：%d,%d vs %d,%d", w1, h1, w2, h2)
	}
	// 改写文件后（内容与尺寸都变）缓存必须失效
	img := image.NewRGBA(image.Rect(0, 0, 77, 55))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if w3, h3 := imageDimCached(p); w3 != 77 || h3 != 55 {
		t.Errorf("改写原图后仍返回旧尺寸 %d,%d，期望 77,55 —— 缓存键没含 mtime/size", w3, h3)
	}
}

// TestL1EStatCheckPresent 契约：ffmpeg 图片分支必须有 stat+size 校验。
func TestL1EStatCheckPresent(t *testing.T) {
	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))
	if !strings.Contains(th, "thumbUseFFmpegForImage(path)") {
		t.Fatal("找不到 ffmpeg 引擎分流点")
	}
	i := strings.Index(th, "thumbUseFFmpegForImage(path)")
	seg := th[i:]
	if j := strings.Index(seg, "tryVipsImageThumb"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "os.Stat(ftmp)") || !strings.Contains(seg, "st.Size() > 0") {
		t.Error("ffmpeg 图片分支缺少 stat + 非空校验 —— 空产物会进缓存并永久命中")
	}
}

// TestL1BExtractFrameReturnsDim 契约：extractFrame 返回原图尺寸，调用方不再重复读头。
func TestL1BExtractFrameReturnsDim(t *testing.T) {
	ff := stripLineComments(readSourceOrSkip(t, "ffmpeg.go"))
	if !strings.Contains(ff, "func extractFrame(ctx context.Context, path string, size int, out string, seek bool) (int, int, error)") {
		t.Error("extractFrame 没有返回原图尺寸 —— 调用方还得再读一次文件头")
	}
	if !strings.Contains(ff, "imageDimCached(path)") {
		t.Error("extractFrame 没有用缓存的尺寸探测")
	}
}
