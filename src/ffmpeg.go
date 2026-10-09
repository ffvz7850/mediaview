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
func extractVideoFrame(ctx context.Context, path string, size int, out string) (int, int, error) {
	return extractFrame(ctx, path, size, out, true)
}

// extractImageFrame 从图片生成缩略图（不用 -ss）。
func extractImageFrame(ctx context.Context, path string, size int, out string) (int, int, error) {
	return extractFrame(ctx, path, size, out, false)
}

// lowresFor 按目标尺寸计算 ffmpeg 的 -lowres 档位。
//
// -lowres N 让解码器只取 DCT 的低频系数，直接产出 1/2^N 尺寸的图像 ——
// 这正是 vips 的 shrink-on-load 本该做、却在**渐进式 JPEG** 上做不到的事
// （渐进式把系数分散在多次扫描里，任何引擎都只能完整解码）。
//
// 档位必须**动态算**：档位过高会把源图缩到比目标还小，后面接 scale 就成了
// 「放大糊掉」。实测（8736x11648 原图 → 1920 档）：
//
//	  正确档 N=2 → 输出 305479 B；过高档 N=3 → 只有 269918 B（细节已丢）。
//
//		档位公式：N = clamp(floor(log2(长边/目标)), 0, 3)
//		ffmpeg 的上限实测就是 3（传 4 或 9 的输出与 3 逐字节相同）。
//
// lowresMinRatioNum/Den = 9/10：允许 lowres 把源缩到目标的 90%（详见 lowresFor）。
const lowresMinRatioNum, lowresMinRatioDen = 9, 10

// lowresForViewer 大图预览的 lowres 档位：优先用设置里的固定档，其次公式。
func lowresForViewer(w, h, target int) int {
	switch lv := getSettings().ViewerLowres; {
	case lv == -1:
		return 0 // 用户关闭
	case lv > 0:
		return lv // 用户指定
	default:
		return lowresFor(w, h, target) // 自动
	}
}

// lowresForThumb 缩略图的 lowres 档位：受总开关 + 档位设置共同决定。
func lowresForThumb(w, h, target int) int {
	if !getSettings().ThumbLowres {
		return 0
	}
	switch lv := getSettings().ThumbLowresLevel; {
	case lv == -1:
		return 0
	case lv > 0:
		return lv
	default:
		return lowresFor(w, h, target)
	}
}

func lowresFor(w, h, target int) int {
	longSide := w
	if h > longSide {
		longSide = h
	}
	if target <= 0 || longSide <= target {
		return 0
	}
	// lowresMinRatio：允许 lowres 把源缩到「目标的 90%」以内，以换取更大的档位。
	//
	// 为什么需要它（实测，8736x11648 原图，-q:v 4，中位 3 次）：
	//   目标 3072：N=1 → 1937ms；N=2 → 1196ms（快 39%），但输出从 3072 缩到 2912（缩 5%）
	//   **N=1 相比"不用 lowres"只快 0.6%（1937 vs 1948ms）—— 基本是白折腾**
	//   而 2912 仍远高于实际视口需求（2160 设备像素），观感无差别。
	//   用 5% 的尺寸余量换 39% 的时间，值得。
	//
	// 保守取 0.9（最多缩 10%）：
	//   3072 → 允许缩到 2764（2912 通过 → N=2）
	//   3584 → 允许缩到 3226（2912 通过 → N=2，输出缩 19%，已确认可接受）
	//   4096 → 允许缩到 3686（2912 不通过 → 仍 N=1，避免明显缩水）
	floor := (target*lowresMinRatioNum + lowresMinRatioDen - 1) / lowresMinRatioDen
	n := 0
	for longSide/(1<<(n+1)) >= floor && n < 3 {
		n++
	}
	return n
}

func extractFrame(ctx context.Context, path string, size int, out string, seek bool) (int, int, error) {
	if ffmpegPath == "" {
		return 0, 0, errNoFFmpeg
	}

	// 视频优先走 VAAPI 硬件解码（H.264/HEVC 抽帧是 CPU 重活，A8-7680 只有 4 核）。
	// 硬解失败时自动回退软件解码，不影响最终结果。
	// 熔断机制：连续失败 5 次后关闭硬解 10 分钟，避免不支持的编码每个都白跑两次 ffmpeg。
	if seek && getSettings().GPUDecode && !hwCircuitOpen() {
		if dev := detectVAAPIDevice(); dev != "" {
			vaErr := extractFrameVAAPI(ctx, path, size, out, dev)
			if vaErr == nil {
				hwRecordSuccess()
				return 0, 0, nil
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
	// 图片缩略图：加动态 -lowres（只解 DCT 低频系数）。
	// 必须是**输入选项**，所以放在 -i 之前。
	// 视频不加：视频走 -ss + VAAPI 抽帧，语义不同。
	// 顺便取一次原图尺寸：既用于 -lowres 档位，也随返回值给调用方写 meta，
	// 避免调用方再读一次文件头（L1-B：原来这里和 generateThumb 各读一次）。
	origW, origH := 0, 0
	if !seek {
		origW, origH = imageDimCached(path)
		if origW > 0 && origH > 0 {
			if lr := lowresForThumb(origW, origH, size); lr > 0 {
				args = append(args, "-lowres", itoa(lr))
			}
		}
	}
	args = append(args, "-autorotate", "-i", path,
		"-frames:v", "1", "-f", "image2", "-update", "1",
		"-vf", vf, "-an", "-y", "-q:v", itoa(q), out)

	cmd := exec.CommandContext(cctx, ffmpegPath, args...)
	if err := cmd.Run(); err != nil {
		return 0, 0, err
	}
	// 校验产物：ffmpeg 退出码 0 **不代表**产物可用（磁盘满、被信号 kill 但 Run 未报错、
	// 文件系统错误都会留下空文件）。缺这一步时调用方会把一个 0 字节文件 rename 进缓存，
	// 前端拿到破图；而缓存判据要求 size>0 → 判定未命中 → 每次请求都白跑一遍完整解码。
	// 与 extractFrameVAAPI 的「存在+非空」校验对齐（软件路径才是默认路径）。
	if st, serr := os.Stat(out); serr != nil || st.Size() == 0 {
		_ = os.Remove(out)
		// 1.8.164：把内联闭包换成普通变量 —— 逻辑一样但一眼能看懂，
		// 而且返回专用哨兵 errEmptyProduct（"产物空"不是"ffmpeg 不可用"）。
		var sz int64 = -1
		if st != nil {
			sz = st.Size()
		}
		return 0, 0, fmt.Errorf("%w（err=%v size=%d）", errEmptyProduct, serr, sz)
	}
	return origW, origH, nil
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

var (
	errNoProbe  = &simpleErr{"ffprobe unavailable"}
	errNoFFmpeg = &simpleErr{"ffmpeg unavailable"}
	// 1.8.164：单独一个哨兵。原来"产物为空/无效"也返回 errNoFFmpeg，
	// 于是日志与错误串都说 "ffmpeg unavailable"，而 ffmpeg 明明是好的 ——
	// 排障时会被这行字带偏（真正原因是磁盘满 / 被杀 / 文件系统错误）。
	errEmptyProduct = &simpleErr{"ffmpeg produced an empty/invalid file"}
)

func itoa(n int) string { return strconv.Itoa(n) }
