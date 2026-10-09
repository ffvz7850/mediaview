package main

// 1.8.17 服务端缩放链路的验证测试。
// 直接调 serveScaledImage / handleRaw（手动注入 ffmpegPath），绕过路径白名单与进程启动。
// 用法：go test -run 'TestWithinMaxdim|TestCanSkipScale|TestScaleOnlyShrinks|TestScaled|TestHandleRaw|TestNonNativeImage|TestBrowserNative' -v .

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testFFmpeg = `D:\PCAPP\DBAO\mediaview-fnos\_smoke\ffmpeg.exe`

func mkJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 7 % 256), uint8(y * 5 % 256), uint8((x + y) % 256), 255})
		}
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 88}); err != nil {
		t.Fatal(err)
	}
}

// mkNoisePNG 生成体积很大的 PNG（噪声压不动），用于验证体积阈值
func mkNoisePNG(t *testing.T, path string, w, h int) int64 {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(42))
	for i := range img.Pix {
		img.Pix[i] = uint8(rnd.Intn(256))
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	return st.Size()
}

func dims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode config %s: %v", path, err)
	}
	return cfg.Width, cfg.Height
}

func scaleOnce(t *testing.T, path string, maxdim int) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/raw?path=x&maxdim="+strconv.Itoa(maxdim), nil)
	serveScaledImage(rec, req, path, maxdim)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	return rec.Body.Len(), rec.Body.Bytes()
}

func scaledArtifactPath(path string, maxdim int) string {
	info, _ := os.Stat(path)
	// 1.8.162：产物名新增画质与 lowres 档位两维，改用生产代码里的同一个构造函数
	// （名字里少一维 = 用户改了设置不生效，见 media.go 的 scaledParams 注释）
	q, lr := scaledParams(path, maxdim)
	return scaledOutPath(cacheKey(path, info), maxdim, q, lr)
}

// --- 1) withinMaxdim：只读文件头，尺寸判定正确 ---

func TestWithinMaxdim(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "small.jpg")
	mkJPEG(t, jp, 800, 600)
	pnp := filepath.Join(dir, "shot.png")
	mkNoisePNG(t, pnp, 300, 200)

	if !withinMaxdim(jp, 4096) {
		t.Error("800x600 jpg 应判定为不超过 4096")
	}
	if !withinMaxdim(jp, 800) {
		t.Error("800x600 在 maxdim=800 时应判定为不超过")
	}
	if withinMaxdim(jp, 799) {
		t.Error("800x600 在 maxdim=799 时不应判定为不超过")
	}
	if !withinMaxdim(pnp, 4096) {
		t.Error("png 解码器未注册？300x200 png 应判定为不超过 4096")
	}

	start := time.Now()
	for i := 0; i < 20; i++ {
		withinMaxdim(jp, 4096)
	}
	avg := time.Since(start) / 20
	if avg > 20*time.Millisecond {
		t.Errorf("withinMaxdim 太慢：%v", avg)
	}
	t.Logf("withinMaxdim 平均耗时 %v", avg)

	junk := filepath.Join(dir, "x.bin")
	os.WriteFile(junk, []byte("not an image"), 0o644)
	if withinMaxdim(junk, 4096) {
		t.Error("非图片文件应返回 false")
	}
	if withinMaxdim(filepath.Join(dir, "nope.jpg"), 4096) {
		t.Error("不存在的文件应返回 false")
	}
}

// --- 2) canSkipScale：只看尺寸是否超 maxdim ---

func TestCanSkipScale(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.jpg")
	mkJPEG(t, small, 800, 600)
	st, _ := os.Stat(small)
	t.Logf("小图体积 %d B", st.Size())

	if !canSkipScale(small, st.Size(), 4096) {
		t.Error("800x600 小 jpg 应可跳过转码")
	}
	if canSkipScale(small, st.Size(), 400) {
		t.Error("800x600 在 maxdim=400 时不应跳过（需要缩小）")
	}
	if canSkipScale(small, 0, 4096) {
		t.Error("size<=0 不应跳过")
	}
	// 体积不再参与判断：尺寸不超 maxdim 就必须直出。
	// 否则会走一次"尺寸恒等"的 JPEG 二次编码 —— 画质白掉一档，而前端看不到
	// naturalWidth 变化，会认为"后端没缩过"→ 放大时永不升级，用户一直看糊图。
	if !canSkipScale(small, 8<<20, 4096) {
		t.Error("体积大不该成为转码理由：尺寸不超 maxdim 时应直出")
	}

	bigPNG := filepath.Join(dir, "big.png")
	sz := mkNoisePNG(t, bigPNG, 1600, 1200)
	t.Logf("噪声 PNG 体积 %d B", sz)
	if !canSkipScale(bigPNG, sz, 4096) {
		t.Error("1600x1200 在 maxdim=4096 下尺寸不超，应可跳过转码")
	}
	if canSkipScale(bigPNG, sz, 1024) {
		t.Error("1600x1200 在 maxdim=1024 下必须转码")
	}
}

// --- 3) scaleOnlyShrinks：小图走转码分支时也不得被放大 ---

func TestScaleOnlyShrinks(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()

	cases := []struct{ w, h, maxdim int }{
		{800, 600, 4096},
		{1200, 800, 4096},
		{2000, 3000, 1024},
		{3000, 1000, 1024},
	}
	for _, c := range cases {
		p := filepath.Join(dir, "case.jpg")
		mkJPEG(t, p, c.w, c.h)
		n, _ := scaleOnce(t, p, c.maxdim)
		out := scaledArtifactPath(p, c.maxdim)
		w, h := dims(t, out)
		longest := w
		if h > w {
			longest = h
		}
		if longest > c.maxdim {
			t.Errorf("%dx%d maxdim=%d → 输出 %dx%d 超过上限", c.w, c.h, c.maxdim, w, h)
		}
		if c.w <= c.maxdim && c.h <= c.maxdim && (w != c.w || h != c.h) {
			t.Errorf("%dx%d maxdim=%d → 输出被改成 %dx%d（应保持原尺寸，不允许放大/重采样）",
				c.w, c.h, c.maxdim, w, h)
		}
		t.Logf("%dx%d maxdim=%-5d → %dx%d  (%d B)", c.w, c.h, c.maxdim, w, h, n)
		os.Remove(out)
	}
}

// --- 4) cacheHit：第二次请求必须命中缓存 ---

func TestScaledCacheHit(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()
	p := filepath.Join(dir, "big.jpg")
	mkJPEG(t, p, 3000, 2000)
	out := scaledArtifactPath(p, 1024)
	if !strings.Contains(filepath.Base(out), "mediaview-scaled-"+scaledCacheTag+"-") {
		t.Errorf("产物名未带算法版本标记：%s", filepath.Base(out))
	}
	os.Remove(out)
	defer os.Remove(out)

	t0 := time.Now()
	scaleOnce(t, p, 1024)
	first := time.Since(t0)
	st1, err := os.Stat(out)
	if err != nil {
		t.Fatalf("产物未生成: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)

	t1 := time.Now()
	scaleOnce(t, p, 1024)
	second := time.Since(t1)
	st2, _ := os.Stat(out)

	t.Logf("首次 %v（产物 %d B） / 第二次 %v", first, st1.Size(), second)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("第二次请求重新生成了文件 → 缓存未命中")
	}
	if st2.Size() == 0 {
		t.Error("产物为空")
	}
	if second >= first {
		t.Errorf("第二次没有变快：%v vs %v", first, second)
	}
}

// --- 5) 并发：同一张图被并发请求，不得产出半成品或 panic ---

func TestScaledConcurrent(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()
	p := filepath.Join(dir, "c.jpg")
	mkJPEG(t, p, 2400, 1600)
	out := scaledArtifactPath(p, 1024)
	os.Remove(out)
	defer os.Remove(out)

	var wg sync.WaitGroup
	errs := make(chan string, 16)
	sizes := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/api/raw?path=x&maxdim=1024", nil)
			func() {
				defer func() {
					if r := recover(); r != nil {
						errs <- "panic: " + toStr(r)
					}
				}()
				serveScaledImage(rec, req, p, 1024)
			}()
			if rec.Code != 200 {
				errs <- "status " + strconv.Itoa(rec.Code)
			}
			sizes[i] = rec.Body.Len()
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	for i, s := range sizes {
		if s == 0 {
			t.Errorf("第 %d 个并发请求拿到空响应", i)
		}
	}
	if w, h := dims(t, out); w == 0 || h == 0 {
		t.Error("最终产物不可解码")
	} else {
		t.Logf("并发 8 次后产物 %dx%d，各响应长度 %v", w, h, sizes)
	}
}

// --- 6) 端到端：真实请求路径下，小图完全不碰 ffmpeg ---

func TestHandleRawEndToEnd(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", dir)

	small := filepath.Join(dir, "small.jpg")
	mkJPEG(t, small, 1200, 800)
	smallSt, _ := os.Stat(small)

	rec := httptest.NewRecorder()
	handleRaw(rec, httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(small)+"&maxdim=3840", nil))
	if rec.Code != 200 {
		t.Fatalf("小图状态码 %d", rec.Code)
	}
	if int64(rec.Body.Len()) != smallSt.Size() {
		t.Errorf("小图响应 %d B != 源文件 %d B → 没走直出，仍被 ffmpeg 重编码",
			rec.Body.Len(), smallSt.Size())
	}
	t.Logf("小图 1200x800 (%d B) → 直出 %d B  CT=%s",
		smallSt.Size(), rec.Body.Len(), rec.Result().Header.Get("Content-Type"))

	big := filepath.Join(dir, "big.jpg")
	mkJPEG(t, big, 3000, 2000)
	bigSt, _ := os.Stat(big)
	rec2 := httptest.NewRecorder()
	handleRaw(rec2, httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(big)+"&maxdim=1024", nil))
	if rec2.Code != 200 {
		t.Fatalf("大图状态码 %d", rec2.Code)
	}
	out := scaledArtifactPath(big, 1024)
	w, h := dims(t, out)
	if w > 1024 || h > 1024 {
		t.Errorf("大图产物 %dx%d 未缩到 1024 以内", w, h)
	}
	if int64(rec2.Body.Len()) == bigSt.Size() {
		t.Error("大图响应与源文件一样大 → 没走转码")
	}
	t.Logf("大图 3000x2000 (%d B) → 转码 %dx%d 响应 %d B", bigSt.Size(), w, h, rec2.Body.Len())
	os.Remove(out)

	// 尺寸不超 maxdim 的大体积 PNG：应当【直出】原文件。
	// 曾经这里断言"仍走转码（弱网保护）"，但那条保护的实际代价是——
	// 尺寸恒等却做一次二次编码，画质白掉一档，且前端看不出被重编码过，
	// 放大时不会升级到原图，用户只能一直看这张掉了画质的图。
	huge := filepath.Join(dir, "huge.png")
	mkNoisePNG(t, huge, 1600, 1200)
	hugeSt, _ := os.Stat(huge)
	rec3 := httptest.NewRecorder()
	handleRaw(rec3, httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(huge)+"&maxdim=3840", nil))
	if rec3.Code != 200 {
		t.Fatalf("大体积 PNG 状态码 %d", rec3.Code)
	}
	if int64(rec3.Body.Len()) != hugeSt.Size() {
		t.Errorf("1600x1200 未超 maxdim=3840 应直出原文件（%d B），实际 %d B",
			hugeSt.Size(), rec3.Body.Len())
	}
	t.Logf("大体积 PNG 1600x1200 (%d B，未超 maxdim) → 直出原文件 ✓", hugeSt.Size())
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		return "unknown"
	}
}

// ===========================================================================
// 7) 浏览器原生解码能力边界（HEIC/TIFF 必须转码）
// ===========================================================================
//
// 1.8.22 起设置页的「源文件直出」开关已移除，因此这里不再有 setRawOriginal。
// 原 TestRawOriginalSwitch 覆盖的等价语义改由别处承担：
//   - 「尺寸不超 maxdim 的图直出、超了必须缩」 → TestCanSkipScale / TestHandleRaw
//   - 「真实大照片必须被缩到 maxdim 内」       → TestZDriveRealPhotoEndToEnd

// rawGet 走真实请求路径（handleRaw），maxdim<=0 表示不带该参数。
func rawGet(t *testing.T, path string, maxdim int) *httptest.ResponseRecorder {
	t.Helper()
	u := "/api/raw?path=" + url.QueryEscape(path)
	if maxdim > 0 {
		u += "&maxdim=" + strconv.Itoa(maxdim)
	}
	rec := httptest.NewRecorder()
	handleRaw(rec, httptest.NewRequest("GET", u, nil))
	return rec
}

// hasFile 判断路径存在且非空（不复用 ffmpeg.go 的同名函数，签名不同）。
func hasFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// genTIFF 用 ffmpeg 把 src 转成 TIFF，用于验证"浏览器不支持的格式仍转码"。
func genTIFF(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-i", src, "-y", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 无法生成 TIFF（%v）：%s", err, out)
	}
}

// TestBrowserNativeImage 浏览器解码能力判定：HEIC/TIFF 是必须转码的例外。
func TestBrowserNativeImage(t *testing.T) {
	for _, e := range []string{".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp", ".svg", ".jfif"} {
		if !browserNativeImage(e) {
			t.Errorf("%s 浏览器可解码，应允许直出", e)
		}
	}
	// 这几类若直出会从"能看"变成"打不开"，必须留在转码分支
	for _, e := range []string{".heic", ".heif", ".tif", ".tiff"} {
		if browserNativeImage(e) {
			t.Errorf("%s 浏览器不支持，不应直出", e)
		}
	}
}

// TestNonNativeImageMustTranscode 浏览器解不了的格式（TIFF）无论请求怎么发都必须转码。
// 1.8.22 移除「源文件直出」开关后，这条约束的强度不变 —— 它守的是"能看 vs 打不开"。
func TestNonNativeImageMustTranscode(t *testing.T) {
	if _, err := os.Stat(testFFmpeg); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	ffmpegPath = testFFmpeg
	dir := t.TempDir()
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", dir)

	jpg := filepath.Join(dir, "big.jpg")
	mkJPEG(t, jpg, 3000, 2000)
	tif := filepath.Join(dir, "scan.tif")
	genTIFF(t, jpg, tif)
	tifSrc, _ := os.ReadFile(tif)
	tifOut := scaledArtifactPath(tif, 1024)
	os.Remove(tifOut)

	// (a) 带 maxdim 的正常请求：必须转成 JPEG，且缩到 maxdim 内
	recTif := rawGet(t, tif, 1024)
	if recTif.Code != 200 {
		t.Fatalf("TIFF 状态码 %d", recTif.Code)
	}
	if bytes.Equal(recTif.Body.Bytes(), tifSrc) {
		t.Error("TIFF 被直出了 → 浏览器无法显示，应走转码")
	}
	if ct := recTif.Result().Header.Get("Content-Type"); !strings.Contains(ct, "image/jpeg") {
		t.Errorf("TIFF 转码后 Content-Type 应为 image/jpeg，实际 %q", ct)
	}
	t.Logf("TIFF %d B → 转码为 JPEG %d B（maxdim=1024）", len(tifSrc), recTif.Body.Len())
	os.Remove(tifOut)

	// (b) 不带 maxdim 的裸请求（手工拼 URL 的防御场景）：同样转码，
	//     且兜底尺寸取 transcodeFallbackMaxdim(4096)，3000x2000 不该被缩小。
	recBare := rawGet(t, tif, 0)
	if recBare.Code != 200 {
		t.Fatalf("TIFF 裸请求状态码 %d", recBare.Code)
	}
	if bytes.Equal(recBare.Body.Bytes(), tifSrc) {
		t.Error("无 maxdim 时 TIFF 被直出了 → 应走兜底转码")
	}
	if w, h := imgDimsBytes(t, recBare.Body.Bytes()); w != 3000 || h != 2000 {
		t.Errorf("兜底转码不该缩小原尺寸：期望 3000x2000，实际 %dx%d", w, h)
	}
	t.Logf("TIFF 裸请求（无 maxdim）→ JPEG %d B（尺寸保持 3000x2000）", recBare.Body.Len())
}
