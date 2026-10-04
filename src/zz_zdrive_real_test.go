package main

// 用 Z 盘真实照片验证大图缩放链路。
// 1.8.22 起「源文件直出」开关已移除：大图一律由服务端 ffmpeg 用 lanczos 降采样。
// Z 盘或 ffmpeg 不可用时自动跳过，不影响 CI。

import (
	"bytes"
	"image"
	_ "image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imgDimsBytes 从内存里的图片字节读出宽高（测试辅助）。
func imgDimsBytes(t *testing.T, b []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode config from bytes: %v", err)
	}
	return cfg.Width, cfg.Height
}

const (
	zRealJPEG  = `Z:\压缩旅游不带视频\2025.3压缩\IMG_5164.JPG`
	zRealJPEG2 = `Z:\压缩旅游不带视频\班戈(1)\DSC00981.JPG`
)

// TestZDriveRealPhotoEndToEnd 真实照片（长边远超 maxdim）走默认路径时，
// 必须由服务端 ffmpeg 缩到 maxdim 内，响应不得等于原文件。
//
// 为什么这条重要：1.8.22 之前有个「源文件直出」开关，打开后这类 6000+ 像素的
// 原图会被原样吐给浏览器。实测显示，让 Chromium 自己降采样比 ffmpeg lanczos
// 明显更糊（同一张 DSC09993、适应窗口：直出锐度 5370.8 vs 预缩 5845.6）。
// 开关已移除，这条用例守住"大图不再直出"这个结论。
func TestZDriveRealPhotoEndToEnd(t *testing.T) {
	if _, err := os.Stat(zRealJPEG); err != nil {
		t.Skip("Z 盘照片不可用，跳过")
	}
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", `Z:\压缩旅游不带视频`)

	src, err := os.ReadFile(zRealJPEG)
	if err != nil {
		t.Fatalf("读原文件失败: %v", err)
	}
	sw, sh := imgDimsBytes(t, src)
	t.Logf("真实照片 %s：%d 字节，%dx%d", filepath.Base(zRealJPEG), len(src), sw, sh)

	out := scaledOutPath(zRealJPEG, 2400)
	os.Remove(out)
	rec := rawGet(t, zRealJPEG, 2400)
	if rec.Code != 200 {
		t.Fatalf("状态码 %d", rec.Code)
	}
	if bytes.Equal(rec.Body.Bytes(), src) {
		t.Errorf("★ 原图被直出了：响应 %d B == 源文件 %d B（应走 ffmpeg 缩放）", rec.Body.Len(), len(src))
	}
	w, h := dims(t, out)
	if w > 2400 || h > 2400 {
		t.Errorf("产物 %dx%d 未缩到 2400 内", w, h)
	} else {
		t.Logf("%dx%d %d B → %dx%d %d B（压缩比 %.1f%%，长边缩到 %.0f%%）",
			sw, sh, len(src), w, h, rec.Body.Len(),
			float64(rec.Body.Len())/float64(len(src))*100,
			float64(w)/float64(sw)*100)
	}
	// 长宽比必须保持（force_original_aspect_ratio=decrease，容差 1%）
	if w > 0 && h > 0 {
		got := float64(w) / float64(h)
		want := float64(sw) / float64(sh)
		if got-want > 0.01 || want-got > 0.01 {
			t.Errorf("产物长宽比 %.4f 与原图 %.4f 不符（在 1%% 容差外）", got, want)
		}
	}
}

// TestFakeHEICMustTranscode 非原生格式（heic/tiff）必须仍转码，否则浏览器显示不了。
//
// 探测方式：把一张 JPEG 改名成 .heic（后端只按扩展名判断是否原生支持）。
//   - 若响应字节 == 源文件  → 走了直出（错误：浏览器解不了 heic）
//   - 若响应字节 != 源文件  → 走了转码（正确）
func TestFakeHEICMustTranscode(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", dir)

	// 造一个「名字是 .heic、内容其实是 JPEG」的文件
	jpg := filepath.Join(dir, "real.jpg")
	mkJPEG(t, jpg, 3000, 2000)
	heic := filepath.Join(dir, "fake.heic")
	data, err := os.ReadFile(jpg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(heic, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// (a) 无 maxdim 的裸请求：兜底转码必须生效，且尺寸不被缩小
	recNoMax := rawGet(t, heic, 0)
	directNoMax := bytes.Equal(recNoMax.Body.Bytes(), data)
	ctNoMax := recNoMax.Result().Header.Get("Content-Type")
	t.Logf("裸请求（无 maxdim）：响应 %d B，直出=%v，CT=%s", recNoMax.Body.Len(), directNoMax, ctNoMax)

	// (b) 带 maxdim 时也必须转码
	recMax := rawGet(t, heic, 4096)
	directMax := bytes.Equal(recMax.Body.Bytes(), data)
	ctMax := recMax.Result().Header.Get("Content-Type")
	t.Logf("带 maxdim=4096：响应 %d B，直出=%v，CT=%s", recMax.Body.Len(), directMax, ctMax)

	if directNoMax {
		t.Errorf("无 maxdim 时不应把 .heic 原样直出（浏览器解不了），必须转码")
	}
	if !strings.HasPrefix(ctNoMax, "image/jpeg") {
		t.Errorf("HEIC 兜底转码后 Content-Type 应为 image/jpeg，实际 %q", ctNoMax)
	}
	if recNoMax.Body.Len() == 0 {
		t.Errorf("HEIC 兜底转码响应为空")
	}
	if directMax {
		t.Errorf("带 maxdim 时 .heic 也不应直出")
	}
	// 兜底转码用了 transcodeFallbackMaxdim（4096）：3000x2000 的原图不该被缩，
	// 否则「能看」的图会平白掉一档画质。
	if w, h := imgDimsBytes(t, recNoMax.Body.Bytes()); w != 3000 || h != 2000 {
		t.Errorf("兜底转码不该缩小原尺寸：期望 3000x2000，实际 %dx%d", w, h)
	}
}
