package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ffmpegPath  string
	ffprobePath string

	durationRe = regexp.MustCompile(`Duration:\s*(\d+):(\d+):(\d+\.\d+)`)
	videoRe    = regexp.MustCompile(`Video:\s*([a-zA-Z0-9]+)[^\n]*?,\s*(\d{2,5})x(\d{2,5})`)

	// VAAPI 硬解熔断与可观测性
	hwFailCount     int64      // 连续失败计数
	hwCircuitUntil  time.Time  // 熔断到期时间
	hwCircuitMu     sync.Mutex // 保护 hwCircuitUntil
	hwOKCount       int64      // 硬解成功总数
	hwFallbackCount int64      // 硬解失败回退总数
)

const (
	hwFailThreshold = 5                // 连续失败 5 次触发熔断
	hwCooldown      = 10 * time.Minute // 熔断持续时间
)

// vaapiEnv 返回 VAAPI 硬解所需的环境变量。
// LIBVA_DRIVERS_PATH 必须显式设置：飞牛 mediasrv 的 radeonsi_drv_video.so
// 在非标准路径 /usr/trim/lib/mediasrv/lib/dri，不设置 libva 可能找不到驱动。
// 不设置 LIBVA_DRIVER_NAME：让 libva 根据 GPU vendor 自动选择驱动（跨 AMD/Intel/NVIDIA）。
func vaapiEnv() []string {
	return append(os.Environ(),
		"LIBVA_DRIVERS_PATH=/usr/trim/lib/mediasrv/lib/dri:/usr/lib/x86_64-linux-gnu/dri",
	)
}

// hwCircuitOpen 检查硬解熔断是否开启。
func hwCircuitOpen() bool {
	hwCircuitMu.Lock()
	defer hwCircuitMu.Unlock()
	return time.Now().Before(hwCircuitUntil)
}

// hwRecordFailure 记录一次硬解失败，达到阈值时触发熔断。
func hwRecordFailure() {
	if atomic.AddInt64(&hwFailCount, 1) >= hwFailThreshold {
		hwCircuitMu.Lock()
		hwCircuitUntil = time.Now().Add(hwCooldown)
		hwCircuitMu.Unlock()
		atomic.StoreInt64(&hwFailCount, 0)
		log.Printf("VAAPI 硬解连续失败 %d 次，熔断 %v，期间直接用软件解码", hwFailThreshold, hwCooldown)
	}
}

// hwRecordSuccess 记录一次硬解成功，重置连续失败计数。
func hwRecordSuccess() {
	atomic.StoreInt64(&hwFailCount, 0)
	atomic.AddInt64(&hwOKCount, 1)
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if ok, _ := fileExists(p); ok {
			return p
		}
	}
	return ""
}

func fileExists(p string) (bool, error) {
	info, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

func initFFmpeg() {
	// 系统 ffmpeg 优先：飞牛系统自带 mediasrv 版 ffmpeg，完整支持 VAAPI 硬件解码
	// （FPK 自带的 johnvansickle 静态版 VAAPI 无法加载系统驱动）
	// 按优先级逐个检测，找到第一个可执行的就用
	ffmpegCandidates := []string{
		"/usr/trim/lib/mediasrv/ffmpeg", // 飞牛 mediasrv 实际路径
		"/usr/bin/ffmpeg",               // 标准路径（通常是符号链接）
		"/usr/local/bin/ffmpeg",
		filepath.Join(appDest, "bin", "ffmpeg"), // FPK 自带
		filepath.Join(appDest, "ffmpeg"),
	}
	for _, p := range ffmpegCandidates {
		if isExecutable(p) {
			ffmpegPath = p
			break
		}
	}
	// 最后尝试 PATH
	if ffmpegPath == "" {
		if p, err := exec.LookPath("ffmpeg"); err == nil && isExecutable(p) {
			ffmpegPath = p
		}
	}

	// ffprobe 同样逻辑
	ffprobeCandidates := []string{
		"/usr/trim/lib/mediasrv/ffprobe",
		"/usr/bin/ffprobe",
		"/usr/local/bin/ffprobe",
		filepath.Join(appDest, "bin", "ffprobe"),
		filepath.Join(appDest, "ffprobe"),
	}
	for _, p := range ffprobeCandidates {
		if isExecutable(p) {
			ffprobePath = p
			break
		}
	}
	if ffprobePath == "" {
		if p, err := exec.LookPath("ffprobe"); err == nil && isExecutable(p) {
			ffprobePath = p
		}
	}

	log.Printf("ffmpeg: path=%s ffprobe=%s appDest=%s", ffmpegPath, ffprobePath, appDest)
	if ffmpegPath != "" {
		if out, err := exec.Command(ffmpegPath, "-version").CombinedOutput(); err == nil {
			log.Printf("ffmpeg version: %s", strings.Split(string(out), "\n")[0])
		} else {
			log.Printf("ffmpeg execute FAILED: %v", err)
		}
	}
}

// isExecutable 检查文件是否存在且有执行权限
func isExecutable(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
}

// ---- ffprobe ----

type ffprobeOut struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type Meta struct {
	W        int     `json:"w"`
	H        int     `json:"h"`
	Duration float64 `json:"duration"`
	Codec    string  `json:"codec"`
}

func probeMeta(ctx context.Context, path string) (*Meta, error) {
	if ffprobePath != "" {
		return probeWithFFprobe(ctx, path)
	}
	if ffmpegPath != "" {
		return probeWithFFmpeg(ctx, path)
	}
	return nil, errNoProbe
}

func probeWithFFprobe(ctx context.Context, path string) (*Meta, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, ffprobePath, "-v", "quiet",
		"-print_format", "json", "-show_format", "-show_streams", path)
	data, err := cmd.Output()
	if err != nil {
		// ffprobe failed; try ffmpeg fallback
		if ffmpegPath != "" {
			return probeWithFFmpeg(ctx, path)
		}
		return nil, err
	}
	var pf ffprobeOut
	if err := json.Unmarshal(data, &pf); err != nil {
		return nil, err
	}
	m := &Meta{}
	for _, s := range pf.Streams {
		if s.CodecType == "video" {
			m.W, m.H = s.Width, s.Height
			m.Codec = s.CodecName
			if d, err := strconv.ParseFloat(s.Duration, 64); err == nil {
				m.Duration = d
			}
			break
		}
	}
	if m.Duration == 0 {
		if d, err := strconv.ParseFloat(pf.Format.Duration, 64); err == nil {
			m.Duration = d
		}
	}
	return m, nil
}

// probeWithFFmpeg parses the stderr of `ffmpeg -i file` (no output => exits 1).
func probeWithFFmpeg(ctx context.Context, path string) (*Meta, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, ffmpegPath, "-hide_banner", "-i", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run() // expected non-zero exit
	return parseFFmpegInfo(stderr.String())
}

func parseFFmpegInfo(s string) (*Meta, error) {
	m := &Meta{}
	// Duration: HH:MM:SS.xx
	if loc := durationRe.FindStringSubmatch(s); loc != nil {
		h, _ := strconv.ParseFloat(loc[1], 64)
		mi, _ := strconv.ParseFloat(loc[2], 64)
		se, _ := strconv.ParseFloat(loc[3], 64)
		m.Duration = h*3600 + mi*60 + se
	}
	// Video stream: codec ..., WxH
	if loc := videoRe.FindStringSubmatch(s); loc != nil {
		m.Codec = loc[1]
		w, _ := strconv.Atoi(loc[2])
		h, _ := strconv.Atoi(loc[3])
		m.W, m.H = w, h
	}
	if m.W == 0 && m.Duration == 0 {
		return nil, errNoProbe
	}
	return m, nil
}

// extractVideoFrame produces a center-cropped square thumbnail via ffmpeg.
// extractVideoFrame 从视频提取一帧生成缩略图。
// 注意：对图片文件不要用 -ss（图片没有时间轴，seek 会失败），由调用方决定是否 seek。
func extractVideoFrame(ctx context.Context, path string, size int, out string) error {
	return extractFrame(ctx, path, size, out, true)
}

// extractImageFrame 从图片生成缩略图（不用 -ss）。
func extractImageFrame(ctx context.Context, path string, size int, out string) error {
	return extractFrame(ctx, path, size, out, false)
}

func extractFrame(ctx context.Context, path string, size int, out string, seek bool) error {
	if ffmpegPath == "" {
		return errNoFFmpeg
	}

	// 视频优先走 VAAPI 硬件解码（H.264/HEVC 抽帧是 CPU 重活，A8-7680 只有 4 核）。
	// 硬解失败时自动回退软件解码，不影响最终结果。
	// 熔断机制：连续失败 5 次后关闭硬解 10 分钟，避免不支持的编码每个都白跑两次 ffmpeg。
	if seek && getSettings().GPUDecode && !hwCircuitOpen() {
		if dev := detectVAAPIDevice(); dev != "" {
			vaErr := extractFrameVAAPI(ctx, path, size, out, dev)
			if vaErr == nil {
				hwRecordSuccess()
				return nil
			}
			// 硬解失败：删除可能残留的不完整输出，回退软件解码
			_ = os.Remove(out)
			atomic.AddInt64(&hwFallbackCount, 1)
			hwRecordFailure()
			log.Printf("ffmpeg: VAAPI 硬解失败，回退软件解码: %s (%v)", filepath.Base(path), vaErr)
		}
	}

	vf := "scale=" + itoa(size) + ":" + itoa(size) +
		":force_original_aspect_ratio=increase,crop=" + itoa(size) + ":" + itoa(size)
	q := qualityToQScale(getSettings().ThumbQuality)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"-hide_banner", "-loglevel", "error"}
	if seek {
		args = append(args, "-ss", "0.05")
	}
	args = append(args, "-autorotate", "-i", path,
		"-frames:v", "1", "-f", "image2", "-update", "1",
		"-vf", vf, "-an", "-y", "-q:v", itoa(q), out)

	cmd := exec.CommandContext(cctx, ffmpegPath, args...)
	return cmd.Run()
}

// extractFrameVAAPI 用 VAAPI 硬件解码抽取视频帧并生成缩略图。
// 不加 -hwaccel_output_format vaapi：让 ffmpeg 自动把解码帧下载回内存，
// 滤镜语法与软件路径完全一致（scale_vaapi 不支持 force_original_aspect_ratio）。
// 不设置 LIBVA_DRIVER_NAME：让 libva 根据 DRM 设备 vendor ID 自动选择驱动
// （AMD→radeonsi, Intel→iHD/i965, NVIDIA→nvidia via nvidia-vaapi-driver），
// 跨 GPU 平台兼容。驱动不可用时 VAAPI 失败，自动回退软件解码。
func extractFrameVAAPI(ctx context.Context, path string, size int, out string, device string) error {
	vf := "scale=" + itoa(size) + ":" + itoa(size) +
		":force_original_aspect_ratio=increase,crop=" + itoa(size) + ":" + itoa(size)
	q := qualityToQScale(getSettings().ThumbQuality)
	// 硬解应该很快，10 秒超时足够；超时说明驱动/编码有问题，尽早回退
	// （从 15s 缩短到 10s，减少病态视频占用前台并发槽的时间）
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-hwaccel", "vaapi",
		"-hwaccel_device", device,
		"-ss", "0.05",
		"-autorotate", "-i", path,
		"-frames:v", "1", "-f", "image2", "-update", "1",
		"-vf", vf, "-an", "-y", "-q:v", itoa(q), out,
	}

	cmd := exec.CommandContext(cctx, ffmpegPath, args...)
	cmd.Env = vaapiEnv()
	// 用 CombinedOutput 捕获 stderr，失败时能看到具体原因（profile 不支持 / 驱动缺失等）
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("VAAPI ffmpeg 执行失败: %w (stderr: %s)", err, strings.TrimSpace(string(output)))
	}

	// 校验输出文件存在且非空（防止 0 字节坏图被当作成功永久缓存）
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return fmt.Errorf("VAAPI 输出文件无效: %v (size=0)", err)
	}

	// 校验 JPEG 可解码且不是绿图（与图片路径校验一致）
	if !validateJPEG(out) {
		return fmt.Errorf("VAAPI 输出校验失败（可能是绿图或解码异常）")
	}

	return nil
}

// qualityToQScale 把 JPEG quality(50~95, 越大越好) 映射成 ffmpeg -q:v(1~31, 越小越好)
func qualityToQScale(quality int) int {
	if quality < 50 {
		quality = 50
	}
	if quality > 95 {
		quality = 95
	}
	// 95 -> 2, 50 -> 10
	q := 2 + int(float64(95-quality)*8.0/45.0+0.5)
	if q < 1 {
		q = 1
	}
	if q > 31 {
		q = 31
	}
	return q
}

// transcodeAvailable reports whether on-the-fly transcoding is possible.
func transcodeAvailable() bool { return ffmpegPath != "" }

var (
	errNoProbe  = &simpleErr{"ffprobe unavailable"}
	errNoFFmpeg = &simpleErr{"ffmpeg unavailable"}
)

func itoa(n int) string         { return strconv.Itoa(n) }
func floatStr(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
