package main

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif" // 注册 GIF 解码器
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码器，否则 image.Decode 无法处理 PNG
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册 WebP 解码器（飞牛压缩系统可能输出 webp）
	"golang.org/x/sync/singleflight"
)

// ---- in-flight de-duplication for concurrent thumbnail requests ----
var sfGroup singleflight.Group

// errThumbDisabled 缩略图生成已被用户在设置里关闭
var errThumbDisabled = &simpleErr{"thumbnail generation disabled"}

// errThumbFailed 该文件此前生成失败（负缓存命中），直接回占位图
var errThumbFailed = &simpleErr{"thumbnail generation failed (cached)"}

// isCancelErr 判断错误是否来自 context 取消/超时（属于可恢复，不该进负缓存）。
func isCancelErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ---- 缩略图缓存的内容标识（sidecar）----
//
// 缓存有效性原先只看「缩略图 mtime >= 源文件 mtime」。但用 rsync -a / cp -p 这类
// 保留时间戳的方式覆盖同名文件后，旧缩略图的 mtime 仍然更晚 → 永远命中旧图
// （而查看器打开的是新图，前后矛盾）；反过来源文件时间戳在未来时，缓存又永不命中、
// 每次都重新全量解码。改成比对内容标识（cacheKey 含 path+mtime+size）两个方向都能修。
const thumbKeySuffix = ".key"

func thumbCacheKeyMatches(out, key string) bool {
	b, err := os.ReadFile(out + thumbKeySuffix)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == key
}

func writeThumbKey(out, key string) {
	_ = os.WriteFile(out+thumbKeySuffix, []byte(key), 0o644)
}

// finishThumb 所有「生成成功」的出口都走它：写下内容标识并返回产物路径。
func finishThumb(out, key string) (string, error) {
	writeThumbKey(out, key)
	return out, nil
}

// ---- 失败负缓存 ----
//
// svg、未编解码器的 heic/avif、损坏文件…… 每次渲染目录都会重跑一次注定失败的
// ffmpeg（失败响应还是 no-store，浏览器也不缓存），把 CPU 白白吃掉。
var (
	thumbFailMu  sync.Mutex
	thumbFailMap = map[string]time.Time{}
)

const thumbFailTTL = 10 * time.Minute

func thumbFailedRecently(key string) bool {
	thumbFailMu.Lock()
	defer thumbFailMu.Unlock()
	at, ok := thumbFailMap[key]
	if !ok {
		return false
	}
	if time.Since(at) > thumbFailTTL {
		delete(thumbFailMap, key)
		return false
	}
	return true
}

func markThumbFailed(key string) {
	thumbFailMu.Lock()
	defer thumbFailMu.Unlock()
	if len(thumbFailMap) > 4096 { // 防止无限增长
		thumbFailMap = map[string]time.Time{}
	}
	thumbFailMap[key] = time.Now()
}

// thumbGenSem 全局缩略图生成并发信号量。
// HTTP 请求和后台 worker 共用，防止用户快速滚动时几十个请求同时解码大图导致 OOM。
// 每个大图 Go 原生解码峰值可达 200-300MB，限制并发数避免内存叠加。
// 并发数可在设置里调整（1~8），调整后需重启应用生效（信号量只创建一次）。
var (
	thumbGenSem chan struct{}
	thumbSemMu  sync.Mutex
)

// getThumbSem 返回全局缩略图生成并发信号量。
// 信号量只在首次调用时创建，运行时不再动态重建：
// 若替换 channel，正在旧 channel 上「sem <- struct{}{}」等待的 goroutine 会永久阻塞
// （旧 channel 既不会被关闭，也不会有人往里发 token），造成 goroutine 泄漏。
// 设置里的 ThumbConcurrency 变更需重启应用生效。
func getThumbSem() chan struct{} {
	thumbSemMu.Lock()
	defer thumbSemMu.Unlock()
	if thumbGenSem == nil {
		n := getSettings().ThumbConcurrency
		if n < 1 {
			n = 1
		}
		if n > maxThumbConcurrency {
			n = maxThumbConcurrency
		}
		thumbGenSem = make(chan struct{}, n)
	}
	return thumbGenSem
}

// ---- 前后台资源让路 ----
//
// 背景：进未缓存目录时，前台（网格滚动）最多 ThumbConcurrency 个解码任务，
// 后台预生成再叠加 PreloadConcurrency 个 —— 两边各自计数、互不知情，
// 峰值并发 = 6 + 3 = 9，低功耗 NAS 上直接把 CPU 与内存带宽打满，
// 用户此时点开大图只能排队等。这里给后台立两条规矩：
//   1. 后台只用「前台用不到的槽」（非阻塞抢槽），不再与前台叠加；
//   2. 最近有前台请求时后台主动静默，等用户停手（静默期）再干活。
const maxThumbConcurrency = 8

// thumbGenActive / thumbGenPeak 仅用于观测：当前正在解码的缩略图数、历史峰值。
// 「后台让路」是否真的把峰值压在 ThumbConcurrency 以内，靠它验证；
// 线上排查「一进目录就卡」时也能从 /api/health 直接看到。
var (
	thumbGenActive int64
	thumbGenPeak   int64
)

func trackThumbGenStart() {
	n := atomic.AddInt64(&thumbGenActive, 1)
	for {
		peak := atomic.LoadInt64(&thumbGenPeak)
		if n <= peak || atomic.CompareAndSwapInt64(&thumbGenPeak, peak, n) {
			return
		}
	}
}

func trackThumbGenEnd() { atomic.AddInt64(&thumbGenActive, -1) }

// thumbActivityUnixNano 最近一次前台请求的时间戳（UnixNano，0 = 从未）。
var thumbActivityUnixNano int64

// thumbQuietPeriod 后台判定「用户已停手」所需的静默时长。
// 变量而非常量：测试里可以调小，避免用例真等 1.5 秒。
var thumbQuietPeriod = 1500 * time.Millisecond

// bgPollInterval 后台等待「轮到自己」的轮询间隔。
var bgPollInterval = 200 * time.Millisecond

// bgMaxWait 后台为等一个合适时机最多等多久，超时就放弃这个任务
// （队列里还有别的文件，没必要死等一个）。
var bgMaxWait = 60 * time.Second

// touchThumbActivity 标记「用户正在交互」。所有前台媒体请求都调用它。
func touchThumbActivity() {
	atomic.StoreInt64(&thumbActivityUnixNano, time.Now().UnixNano())
}

// thumbForegroundQuiet 距上次前台活动是否已超过静默期。
func thumbForegroundQuiet() bool {
	last := atomic.LoadInt64(&thumbActivityUnixNano)
	if last == 0 {
		return true
	}
	return time.Since(time.Unix(0, last)) >= thumbQuietPeriod
}

// tryAcquireThumbSlot 非阻塞抢一个「前台用不到的」槽。
// 后台只在抢得到时干活，因此「前台 + 后台」的总并发始终不超过 ThumbConcurrency。
func tryAcquireThumbSlot() bool {
	sem := getThumbSem()
	// 给前台留一个槽：后台最多占到 cap-1。
	// 否则极端配置（ThumbConcurrency=1 而 PreloadConcurrency=8）下后台会把槽占满，
	// 用户滚动的每一条 /api/thumb 都只能排队等后台任务跑完（单张最长 30s）。
	// cap==1 时 len>=0 恒成立 → 后台不抢，唯一的槽全留给前台。
	if len(sem) >= cap(sem)-1 {
		return false
	}
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseThumbSlot() { <-getThumbSem() }

// ---- 后台任务的抢占式取消 ----
//
// 只把「还没开始」的任务挡住是不够的：已经在跑的后台任务（每个都是一次 ffmpeg
// 全解码，数百 ms ~ 数十秒）会继续和新开的大图请求抢 CPU。
// 这里给后台任务挂一把可取消的 ctx，暂停时 cancel → exec.CommandContext
// 立刻 kill 掉在跑的 ffmpeg，把资源真正让出来。
var (
	bgCtxMu   sync.Mutex
	bgCtx     context.Context
	bgCtxStop context.CancelFunc
)

// currentBackgroundCtx 返回后台任务当前使用的 ctx（惰性创建）。
func currentBackgroundCtx() context.Context {
	bgCtxMu.Lock()
	defer bgCtxMu.Unlock()
	if bgCtx == nil {
		bgCtx, bgCtxStop = context.WithCancel(context.Background())
	}
	return bgCtx
}

// cancelBackgroundCtx 取消当前后台 ctx（暂停时调用）并丢弃引用，
// 下一个后台任务会拿到一把干净的新 ctx。
func cancelBackgroundCtx() {
	bgCtxMu.Lock()
	defer bgCtxMu.Unlock()
	if bgCtxStop != nil {
		bgCtxStop()
	}
	bgCtx, bgCtxStop = nil, nil
}

// ---- background pre-generation worker pool ----
type thumbReq struct {
	path string
	kind string
	size int
}

var (
	thumbQueue        chan thumbReq
	thumbCtx          context.Context // 供 worker 长期存活的父 context
	thumbWorkerCtx    context.Context
	thumbWorkerCancel context.CancelFunc
	thumbWorkerMu     sync.Mutex
	thumbPaused       bool                  // 大图浏览时暂停后台预生成
	thumbPauseUntil   time.Time             // 暂停的自动过期时刻（见 thumbPauseTTL）
	thumbPauseMu      sync.Mutex            // 保护 thumbPaused / thumbPauseUntil
	thumbResumeCh     = make(chan struct{}) // 恢复信号，关闭即广播
)

// thumbPauseTTL 暂停的最长有效期。前端在异常路径（点窗口标题栏 X 关闭、刷新页面、
// 宿主销毁页面）不会发 resume，没有上限的话后台预生成会**永久停摆**，只能重启应用。
const thumbPauseTTL = 2 * time.Minute

// initThumbWorkers 记录 ctx，并只在缩略图开启时启动 worker。
func initThumbWorkers(ctx context.Context) {
	thumbCtx = ctx
	if getSettings().ThumbEnabled {
		spawnThumbWorkers()
	}
	logThumbState("thumbnail workers: enabled=" + boolStr(getSettings().ThumbEnabled))
}

// spawnThumbWorkers 拉起 worker goroutine，数量由配置 PreloadConcurrency 决定。
// 重复调用会先停止旧 worker 再启动新的（支持设置变更后动态调整）。
func spawnThumbWorkers() {
	thumbWorkerMu.Lock()
	defer thumbWorkerMu.Unlock()

	// 停止旧 worker
	if thumbWorkerCancel != nil {
		thumbWorkerCancel()
	}

	n := getSettings().PreloadConcurrency
	if n < 1 {
		n = 1
	}
	if n > maxThumbConcurrency {
		n = maxThumbConcurrency
	}

	// 用局部变量创建队列和 ctx，再赋值给全局：worker 闭包必须捕获局部变量，
	// 否则每轮 select 读全局 thumbWorkerCtx/thumbQueue 会与 spawn 时的替换产生 data race；
	// 更严重的是旧 worker 在"回到 select 顶部"时读到新 ctx（未被 cancel）→ 永远等不到 Done()，
	// 每次 spawn 泄漏 PreloadConcurrency 个 worker，且新旧 worker 一起消费同一队列。
	q := make(chan thumbReq, 512)
	thumbQueue = q
	ctx := thumbCtx
	if ctx == nil {
		ctx = context.Background()
	}
	wctx, wcancel := context.WithCancel(ctx)
	thumbWorkerCtx, thumbWorkerCancel = wctx, wcancel

	for i := 0; i < n; i++ {
		go func() {
			for {
				select {
				case <-wctx.Done():
					return
				case req := <-q:
					if !runBackgroundThumb(wctx, req) {
						return
					}
				}
			}
		}()
	}
}

// waitWhilePaused 在「后台已暂停」期间阻塞，直到恢复或被取消。
// 返回 false 表示应当退出（上下文已取消）。
func waitWhilePaused(wctx context.Context) bool {
	for {
		thumbPauseMu.Lock()
		// 到点自动恢复：前端异常关窗（点标题栏 X、刷新）时不会发 resume，
		// 没有这条后台预生成会永久停摆，只能重启应用。
		paused := thumbPaused && time.Now().Before(thumbPauseUntil)
		resumeCh := thumbResumeCh // 在锁内复制引用：全局变量可能被 handleResumeThumb 替换，
		thumbPauseMu.Unlock()     // 直接读全局变量会与 close+替换产生数据竞争
		if !paused {
			return true
		}
		select {
		case <-wctx.Done():
			return false
		case <-resumeCh:
		}
	}
}

// waitBackgroundTurn 等到「轮到后台干活」：用户已停手，并且抢到一个空余槽。
// 抢到槽时返回 true，调用方必须 releaseThumbSlot()；返回 false 表示本轮放弃。
func waitBackgroundTurn(wctx, bgctx context.Context) bool {
	deadline := time.Now().Add(bgMaxWait)
	for {
		select {
		case <-wctx.Done():
			return false
		case <-bgctx.Done(): // 已被 pause 抢占，立刻收手
			return false
		default:
		}
		// 用户正在看/切图：后台一个槽都不占，完全让路
		if thumbForegroundQuiet() && tryAcquireThumbSlot() {
			return true
		}
		select {
		case <-wctx.Done():
			return false
		case <-bgctx.Done():
			return false
		case <-time.After(bgPollInterval):
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// runBackgroundThumb 处理一个后台预生成任务；返回 false 表示 worker 应当退出。
func runBackgroundThumb(wctx context.Context, req thumbReq) bool {
	if !waitWhilePaused(wctx) {
		return false
	}
	if !getSettings().ThumbEnabled {
		return true
	}
	bgctx := currentBackgroundCtx()
	if !waitBackgroundTurn(wctx, bgctx) {
		if wctx.Err() != nil {
			return false // 应用要退出了
		}
		// 被 pause 抢占（或等超时）：任务**放回队列**而不是丢掉。
		// 不能因为用户看了一眼大图，就让这批预生成永远不再发生 ——
		// 恢复后 worker 会接着处理，而「活动感知退让 + 预留前台槽」保证它不会跟用户抢资源。
		if bgctx.Err() != nil {
			requeueThumb(req)
		}
		return true
	}
	defer releaseThumbSlot()
	if bgctx.Err() != nil {
		requeueThumb(req)
		return true
	}
	if _, err := ensureThumbBG(bgctx, req.path, req.kind, req.size); err != nil && isCancelErr(err) {
		requeueThumb(req) // 跑到一半被抢占（ffmpeg 被 kill），重排到队尾重试
	}
	return true
}

// restartThumbWorkers 在设置变更后按需启停。
func restartThumbWorkers() {
	if getSettings().ThumbEnabled {
		spawnThumbWorkers()
	} else if thumbWorkerCancel != nil {
		// 关闭时停止 worker
		thumbWorkerCancel()
		thumbWorkerCancel = nil
	}
}

func enqueueThumb(path, kind string, size int) {
	if !getSettings().ThumbEnabled {
		return
	}
	// worker 由 initThumbWorkers（启动时）和 restartThumbWorkers（设置变更时）负责启停，
	// 这里不再调用 spawnThumbWorkers() —— 每次入队都重启 worker 会导致频繁
	// 停止/启动 goroutine，开销大且可能丢失队列中尚未处理的请求。
	q := thumbQueue
	if q == nil {
		return
	}
	select {
	case q <- thumbReq{path, kind, size}:
	default:
	}
}

// requeueThumb 把任务放回后台队列（非阻塞：队列满就丢，那种情况由前台按需生成兜底）。
// 专用于「被 pause 抢占」这类可恢复的放弃 —— 不能因为用户看了一眼大图，
// 就让这批预生成任务永久消失。
func requeueThumb(req thumbReq) {
	q := thumbQueue
	if q == nil {
		return
	}
	select {
	case q <- req:
	default:
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ensureThumb generates the thumbnail if missing; returns cache file path.
// 前台请求固定用 Background：客户端断开（翻页、关窗）时若绑 r.Context()，
// exec.CommandContext 会 kill 掉跑到一半的 ffmpeg，缓存不落盘、下次从零重跑。
func ensureThumb(path, kind string, size int) (string, error) {
	return ensureThumbInternal(context.Background(), path, kind, size, true)
}

// ensureThumbBG 后台预生成专用。
// 调用方（worker）必须已持有前台信号量的一个空余槽（见 waitBackgroundTurn）——
// 后台不再「额外」占并发，所以前台+后台的总并发始终不超过 ThumbConcurrency。
// ctx 可被 pause 取消：取消后正在跑的 ffmpeg 被 kill，半成品不会落进缓存。
func ensureThumbBG(ctx context.Context, path, kind string, size int) (string, error) {
	return ensureThumbInternal(ctx, path, kind, size, false)
}

func ensureThumbInternal(ctx context.Context, path, kind string, size int, foreground bool) (string, error) {
	if !getSettings().ThumbEnabled {
		return "", errThumbDisabled
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	// 两个 key 各司其职，**不能混用**：
	//   key     —— 元数据缓存用，只与 path+mtime+size 有关（meta 和画质无关，
	//              混进画质会让 loadMetaCache 永远查不到 saveMeta 写的条目，
	//              结果每个视频每次列目录都要重新读盘）；
	//   cacheID —— 缩略图产物用，额外含画质（缓存路径只按 path+size 组织，
	//              不把画质体现在标识里的话，用户改完画质会一直看到旧画质的图）。
	key := cacheKey(path, info)
	cacheID := key + "|q" + strconv.Itoa(getSettings().ThumbQuality)
	out := thumbPathFor(path, size)
	// 缓存命中判据：产物存在、非空，且**内容标识一致**（见 thumbKeySuffix 注释）
	if thumbInfo, err := os.Stat(out); err == nil {
		if thumbInfo.Size() > 0 && thumbCacheKeyMatches(out, cacheID) {
			return out, nil
		}
		// 缓存过期 / 空文件 / 标识不符：删掉重新生成
		_ = os.Remove(out)
		_ = os.Remove(out + ".meta.json")
		_ = os.Remove(out + thumbKeySuffix)
	}
	// 负缓存**只对后台预生成生效**：前台是用户正在等的那张图，必须真的再试一次，
	// 否则一次偶发失败（例如被 pause 抢占）会让它在 TTL 内一直显示占位图。
	if !foreground && thumbFailedRecently(cacheID) {
		return "", errThumbFailed
	}
	// singleflight key 必须包含 size：同一文件可能同时被请求 320/800/1920 三种尺寸
	// （系统缩略图接管时文件管理器会并发请求 list/medium/big），不拼 size 会导致
	// 后到达的大尺寸请求拿到先生成的小尺寸结果。
	sfKey := cacheID + ":s" + strconv.Itoa(size)
	v, err, _ := sfGroup.Do(sfKey, func() (any, error) {
		return generateThumb(ctx, path, kind, size, info, key, cacheID, foreground)
	})
	if err != nil {
		// 只对「确定性失败」记负缓存：被 pause 抢占（context canceled）或超时是可恢复的，
		// 记下来反而会让这张图在 TTL 内一直拿不到缩略图。
		if err != errThumbDisabled && !isCancelErr(err) {
			markThumbFailed(cacheID)
		}
		return "", err
	}
	// 显式 chmod 生成的缩略图文件（umask 可能导致权限 000）
	_ = os.Chmod(v.(string), 0o644)
	return v.(string), nil
}

func generateThumb(ctx context.Context, path, kind string, size int, info os.FileInfo, key, cacheID string, foreground bool) (string, error) {
	// 前台请求在这里阻塞取槽（防止快速滚动时几十个大图同时解码导致 OOM）。
	// 后台任务的槽已由 worker 在进入 singleflight 之前用 tryAcquireThumbSlot 拿到，
	// 这里不再取槽 —— 否则「前台 6 + 后台 3」会重新叠加成 9 个并发解码。
	if foreground {
		sem := getThumbSem()
		sem <- struct{}{}
		defer func() { <-sem }()
	}
	// 计数必须在拿到槽之后：否则「排队等槽」的请求也会被算成正在解码，
	// 峰值看起来仍然叠加（实测会被这条误导）。
	trackThumbGenStart()
	defer trackThumbGenEnd()

	out := thumbPathFor(path, size)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	// 显式 chmod 目录（umask 可能是 0777 导致权限 000，影响后续清除和系统缩略图读取）
	_ = os.Chmod(filepath.Dir(out), 0o755)
	tmp := out + ".tmp"

	// ffmpegImageFailed 标记图片路径中是否已经尝试过 ffmpeg 软件解码且失败。
	// 避免 fall through 到通用路径后重复调用 ffmpeg（每次超时 30 秒，大图会白等一分钟）。
	ffmpegImageFailed := false

	if kind == "image" {
		// JPEG 优先尝试 VAAPI 硬件解码（仅对 Baseline JPEG 有效，Progressive JPEG 自动回退）
		// 注意：VAAPI 硬件解码受 GPUImageDecode 门控（默认关闭，因 AMD Carrizo 下可能出绿图）；
		// 但下面的「大图 ffmpeg 软件解码」不依赖 VAAPI，不应被同一个开关门控。
		// 图片 VAAPI 分支同样要受熔断保护、并计入成败：
		// 否则勾了实验性「图片硬解」而 GPU 不支持时，**每一张** JPEG 都要先白等一次
		// 注定失败的 ffmpeg（超时期间还占着缩略图并发槽），比不做硬解还慢。
		if isJPEGFile(path) && getSettings().GPUDecode && getSettings().GPUImageDecode && !hwCircuitOpen() {
			if ok, meta := ffmpegImageThumbVAAPI(ctx, path, size, tmp); ok {
				if err := os.Rename(tmp, out); err == nil {
					hwRecordSuccess()
					if meta != nil {
						saveMeta(key, path, size, meta)
					}
					return finishThumb(out, cacheID)
				}
			}
			_ = os.Remove(tmp)
			atomic.AddInt64(&hwFallbackCount, 1)
			hwRecordFailure()
		}

		// 大图走 ffmpeg 软件解码（内部 libjpeg-turbo，SIMD 优化，比 Go 原生快 2-3 倍）。
		// 此路径不依赖 VAAPI / GPUImageDecode，默认配置下即生效。
		// 小图（<1MB）直接走 Go 原生，避免 ffmpeg 进程启动开销。
		if info != nil && info.Size() > 1024*1024 && ffmpegPath != "" && !ffmpegImageFailed {
			ftmp := out + ".gen.jpg"
			_ = os.Remove(ftmp)
			if err := extractImageFrame(ctx, path, size, ftmp); err == nil {
				if err := os.Rename(ftmp, out); err == nil {
					// 只在尺寸有效时落盘：heic/avif/svg 的 imageDim 返回 0，
					// 写进去会让 /api/meta 永远返回 0×0 且不再 probe。
					if w, h := imageDim(path); w > 0 && h > 0 {
						saveMeta(key, path, size, &Meta{W: w, H: h})
					}
					return finishThumb(out, cacheID)
				}
			}
			_ = os.Remove(ftmp)
			ffmpegImageFailed = true // 已试过 ffmpeg 且失败，fall through 时不再重复
		}
		// 回退 Go 原生解码（Go 解码不可中断，开始前先确认没被取消）
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ok, meta := goImageThumb(path, size, tmp)
		if ok {
			if err := os.Rename(tmp, out); err != nil {
				return "", err
			}
			if meta != nil {
				saveMeta(key, path, size, meta)
			}
			return finishThumb(out, cacheID)
		}
		_ = os.Remove(tmp)
		// Go decode failed (HEIC/AVIF/...): fall through to ffmpeg
	}

	// video or exotic image: ffmpeg（图片若已在上方试过 ffmpeg 且失败则跳过，避免重复 30 秒超时）
	if ffmpegPath != "" && !ffmpegImageFailed {
		ftmp := out + ".gen.jpg"
		_ = os.Remove(ftmp)
		var ferr error
		if kind == "image" {
			ferr = extractImageFrame(ctx, path, size, ftmp) // 图片不用 -ss
		} else {
			ferr = extractVideoFrame(ctx, path, size, ftmp) // 视频 seek 到 0.05s
		}
		if ferr == nil {
			if err := os.Rename(ftmp, out); err != nil {
				return "", err
			}
			// probe & cache meta
			if m, err := probeMeta(ctx, path); err == nil {
				saveMeta(key, path, size, m)
			}
			return finishThumb(out, cacheID)
		}
		_ = os.Remove(ftmp)
	}
	return "", errNoFFmpeg
}

// ---- VAAPI 硬件加速图片缩略图 ----

var (
	vaapiMu    sync.Mutex
	vaapiCache string    // 缓存检测结果，空字符串表示不可用
	vaapiAt    time.Time // 缓存时间，带 30 秒 TTL
)

// detectVAAPIDevice 检测 VAAPI 渲染设备，结果缓存 30 秒。
// 不用 sync.Once：应用先于 amdgpu/libva 就绪启动时，首次探测失败会永久为空，
// 整个进程生命周期内硬解被静默跳过，直到重启。带 TTL 后驱动就绪最多 30 秒自愈。
// 只检查 render node（renderD128/renderD129），不回退到 card0：
// card0 是显示节点不是 render node，做 -hwaccel_device 基本必然失败，
// 回退到它只会让硬解多白跑一次再触发熔断。
func detectVAAPIDevice() string {
	vaapiMu.Lock()
	defer vaapiMu.Unlock()

	if time.Since(vaapiAt) < 30*time.Second && !vaapiAt.IsZero() {
		return vaapiCache
	}

	candidates := []string{
		"/dev/dri/renderD128",
		"/dev/dri/renderD129",
	}
	for _, dev := range candidates {
		if _, err := os.Stat(dev); err == nil {
			vaapiCache = dev
			vaapiAt = time.Now()
			return dev
		}
	}
	vaapiCache = ""
	vaapiAt = time.Now()
	return ""
}

// isJPEGFile 判断文件是否为 JPEG 格式（仅 VAAPI 支持 JPEG 硬件解码）
func isJPEGFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".jfif" || ext == ".jpe"
}

// isProgressiveJPEG 检测 JPEG 是否为 VAAPI 不支持的格式。
// VAAPI 仅支持 Baseline DCT (SOF0=0xC0)，其他所有 SOF marker（Progressive、Extended、Lossless 等）
// 都会输出绿色方块或解码失败，需提前跳过。
func isProgressiveJPEG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	// 读取前 2 字节确认是 JPEG（FFD8）
	var soi [2]byte
	if _, err := io.ReadFull(f, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return false
	}

	// 扫描 marker，找 SOF marker
	buf := make([]byte, 2)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			return false
		}
		// 跳过填充字节（FF 后可能跟多个 FF）
		if buf[0] != 0xFF {
			continue
		}
		marker := buf[1]
		// 0xC0 = Baseline DCT（VAAPI 唯一支持的）
		if marker == 0xC0 {
			return false
		}
		// 其他所有 SOF marker（0xC1~0xCF 中除 0xC4/D0~D7 外）都不支持
		// 0xC1 Extended, 0xC2 Progressive, 0xC3 Lossless, 0xC5~0xC7 Differential,
		// 0xC9~0xCB Arithmetic, 0xCD~0xCF Differential arithmetic
		if marker >= 0xC1 && marker <= 0xCF && marker != 0xC4 && marker < 0xD0 {
			return true
		}
		// 其他 marker：读取长度并跳过
		if _, err := io.ReadFull(f, buf); err != nil {
			return false
		}
		segLen := int(buf[0])<<8 | int(buf[1])
		if segLen < 2 {
			return false
		}
		skip := make([]byte, segLen-2)
		if _, err := io.ReadFull(f, skip); err != nil {
			return false
		}
	}
}

// ffmpegImageThumbVAAPI 用 ffmpeg VAAPI 硬件解码生成图片缩略图。
// 返回 (成功, 元数据)。VAAPI 不可用或解码失败时返回 (false, nil)。
// parent 被取消（例如后台预生成被 pause 抢占）时 ffmpeg 会被 kill。
func ffmpegImageThumbVAAPI(parent context.Context, path string, size int, out string) (bool, *Meta) {
	if ffmpegPath == "" {
		return false, nil
	}
	dev := detectVAAPIDevice()
	if dev == "" {
		return false, nil
	}

	// VAAPI 不支持 Progressive JPEG，直接跳过（会输出绿色方块）
	if isProgressiveJPEG(path) {
		return false, nil
	}

	vf := "scale=" + itoa(size) + ":" + itoa(size) +
		":force_original_aspect_ratio=increase,crop=" + itoa(size) + ":" + itoa(size)
	q := qualityToQScale(getSettings().ThumbQuality)

	// 超时与视频硬解对齐（10s）：硬解要么很快，要么就是驱动/编码不支持，
	// 早点失败回退，别让前台的缩略图并发槽被白占 30 秒。
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-hwaccel", "vaapi", "-hwaccel_device", dev,
		"-autorotate", "-i", path,
		"-frames:v", "1", "-f", "image2", "-update", "1",
		"-vf", vf, "-an", "-y", "-q:v", itoa(q), out)

	cmd.Env = vaapiEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("VAAPI ffmpeg FAILED for %s (dev=%s): %v\n%s", path, dev, err, string(output))
		return false, nil
	}

	// 验证输出文件存在且非空
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		log.Printf("VAAPI ffmpeg output empty for %s", path)
		return false, nil
	}

	// 验证输出 JPEG 能正常解码（防止绿色方块等颜色错误）
	if !validateJPEG(out) {
		log.Printf("VAAPI ffmpeg output invalid for %s, falling back to CPU", path)
		_ = os.Remove(out)
		return false, nil
	}

	log.Printf("VAAPI ffmpeg OK for %s (dev=%s)", path, dev)

	w, h := imageDim(path)
	meta := &Meta{W: w, H: h}
	return true, meta
}

// validateJPEG 验证 JPEG 文件能否正常解码，且不是 VAAPI 输出的绿色方块。
// 绿图能正常解码、尺寸也正常，所以必须做像素级检查。
func validateJPEG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return false
	}
	// 检查图像尺寸合理
	b := img.Bounds()
	if b.Dx() < 1 || b.Dy() < 1 {
		return false
	}

	// 像素级绿图检测：VAAPI 解码失败时输出全绿或大部分绿色
	// 采样检查（每隔几个像素取一个，避免全图遍历太慢）
	w, h := b.Dx(), b.Dy()
	stepX := w / 32
	if stepX < 1 {
		stepX = 1
	}
	stepY := h / 32
	if stepY < 1 {
		stepY = 1
	}
	total := 0
	greenPixels := 0
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, bl, _ := img.At(x, y).RGBA()
			total++
			// 绿图特征：G 明显高于 R 和 B（绿色通道占主导）
			// RGBA 返回 0~65535，绿图通常 R≈0, G≈65535, B≈0
			if g > r*2 && g > bl*2 && g > 32768 {
				greenPixels++
			}
		}
	}
	// 如果超过 40% 的像素是纯绿色，认为是 VAAPI 绿图
	if total > 0 && float64(greenPixels)/float64(total) > 0.4 {
		log.Printf("validateJPEG: green frame detected (%d/%d green pixels), rejecting", greenPixels, total)
		return false
	}
	return true
}

// readJPEGOrientation 读取 JPEG 文件的 EXIF Orientation（1~8），失败或无 EXIF 返回 1。
// 只解析文件头，不全量解码，开销极小。竖拍手机照片通常为 6（顺时针90°）或 8（逆时针90°）。
func readJPEGOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	// SOI marker
	var soi [2]byte
	if _, err := io.ReadFull(f, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 1
	}
	// 扫描各 marker segment，找 APP1 (0xFFE1)
	for {
		var marker [2]byte
		if _, err := io.ReadFull(f, marker[:]); err != nil {
			return 1
		}
		if marker[0] != 0xFF {
			return 1
		}
		// 跳过填充字节 0xFF
		for marker[1] == 0xFF {
			if _, err := io.ReadFull(f, marker[1:2]); err != nil {
				return 1
			}
		}
		// SOS (0xDA) 之后是压缩数据，没有更多 meta
		if marker[1] == 0xDA {
			return 1
		}
		// segment length
		var lenBuf [2]byte
		if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(lenBuf[:])) - 2
		if segLen < 0 {
			return 1
		}
		if marker[1] == 0xE1 && segLen >= 8 {
			// APP1: "Exif\0\0" + TIFF
			var sig [6]byte
			if _, err := io.ReadFull(f, sig[:]); err != nil {
				return 1
			}
			if string(sig[:]) != "Exif\x00\x00" {
				// 不是 Exif（XMP 等其它 APP1 很常见，而且可能排在 Exif 之前）：
				// 跳过本段继续往后找，不能直接放弃 —— 否则方向被当成 1。
				if _, err := f.Seek(int64(segLen-6), io.SeekCurrent); err != nil {
					return 1
				}
				continue
			}
			// TIFF header
			var tiff [8]byte
			if _, err := io.ReadFull(f, tiff[:]); err != nil {
				return 1
			}
			var order binary.ByteOrder
			if tiff[0] == 'I' && tiff[1] == 'I' {
				order = binary.LittleEndian
			} else if tiff[0] == 'M' && tiff[1] == 'M' {
				order = binary.BigEndian
			} else {
				return 1
			}
			ifd0Offset := int(order.Uint32(tiff[4:8]))
			// IFD0 是「相对 TIFF 头起始」的偏移，所以必须知道 TIFF 头在文件里的真实位置。
			// 不能写死 12：那个数字只在「APP1 紧随 SOI」时成立，而绝大多数相机 JPEG
			// 前面还有 APP0/JFIF 段（甚至多个 APP1），写死会让读到的 IFD 完全错位 ——
			// 表现为竖拍照片的方向读成 1（不旋转），网格里横躺。
			// 当前文件位置 = TIFF 头起始 + 8（刚读完 8 字节 TIFF header）。
			pos, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return 1
			}
			ifd0Abs := int(pos) - 8 + ifd0Offset
			if _, err := f.Seek(int64(ifd0Abs), io.SeekStart); err != nil {
				return 1
			}
			var numEntries [2]byte
			if _, err := io.ReadFull(f, numEntries[:]); err != nil {
				return 1
			}
			n := int(order.Uint16(numEntries[:]))
			for i := 0; i < n; i++ {
				var entry [12]byte
				if _, err := io.ReadFull(f, entry[:]); err != nil {
					return 1
				}
				tag := order.Uint16(entry[0:2])
				if tag == 0x0112 { // Orientation
					typ := order.Uint16(entry[2:4])
					if typ == 3 { // SHORT
						return int(order.Uint16(entry[8:10]))
					}
					return 1
				}
			}
			return 1
		}
		// 不是 APP1，跳过整个 segment
		if _, err := f.Seek(int64(segLen), io.SeekCurrent); err != nil {
			return 1
		}
	}
}

// goImageThumb decodes with native libs, center-crops to square, resizes.
// 超大图（像素数超过 goImageMaxPixels）直接返回 false，走 ffmpeg 分支，
// 避免 image.Decode 把几万像素的图全量解到内存导致 OOM。
// EXIF 方向非 1 的图（竖拍照片）也走 ffmpeg，因为 Go image 包不处理 EXIF 旋转。
const goImageMaxPixels = 20_000_000 // ~4500x4500，超过走 ffmpeg 避免 Go 原生解码内存峰值

func goImageThumb(path string, size int, out string) (bool, *Meta) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	// 先只读 header 拿尺寸，不解码像素
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return false, nil
	}
	w, h := cfg.Width, cfg.Height
	if w <= 0 || h <= 0 {
		return false, nil
	}
	// EXIF 方向非 1（竖拍照片等）：Go image 包不自动旋转，走 ffmpeg 分支
	if readJPEGOrientation(path) != 1 {
		return false, &Meta{W: w, H: h}
	}
	// 超大图交给 ffmpeg（分块解码+scale，内存占用低）
	if w*h > goImageMaxPixels {
		return false, &Meta{W: w, H: h}
	}
	// 回到开头再完整解码
	if _, err := f.Seek(0, 0); err != nil {
		return false, nil
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return false, nil
	}
	b := img.Bounds()
	w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return false, nil
	}
	side := w
	if h < side {
		side = h
	}
	// center crop square
	x0 := (w - side) / 2
	y0 := (h - side) / 2
	crop := image.Rect(x0, y0, x0+side, y0+side)

	canvas := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.ApproxBiLinear.Scale(canvas, canvas.Bounds(), img, crop, draw.Src, nil)

	of, err := os.Create(out)
	if err != nil {
		return false, nil
	}
	defer of.Close()
	q := getSettings().ThumbQuality
	if q < 50 || q > 95 {
		q = 80
	}
	if err := jpeg.Encode(of, canvas, &jpeg.Options{Quality: q}); err != nil {
		return false, nil
	}
	return true, &Meta{W: w, H: h}
}

// ---- HTTP handlers ----

func handleThumb(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity() // 网格正在取缩略图 = 用户正在滚动，后台预生成让路
	if !getSettings().ThumbEnabled {
		// 缩略图被关闭：明确告诉前端"别再来取了"，前端据此降级为文件图标。
		// 这里不再返回 1x1 占位图 —— 那会让用户以为"缩略图坏了"。
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-MediaView-Thumb", "disabled")
		http.Error(w, "thumbnail generation disabled", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	path, sc := resolveMediaPath(q.Get("path"))
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	defSize := getSettings().ThumbSize
	if defSize < 64 || defSize > 640 {
		defSize = 320
	}
	size := defSize
	if s, err := strconv.Atoi(q.Get("size")); err == nil && s >= 64 && s <= 640 {
		size = s
	}
	kind := classify(filepath.Ext(path))
	if kind == "" {
		http.Error(w, "unsupported type", http.StatusBadRequest)
		return
	}
	file, err := ensureThumb(path, kind, size)
	if err != nil {
		if err == errThumbDisabled {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-MediaView-Thumb", "disabled")
			http.Error(w, "thumbnail generation disabled", http.StatusForbidden)
			return
		}
		// placeholder: return a generic 1x1 / let frontend fallback
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-MediaView-Thumb", "failed")
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(placeholderJPEG)
		return
	}
	info, err := os.Stat(file)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "image/jpeg")
	tf, err := os.Open(file)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer tf.Close()
	http.ServeContent(w, r, "thumb.jpg", info.ModTime(), tf)
}

var placeholderJPEG = []byte{
	0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
	0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0xff, 0xdb, 0x00, 0x43,
	0x00, 0x08, 0x06, 0x06, 0x07, 0x06, 0x05, 0x08, 0x07, 0x07, 0x07, 0x09,
	0x09, 0x08, 0x0a, 0x0c, 0x14, 0x0d, 0x0c, 0x0b, 0x0b, 0x0c, 0x19, 0x12,
	0x13, 0x0f, 0x14, 0x1d, 0x1a, 0x1f, 0x1e, 0x1d, 0x1a, 0x1c, 0x1c, 0x20,
	0x24, 0x2e, 0x27, 0x20, 0x22, 0x2c, 0x23, 0x1c, 0x1c, 0x28, 0x37, 0x29,
	0x2c, 0x30, 0x31, 0x34, 0x34, 0x34, 0x1f, 0x27, 0x39, 0x3d, 0x38, 0x32,
	0x3c, 0x2e, 0x33, 0x34, 0x32, 0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x01,
	0x00, 0x01, 0x01, 0x01, 0x11, 0x00, 0xff, 0xc4, 0x00, 0x1f, 0x00, 0x00,
	0x01, 0x05, 0x01, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0xff, 0xc4, 0x00, 0xb5, 0x10, 0x00, 0x02, 0x01, 0x03,
	0x03, 0x02, 0x04, 0x03, 0x05, 0x05, 0x04, 0x04, 0x00, 0x00, 0x01, 0x7d,
	0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06,
	0x13, 0x51, 0x61, 0x07, 0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08,
	0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0, 0x24, 0x33, 0x62, 0x72,
	0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
	0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45,
	0x46, 0x47, 0x48, 0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59,
	0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x73, 0x74, 0x75,
	0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
	0x8a, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa3, 0xa4, 0xa5,
	0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9,
	0xba, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00, 0xfb,
	0xd0, 0xff, 0xd9,
}

// handleClearThumbCache 清除缩略图缓存。
// POST /api/thumb/clear?path=<目录>  清除指定目录的缩略图（递归），并立即触发重新生成
// POST /api/thumb/clear（无 path）  清除全部缩略图
// 删除操作异步执行，立即返回响应，避免大目录 chmod+删除导致 HTTP 超时
func handleClearThumbCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root := currentThumbRoot()
	if root == "" {
		writeJSON(w, r, map[string]any{"ok": false, "error": "thumb dir not set"})
		return
	}

	targetPath := r.URL.Query().Get("path")
	if targetPath != "" {
		// 该参数是「要重建缩略图的媒体目录」（不是缓存目录），此前完全未校验。
		// 由于下一段会用 filepath.Join(root, sizeDir, relPath) 折算出删除目标，
		// 而 filepath.Join 会把 ../ 折叠掉，未校验时可以用相对路径把删除目标
		// 顶到缩略图根目录之外（实测 ../../../../etc 可越出 /vol）。
		// 这里改为：必须是绝对路径 + 必须在媒体白名单内（相对路径一律拒绝）。
		abs, sc := resolveMediaPath(targetPath)
		if sc != 0 {
			http.Error(w, http.StatusText(sc), sc)
			return
		}
		targetPath = abs
	}

	// 异步执行删除和重新生成，立即返回响应
	go func() {
		// 内存里的元数据同样要失效：否则清完缓存后列表仍显示旧的宽高/时长
		// （purgeCache 那条路径已经这么做了，这里之前漏了）
		dropMetaMem()
		invalidateCacheStats() // 占用统计也要立刻反映，别等 TTL 过期
		if targetPath == "" {
			// 清除全部：先递归 chmod（缩略图目录权限可能是 000，影响 RemoveAll），再删除重建
			filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				// 符号链接本身不能 chmod（Chmod 作用在链目标上，会把链指向的目录/文件改坏）
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil
				}
				if fi.IsDir() {
					_ = os.Chmod(p, 0o755)
				} else {
					_ = os.Chmod(p, 0o644)
				}
				return nil
			})
			_ = os.RemoveAll(root)
			_ = os.MkdirAll(root, 0o755)
			log.Printf("clear: all thumbnails removed (root=%s)", root)
			return
		}

		// 保存原始绝对路径（带前导斜杠），用于后续重新生成
		absPath := filepath.Clean(targetPath)
		// 清除指定目录：遍历 thumbRoot 下所有 size 子目录，删除对应路径
		relPath := strings.TrimPrefix(absPath, "/")

		entries, err := os.ReadDir(root)
		if err != nil {
			log.Printf("clear: ReadDir(%s) failed: %v", root, err)
			return
		}
		log.Printf("clear: root=%s target=%s rel=%s sizeDirs=%d", root, absPath, relPath, len(entries))

		var deleted int64
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// 只处理我们自己创建的 size 子目录（目录名必须是纯数字尺寸）
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			dir := filepath.Join(root, entry.Name(), relPath)
			// 纵深防御：清理后的目标必须仍在缩略图根目录之内
			if !underRoot(dir, root) {
				log.Printf("clear: refuse %s (escapes root %s)", dir, root)
				continue
			}
			info, e := os.Stat(dir)
			if e != nil {
				log.Printf("clear: skip %s (stat failed: %v)", dir, e)
				continue
			}
			if !info.IsDir() {
				continue
			}
			// 统计文件数
			filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
				if err == nil && !fi.IsDir() {
					deleted++
				}
				return nil
			})
			// 先递归 chmod 所有目录和文件（缩略图目录权限可能是 000），确保 RemoveAll 能成功
			filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				// 同上：chmod 不跟随符号链接
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil
				}
				if fi.IsDir() {
					_ = os.Chmod(p, 0o755)
				} else {
					_ = os.Chmod(p, 0o644)
				}
				return nil
			})
			if err := os.RemoveAll(dir); err != nil {
				log.Printf("clear: RemoveAll(%s) FAILED: %v", dir, err)
			} else {
				log.Printf("clear: removed %s", dir)
			}
		}
		log.Printf("clear: done, deleted %d files for %s", deleted, absPath)

		// 清除后触发当前目录的缩略图重新生成（后台预生成）
		time.Sleep(200 * time.Millisecond)
		resp, err := listDir(absPath, "name")
		if err != nil {
			log.Printf("rebuild: list %s failed: %v", absPath, err)
			return
		}
		var enqueued int
		for i := range resp.Files {
			if i >= 50 {
				break
			}
			enqueueThumb(resp.Files[i].Path, resp.Files[i].Kind, thumbRequestSize())
			enqueued++
		}
		log.Printf("rebuild: enqueued %d files for %s", enqueued, absPath)
	}()

	writeJSON(w, r, map[string]any{"ok": true})
}

// handlePauseThumb 暂停后台缩略图预生成（大图浏览时调用，避免抢占资源）。
// 暂停是「抢占式」的：取消正在跑的后台任务（kill 掉 ffmpeg），让资源立刻让出来。
// 注意**不清空待办队列** —— 队列里的任务恢复后会继续处理，不会因为用户看了一眼图就永久丢失；
// 至于"恢复后会不会一口气跑十几张"，已由「活动感知退让 + 给前台预留槽」兜住。
func handlePauseThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 用户开始看图 = 前台活动，后台在此之前就该静默
	touchThumbActivity()
	thumbPauseMu.Lock()
	thumbPaused = true
	thumbPauseUntil = time.Now().Add(thumbPauseTTL) // 无 resume 时自动到期恢复
	thumbPauseMu.Unlock()
	// 抢占：取消在跑的后台任务（ffmpeg 被 kill，半成品不落缓存）；
	// 被抢占的任务会由 worker 重新入队，等恢复后重试。
	cancelBackgroundCtx()
	writeJSON(w, r, map[string]any{"ok": true})
}

// handleResumeThumb 恢复后台缩略图预生成
func handleResumeThumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// close(thumbResumeCh) 与重新赋值必须在同一把锁内原子完成：
	// 否则并发调用 resume 会 close 一个已 close 的 channel → panic；
	// worker 在锁外读取全局变量也会与这里的替换产生数据竞争。
	thumbPauseMu.Lock()
	thumbPaused = false
	thumbPauseUntil = time.Time{}
	close(thumbResumeCh) // 广播恢复信号：所有持有旧引用的 worker 同时被唤醒
	thumbResumeCh = make(chan struct{})
	thumbPauseMu.Unlock()
	writeJSON(w, r, map[string]any{"ok": true})
}
