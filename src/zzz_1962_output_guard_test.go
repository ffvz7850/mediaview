package main

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// 1.8.162 审查 → 回归测试（只在本审查副本里跑，不进交付包）
//
// 依赖外部提供的假 ffmpeg：
//   FAKE_FFMPEG=<假 ffmpeg 路径>
//   FAKE_LOG=<调用记录文件>
//   FAKE_MODE=empty | partial | jpeg | fail | ok
//
// 探针边界（一）：Windows 上 thumbPathFor() 把源文件绝对路径（含 "C:"）拼进缓存
// 目录名，mkdir 必失败（真机 Linux 是 /vol1/... 无此问题）。要端到端缩略图路径的
// 断言改用「rooted 相对路径」形态（"/tmp/..."），见 probeSrcPath。
//
// 探针边界（二）：刚编译出的假 exe 可能被 Defender 短暂锁定，第一次 exec 报
// Permission denied —— 表现为 cmd.Run() 失败 → 走回退分支，会被误读成"已回退、无此问题"。
// 所以跑之前先手动执行一次假 exe 确认可用（本目录已记录该坑）。
// ============================================================================

func probeSetup(t *testing.T) string {
	if os.Getenv("FAKE_FFMPEG") == "" {
		t.Skip("未提供 FAKE_FFMPEG")
	}
	dir := t.TempDir()
	workDir = dir
	appDest = ""
	settingsMu.Lock()
	settings = defaultSettings()
	settings.ThumbEnabled = true
	settings.ThumbDir = ""
	settings.ThumbQuality = 80
	settings.ThumbSize = 320
	settings.ThumbEngine = "auto"
	settings.PreloadConcurrency = 0
	settings.ViewerQuality = 0
	settings.ViewerLowres = 0
	settingsMu.Unlock()
	applyThumbRoot()
	ffmpegPath = os.Getenv("FAKE_FFMPEG")
	ffprobePath = ""
	// ★ 自检：假 ffmpeg 必须真的可执行、真的产出文件。
	// 否则 cmd.Run() 失败 → 走回退分支 → 探针会静默变成"没问题"（本次就踩过：
	// FAKE_FFMPEG 指到一个不存在的路径，5 个探针全部假绿/假红）。
	selfOut := filepath.Join(dir, "selfcheck_probe.jpg")
	if err := exec.Command(ffmpegPath, "-y", selfOut).Run(); err != nil {
		t.Fatalf("假 ffmpeg 不可执行（%s）：%v —— 检查路径/Defender 锁定", ffmpegPath, err)
	}
	if _, serr := os.Stat(selfOut); serr != nil {
		t.Fatalf("假 ffmpeg 没产出文件（%s）—— 检查 FAKE_MODE", selfOut)
	}
	_ = os.Remove(selfOut)
	thumbFailMu.Lock()
	thumbFailMap = map[string]time.Time{}
	thumbFailMu.Unlock()
	return dir
}

// probeSrcPath 造一条「rooted 但不带盘符」的源文件路径，绕开 Windows 上 thumbPathFor 的限制
func probeSrcPath(t *testing.T, name string) string {
	dir := "/tmp/mediaview-audit-probe"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("建不了 %s: %v", dir, err)
	}
	p := dir + "/" + name
	if err := os.WriteFile(p, []byte("probe payload"), 0o644); err != nil {
		t.Skipf("写不了 %s: %v", p, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skipf("Windows 上 %s 不可见: %v", p, err)
	}
	return p
}

func fakeCalls(t *testing.T) int {
	b, err := os.ReadFile(os.Getenv("FAKE_LOG"))
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func resetFakeLog() { _ = os.Remove(os.Getenv("FAKE_LOG")) }

// setMode 让每个用例自己选假 ffmpeg 的行为（子进程继承本进程环境）
func setMode(m string) { _ = os.Setenv("FAKE_MODE", m) }

func makeJPEG(t *testing.T, p string, w, h int) int64 {
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x += 8 {
		for y := 0; y < h; y += 8 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	st, _ := os.Stat(p)
	return st.Size()
}

// ---------------------------------------------------------------------------
// A. 改了「大图画质 / lowres 档位」必须重新转码
// ---------------------------------------------------------------------------
func Test1962ScaledCacheMustFollowQualitySetting(t *testing.T) {
	setMode("jpeg")
	dir := probeSetup(t)
	resetFakeLog()
	src := filepath.Join(dir, "q.jpg")
	_ = makeJPEG(t, src, 2600, 1800)

	serveScaledImage(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/raw?path=x&maxdim=2048", nil), src, 2048)
	n1 := fakeCalls(t)

	settingsMu.Lock()
	settings.ViewerQuality = 95 // 用户在设置里调高画质
	settingsMu.Unlock()

	serveScaledImage(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/raw?path=x&maxdim=2048", nil), src, 2048)
	n2 := fakeCalls(t)
	t.Logf("改画质前 ffmpeg 调用 %d 次，改后累计 %d 次", n1, n2)
	if n1 < 1 {
		t.Fatalf("第一张就没跑 ffmpeg（假 ffmpeg 未生效？）")
	}
	if n2 == n1 {
		t.Errorf("★ 画质改了，缩放产物仍命中旧缓存（ffmpeg 未再调用）—— 服务端不重转，" +
			"浏览器侧还带 immutable max-age=1年，用户在设置里调画质完全无效")
	}
}

func Test1962ScaledCacheMustFollowLowresSetting(t *testing.T) {
	setMode("jpeg")
	dir := probeSetup(t)
	resetFakeLog()
	// 用 4000x3000：maxdim=2048 时 lowresFor 自动档 = 1，
	// 改成固定档 3 后**有效档位真的变化** → 产物必须重算。
	// （若用 2600x1800，自动档与"关闭"都是 0，档位没变、命中缓存才是正确行为。）
	src := filepath.Join(dir, "lr.jpg")
	_ = makeJPEG(t, src, 4000, 3000)

	serveScaledImage(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/raw?path=x&maxdim=2048", nil), src, 2048)
	n1 := fakeCalls(t)

	settingsMu.Lock()
	settings.ViewerLowres = 3 // 用户指定固定档位
	settingsMu.Unlock()

	serveScaledImage(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/api/raw?path=x&maxdim=2048", nil), src, 2048)
	n2 := fakeCalls(t)
	t.Logf("改 lowres 前 %d 次，改后累计 %d 次", n1, n2)
	if n2 == n1 {
		t.Errorf("★ lowres 档位改了，缩放产物仍命中旧缓存 —— 同上")
	}
}

// ---------------------------------------------------------------------------
// B. 底层抽帧：产物 0 字节不得当成成功
// ---------------------------------------------------------------------------
func Test1962ExtractFrameRejectsEmptyProduct(t *testing.T) {
	setMode("empty")
	probeSetup(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "v.mp4")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	out := filepath.Join(dir, "f.jpg")
	_, _, err := extractVideoFrame(context.Background(), src, 320, out)
	st, serr := os.Stat(out)
	size := int64(-1)
	if serr == nil {
		size = st.Size()
	}
	t.Logf("extractVideoFrame: err=%v size=%d", err, size)
	if err == nil && size == 0 {
		t.Errorf("★ 0 字节产物被当成成功（err=nil, size=0）。同文件的 VAAPI 路径有「存在+非空+可解码」" +
			"三档校验，软件路径一档都没有 —— 两条路径不对称，而缺校验的是默认路径")
	}
}

// ---------------------------------------------------------------------------
// C. 视频/兜底缩略图：空产物必须报错，且要记负缓存
// ---------------------------------------------------------------------------
func Test1962VideoThumbRejectsEmptyProduct(t *testing.T) {
	setMode("empty")
	probeSetup(t)
	resetFakeLog()
	src := probeSrcPath(t, "clip.mp4")

	out, err := ensureThumb(src, "video", 320)
	if err == nil {
		var sz int64 = -1
		if st, e := os.Stat(out); e == nil {
			sz = st.Size()
		}
		t.Errorf("★ 空产物被当成功返回：out=%s size=%d —— 文件管理器/网格会拿到一张 0 字节的破图", out, sz)
	} else {
		t.Logf("正确：空产物被拒绝（err=%v）", err)
	}

	// 说明：负缓存**只对后台预生成生效**（前台是用户正在等的那张，设计上必须再试一次，
	// 见 ensureThumbInternal 的注释）。所以这里不断言调用次数，只把观察值记下来：
	// 真正的代价是"前景请求遇到注定失败的文件时每次都重烧一次解码"，作为 P3 单列。
	resetFakeLog()
	_, _ = ensureThumb(src, "video", 320)
	t.Logf("观察：前台第二次请求仍调用 ffmpeg %d 次（设计如此：前台不查负缓存）", fakeCalls(t))
}

// ---------------------------------------------------------------------------
// D. /api/raw：空产物必须回退原图，绝不能把 0 字节当图片返回
// ---------------------------------------------------------------------------
func Test1962ScaledFallsBackToOriginalOnEmptyProduct(t *testing.T) {
	setMode("empty")
	dir := probeSetup(t)
	resetFakeLog()
	src := filepath.Join(dir, "big.jpg")
	origSize := makeJPEG(t, src, 2600, 1800)

	rec := httptest.NewRecorder()
	serveScaledImage(rec, httptest.NewRequest("GET", "/api/raw?path=x&maxdim=2048", nil), src, 2048)
	body := rec.Body.Len()
	t.Logf("code=%d body=%d 原图=%d", rec.Code, body, origSize)
	if body == 0 {
		t.Errorf("★ 把一个 0 字节转码产物直接返回给客户端（HTTP %d、body=0）—— 浏览器得到破图，且不回退原图", rec.Code)
	} else if int64(body) != origSize {
		t.Errorf("★ 转码产物无效时没有回退原图：body=%d，原图=%d", body, origSize)
	}
}

// ---------------------------------------------------------------------------
// E. 回归守卫：兜底分支的 rename 之前必须有产物校验
// ---------------------------------------------------------------------------
func Test1962FallbackBranchValidatesProduct(t *testing.T) {
	b, err := os.ReadFile("thumb.go")
	if err != nil {
		t.Skip("读不到 thumb.go")
	}
	code := string(b)
	i := strings.Index(code, "video 或 Go/vips 都解不了的图片格式：交给 ffmpeg")
	if i < 0 {
		t.Fatal("定位不到兜底分支")
	}
	j := strings.Index(code[i:], "return \"\", errNoFFmpeg")
	if j < 0 {
		t.Fatal("定位不到兜底分支结尾")
	}
	branch := code[i : i+j]

	// 1.8.164：原来只写 `strings.Contains(branch, ".Size()")` —— 太弱：
	// 只要分支里**任何地方**出现过 ".Size()" 就绿，把校验挪到 rename 之后、
	// 或写成 `if false { _ = st.Size() }` 都能骗过它（等于假绿）。
	// 改成结构性断言：必须**真的有** Stat 判空，且**必须早于** rename。
	iStat := strings.Index(branch, "os.Stat(")
	iSize := strings.Index(branch, ".Size() == 0")
	iRename := strings.Index(branch, "os.Rename(")
	if iStat < 0 || iSize < 0 {
		t.Errorf("★ generateThumb 的兜底 ffmpeg 分支（视频 / HEIC 走的就是这条）缺少产物判空："+
			"需要 `os.Stat(...)` 且判 `.Size() == 0`（iStat=%d iSize=%d）", iStat, iSize)
	}
	if iRename < 0 {
		t.Fatalf("定位不到该分支里的 os.Rename(")
	}
	if iStat >= 0 && iStat > iRename {
		t.Errorf("★ 产物判空排在 os.Rename 之后（iStat=%d > iRename=%d）—— "+
			"那时坏产物已经被 rename 进缓存并返回给调用方了", iStat, iRename)
	}
	if iSize >= 0 && iSize > iRename {
		t.Errorf("★ `.Size() == 0` 判定排在 os.Rename 之后（iSize=%d > iRename=%d）", iSize, iRename)
	}
	if !strings.Contains(branch, "errEmptyProduct") {
		t.Errorf("★ 空产物没有用专用哨兵 errEmptyProduct（用 errNoFFmpeg 会让日志说成" +
			"\"ffmpeg 不可用\"，把排障带偏）")
	}
}
