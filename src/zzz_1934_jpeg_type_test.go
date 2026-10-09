package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJpegHead 写一个只含「文件头」的最小 JPEG，用于验证 SOF 类型判断。
func writeJpegHead(t *testing.T, name string, head []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, head, 0o644); err != nil {
		t.Fatalf("写样张失败: %v", err)
	}
	return p
}

// TestJpegIsProgressive 守住「渐进式 JPEG 判断」。
//
// 这个判断是 auto 引擎分流的依据：渐进式 → ffmpeg（vips 的 shrink-on-load 失效），
// 普通 baseline → vips（略快）。判错会导致该走 ffmpeg 的图走了 vips（慢且吃不到 -lowres）。
func TestJpegIsProgressive(t *testing.T) {
	soi := []byte{0xFF, 0xD8}
	// SOFn 段：marker + 长度(0x000B=11) + 精度1 + 高2 + 宽2 + 分量1 + ...
	sofBody := []byte{0x00, 0x0B, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01, 0x11, 0x00}
	sof := func(m byte) []byte {
		b := append([]byte{0xFF, m}, sofBody...)
		return append(append([]byte{}, soi...), b...)
	}

	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"baseline SOF0 (0xC0)", sof(0xC0), false},
		{"extended sequential SOF1 (0xC1)", sof(0xC1), false},
		{"progressive SOF2 (0xC2)", sof(0xC2), true},
		{"lossless SOF3 (0xC3)", sof(0xC3), false},
		{"differential progressive SOF6 (0xC6)", sof(0xC6), true},
		{"progressive arithmetic SOF10 (0xCA)", sof(0xCA), true},
		{"dif. progressive arithmetic SOF14 (0xCE)", sof(0xCE), true},
		// 0xC4(DHT)/0xC8(JPG)/0xCC(DAC) 不是 SOFn，必须跳过继续找
		{"DHT(0xC4) 前缀后跟 SOF2", append(append(append([]byte{}, soi...),
			[]byte{0xFF, 0xC4, 0x00, 0x04, 0x00, 0x00}...), sof(0xC2)[2:]...), true},
		{"DAC(0xCC) 前缀后跟 SOF0", append(append(append([]byte{}, soi...),
			[]byte{0xFF, 0xCC, 0x00, 0x04, 0x00, 0x00}...), sof(0xC0)[2:]...), false},
		// 非 JPEG
		{"PNG 头", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, false},
		// 截断
		{"只有 SOI", soi, false},
		{"空文件", []byte{}, false},
	}
	for _, c := range cases {
		p := writeJpegHead(t, "t.jpg", c.head)
		if got := jpegIsProgressive(p); got != c.want {
			t.Errorf("%s: jpegIsProgressive = %v，期望 %v", c.name, got, c.want)
		}
	}
	// 不存在的文件不能 panic
	if jpegIsProgressive(filepath.Join(t.TempDir(), "nope.jpg")) {
		t.Error("不存在的文件被判为 progressive")
	}
}

// TestIsJPEGPath 扩展名判断。
func TestIsJPEGPath(t *testing.T) {
	for _, p := range []string{"a.jpg", "a.JPG", "b.jpeg", "c.jfif", "d.jpe", "/x/Y.JPEG"} {
		if !isJPEGPath(p) {
			t.Errorf("isJPEGPath(%q) = false，期望 true", p)
		}
	}
	for _, p := range []string{"a.png", "b.gif", "c.webp", "d.heic", "e", "f.mp4"} {
		if isJPEGPath(p) {
			t.Errorf("isJPEGPath(%q) = true，期望 false", p)
		}
	}
}

// TestThumbUseFFmpegForImage 守住 auto 的分流规则。
func TestThumbUseFFmpegForImage(t *testing.T) {
	soi := []byte{0xFF, 0xD8}
	body := []byte{0x00, 0x0B, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01, 0x11, 0x00}
	mk := func(marker byte, ext string) string {
		head := append(append(append([]byte{}, soi...), 0xFF, marker), body...)
		return writeJpegHead(t, "s"+ext, head)
	}
	prog := mk(0xC2, ".jpg")
	base := mk(0xC0, ".jpg")
	png := writeJpegHead(t, "p.png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	settingsMu.Lock()
	old := settings.ThumbEngine
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbEngine = old
		settingsMu.Unlock()
	})

	set := func(v string) {
		settingsMu.Lock()
		settings.ThumbEngine = v
		settingsMu.Unlock()
	}

	// auto 的语义在 1.8.137 变了（P1-1）：**lowres 开启时全部交给 ffmpeg**。
	//   实测（320 档）：baseline 上 ffmpeg+lowres 109ms vs vips 243ms（快 2.2×）；
	//   渐进式上 1390ms vs 1658ms（快 19%）；而只有 ffmpeg 能吃 -lowres。
	//   所以"按类型分流给 vips"会白丢 2.2×。只有 lowres 关闭时才回到按类型分流。
	settingsMu.Lock()
	oldLow := settings.ThumbLowres
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ThumbLowres = oldLow
		settingsMu.Unlock()
	})

	set("auto")
	settingsMu.Lock()
	settings.ThumbLowres = true
	settingsMu.Unlock()
	if !thumbUseFFmpegForImage(prog) || !thumbUseFFmpegForImage(base) || !thumbUseFFmpegForImage(png) {
		t.Error("auto + lowres 开启时应**全部**交给 ffmpeg —— 只有它能吃 -lowres，" +
			"分给 vips 会白丢 2.2×（baseline 实测）")
	}

	settingsMu.Lock()
	settings.ThumbLowres = false
	settingsMu.Unlock()
	if !thumbUseFFmpegForImage(prog) {
		t.Error("lowres 关闭时渐进式 JPEG 仍应交给 ffmpeg —— vips 的 shrink 在渐进式上失效")
	}
	if thumbUseFFmpegForImage(base) {
		t.Error("lowres 关闭时 baseline JPEG 不该交给 ffmpeg —— 那时 vips 略快")
	}
	if thumbUseFFmpegForImage(png) {
		t.Error("lowres 关闭时 PNG 不该交给 ffmpeg")
	}

	// ffmpeg：一律交
	set("ffmpeg")
	if !thumbUseFFmpegForImage(base) || !thumbUseFFmpegForImage(png) {
		t.Error("引擎=ffmpeg 时没有把所有图片交给 ffmpeg")
	}

	// vips：不主动交
	set("vips")
	if thumbUseFFmpegForImage(prog) || thumbUseFFmpegForImage(base) {
		t.Error("引擎=vips 时仍把图片交给了 ffmpeg")
	}
}
