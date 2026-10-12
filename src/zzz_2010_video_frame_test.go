package main

// ============================================================================
// 2.0.1：视频缩略图的**抽帧时间点**（用户需求："很多视频第一帧都是黑屏，
// 改成取三秒后的那一帧"）
//
// 背景（代码事实，勿凭注释判断）：
//   · 原来软件路径与 VAAPI 路径各硬编码一份 `"-ss", "0.05"` —— 0.05s 实质上就是
//     第 0 帧，所以缩略图经常是黑屏。
//   · ffmpeg 的 -ss 是**时间** seek（"第几帧"＝时间×帧率，跨视频无法统一），
//     因此需求落到代码上 = "seek 到第 3 秒"。
//   · `-ss` 在 `-i` 之前（输入 seek）+ 默认 accurate_seek ⇒ 真能取到 ~3.0s 的帧；
//     若加 `-noaccurate_seek` 会退回 3 秒前的关键帧（长 GOP 视频可能退回第 0 帧）。
//
// 本文件用**假 ffmpeg 的真实 argv 日志**断言"发出去的参数"，不是读源码猜：
//   FAKE_FFMPEG / FAKE_LOG / FAKE_MODE 由 zzz_1962 的 probeSetup 统一准备，
//   本文件额外用 FAKE_MODE=seekfail + FAKE_FAIL_SEEK_GE=3 模拟
//   「-ss 越过视频末尾」——短视频抽帧的真实失败形态（ffmpeg 报
//   "Output file is empty" 并以 1 退出）。
// ============================================================================

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stripGoComments 去掉行注释（// 到行尾）。反向断言必须用剥注释后的源码 ——
// 否则"源码里没有 X"会被**自己写的注释**满足（本工程踩过，见 §二.57）。
func stripGoComments(src string) string {
	var b strings.Builder
	for _, ln := range strings.Split(src, "\n") {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	return b.String()
}

// fakeLogLines 读回假 ffmpeg 记录的全部调用（每行一次调用的完整 argv）
func fakeLogLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("FAKE_LOG"))
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// A. 时间点决策（纯函数，直接跑真代码）
// ---------------------------------------------------------------------------
func Test2010ChooseVideoSeekSec(t *testing.T) {
	cases := []struct {
		dur  float64
		want float64
		why  string
	}{
		{0, 3.0, "探不到时长 → 用默认 3s（越界由兜底重试接住）"},
		{-1, 3.0, "异常时长同样按未知处理"},
		{120, 3.0, "长视频一律 3s"},
		{3.6, 3.0, "刚过缓冲边界，仍是 3s"},
		{3.4, 1.7, "落在 3s+0.5 缓冲内 → 取中点"},
		{2.0, 1.0, "2 秒视频取 1s（避开开头黑场，也不落到末尾）"},
		{0.6, 0.3, "0.6 秒视频取 0.3s"},
		{0.06, 0.05, "极短视频夹到兜底值"},
	}
	for _, c := range cases {
		if got := chooseVideoSeekSec(c.dur); got != c.want {
			t.Errorf("chooseVideoSeekSec(%v) = %v，期望 %v —— %s", c.dur, got, c.want, c.why)
		}
	}
}

// ---------------------------------------------------------------------------
// B. 真的把 3s 发出去了（读假 ffmpeg 的 argv 日志）
// ---------------------------------------------------------------------------
func Test2010VideoThumbSeeksToThreeSeconds(t *testing.T) {
	setMode("jpeg")
	dir := probeSetup(t)
	resetFakeLog()
	src := filepath.Join(dir, "movie.mp4")
	_ = os.WriteFile(src, []byte("fake video payload"), 0o644)
	out := filepath.Join(dir, "t.jpg")

	if _, _, err := extractVideoFrame(context.Background(), src, 320, out, 12); err != nil {
		t.Fatalf("抽帧失败: %v", err)
	}
	lines := fakeLogLines(t)
	if len(lines) != 1 {
		t.Fatalf("长视频应只调用 1 次 ffmpeg，实际 %d 次：%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], " -ss 3 ") {
		t.Errorf("★ 抽帧时间点不是第 3 秒（第一帧黑屏的根因没修掉）。实际 argv：%s", lines[0])
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Errorf("抽帧成功却没有产物：err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// C. 短视频：3s 越界 → 自动回退到开头附近，且最终成功
//
//	（不加兜底的话：errEmptyProduct/exit1 → 负缓存 10 分钟 → 网格一直占位图）
//
// ---------------------------------------------------------------------------
func Test2010ShortVideoFallsBackToHead(t *testing.T) {
	setMode("seekfail")
	_ = os.Setenv("FAKE_FAIL_SEEK_GE", "3")
	dir := probeSetup(t)
	resetFakeLog()
	src := filepath.Join(dir, "clip.mp4")
	_ = os.WriteFile(src, []byte("too short"), 0o644)
	out := filepath.Join(dir, "t.jpg")

	if _, _, err := extractVideoFrame(context.Background(), src, 320, out, 0); err != nil {
		t.Fatalf("兜底重试后仍然失败: %v", err)
	}
	lines := fakeLogLines(t)
	if len(lines) != 2 {
		t.Fatalf("期望「先 3s 失败 → 回退 0.05s」共 2 次调用，实际 %d 次：%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], " -ss 3 ") {
		t.Errorf("第一次调用不是 3s：%s", lines[0])
	}
	if !strings.Contains(lines[1], " -ss 0.05 ") {
		t.Errorf("★ 3s 抽帧失败后没有回退到开头附近重抽：%s —— 短视频会整张缩略图失败", lines[1])
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Errorf("兜底成功后应有产物：err=%v", err)
	}
	_ = os.Unsetenv("FAKE_FAIL_SEEK_GE")
}

// ---------------------------------------------------------------------------
// D. 时长已知的短视频：直接选对时间点，不白跑一次 3s
// ---------------------------------------------------------------------------
func Test2010KnownShortDurationAvoidsWastedAttempt(t *testing.T) {
	setMode("seekfail")
	_ = os.Setenv("FAKE_FAIL_SEEK_GE", "3")
	dir := probeSetup(t)
	resetFakeLog()
	src := filepath.Join(dir, "clip2.mp4")
	_ = os.WriteFile(src, []byte("2 seconds"), 0o644)
	out := filepath.Join(dir, "t.jpg")

	if _, _, err := extractVideoFrame(context.Background(), src, 320, out, 2); err != nil {
		t.Fatalf("抽帧失败: %v", err)
	}
	lines := fakeLogLines(t)
	if len(lines) != 1 {
		t.Errorf("时长已知(2s)时应一次命中，实际 %d 次：%v", len(lines), lines)
	}
	if len(lines) > 0 && !strings.Contains(lines[0], " -ss 1 ") {
		t.Errorf("2 秒视频应 seek 到 1s（中点），实际：%s", lines[0])
	}
	_ = os.Unsetenv("FAKE_FAIL_SEEK_GE")
}

// ---------------------------------------------------------------------------
// E. 源码守卫：两条抽帧路径都必须用**参数化**的时间点（不许留硬编码）
// ---------------------------------------------------------------------------
func Test2010BothPathsUseParametrizedSeek(t *testing.T) {
	b, err := os.ReadFile("ffmpeg.go")
	if err != nil {
		t.Skip("读不到 ffmpeg.go")
	}
	code := stripGoComments(string(b))

	if strings.Contains(code, `"-ss", "0.05"`) {
		t.Error("★ 还有硬编码的 `-ss 0.05` —— 软件/VAAPI 两条路径必须都走 seekArg(seekSec)，" +
			"漏一条就等于「装了硬解/没装硬解行为不同」")
	}
	if n := strings.Count(code, "seekArg(seekSec)"); n < 2 {
		t.Errorf("seekArg(seekSec) 只出现 %d 次 —— 软件路径与 VAAPI 路径都要用它", n)
	}
	vaapi, ok := funcBody(code, "func extractFrameVAAPI(")
	if !ok {
		t.Fatal("定位不到 extractFrameVAAPI")
	}
	if !strings.Contains(vaapi, "seekArg(seekSec)") {
		t.Error("★ VAAPI 路径没用参数化 seek —— 硬解机器上仍按旧时间点抽帧")
	}
	sw, ok := funcBody(code, "func extractFrameAt(")
	if !ok {
		t.Fatal("定位不到 extractFrameAt")
	}
	if !strings.Contains(sw, `"-ss", seekArg(seekSec)`) {
		t.Error("软件路径没有把 seekSec 传给 -ss")
	}
	// `-ss` 必须在 `-i` 之前（输入 seek + 默认 accurate_seek）：
	// 放在 `-i` 之后是"从头解码到 3s 再丢帧"，功能一样但对长视频慢得多。
	if iSS, iI := strings.Index(sw, "-ss"), strings.Index(sw, `"-i", path`); iSS < 0 || iI < 0 || iSS > iI {
		t.Errorf("`-ss` 不在 `-i` 之前（iSS=%d iI=%d）—— 输入 seek 变成输出 seek，"+
			"长视频要完整解码前 3 秒，抽帧明显变慢", iSS, iI)
	}
	if strings.Contains(code, "-noaccurate_seek") {
		t.Error("★ 加了 `-noaccurate_seek` —— 会退回到目标点**之前的关键帧**，" +
			"长 GOP 视频可能直接退回第 0 帧（黑屏），等于这次修改白做")
	}
	if !strings.Contains(code, "videoFrameSeekSec = 3.0") {
		t.Error("默认抽帧时间点不是 3.0s")
	}
	fv, ok := funcBody(code, "func extractVideoFrame(")
	if !ok {
		t.Fatal("定位不到 extractVideoFrame")
	}
	if !strings.Contains(fv, "videoFrameSeekFallbackSec") {
		t.Error("★ extractVideoFrame 里没有兜底重试 —— 短视频在 3s 处 seek 越界会整张失败")
	}
}

// ---------------------------------------------------------------------------
// F. 源码守卫：调用方必须把探到的时长传进来（否则兜底会白跑一次 3s）
// ---------------------------------------------------------------------------
func Test2010ThumbPassesDurationToVideoFrame(t *testing.T) {
	b, err := os.ReadFile("thumb.go")
	if err != nil {
		t.Skip("读不到 thumb.go")
	}
	code := stripGoComments(string(b))
	if !strings.Contains(code, "extractVideoFrame(ctx, path, size, ftmp, durOf(preMeta))") {
		t.Error("★ thumb.go 没有把视频时长传给抽帧（durOf(preMeta)）—— " +
			"短视频会先白跑一次 3s 才回退")
	}
	if !strings.Contains(code, "preMeta = m") {
		t.Error("thumb.go 没有复用抽帧前探到的元数据（preMeta）")
	}
}
