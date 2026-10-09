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
	// 1.8.148：内容标识改存在**产物文件自己的扩展属性**上，不再写 .key sidecar。
	//    xattr 跟着文件走，所以缓存目录变成纯镜像：一张图一个文件、文件名与源文件同名。
	if v, ok := getThumbKeyXattr(out); ok {
		return strings.TrimSpace(v) == key
	}
	// 回退 1：旧缓存里可能还有 .key sidecar（只读，不再写新的）。
	// 这样升级后**已有缓存不会失效**，不会出现"升级一次全量重生成"。
	if b, err := os.ReadFile(out + thumbKeySuffix); err == nil {
		return strings.TrimSpace(string(b)) == key
	}
	// 回退 2：两者都没有（非 Linux / 文件系统不支持 xattr / 缓存被 cp 搬走丢了属性）。
	// 此时退回最宽松的判据：**产物确实存在且非空**就认为可用。
	//
	// ⚠ 必须真的 Stat 一次 —— 否则"文件根本不存在"也会被判成命中，
	//   那就不是"宽松"，而是错的（写这个回退时的第一版就漏了这步）。
	//
	// 代价要说清楚：这条路径下，若有人用 `rsync -a` / `cp -p` 覆盖同名源文件，
	// 时间戳变新，会出现"网格仍是旧图" —— 这正是当初引入 .key 要修的 bug。
	// 之所以可接受：只在 xattr 与旧 sidecar **都不可用**时走到，
	// 且清一次缓存即恢复（开发期本来就会手动清缓存目录）。
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		return true
	}
	return false
}

func writeThumbKey(out, key string) {
	// 1.8.148：写进产物文件的 xattr，不再落 sidecar 文件。
	// 写失败（非 Linux、文件系统不支持）就什么都不做 —— 读取侧有回退，不会坏。
	_ = setThumbKeyXattr(out, key)
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
// 并发数可在设置里调整（1~maxThumbConcurrency，即 1~6），调整后需重启应用生效（信号量只创建一次）。
var (
	thumbGenSem chan struct{}
	thumbSemMu  sync.Mutex
)

// thumbImmediateSem 专供「用户正在等的那一张」的预留槽。
// 目前只有一个调用方：飞牛文件管理器打开大图预览时发来的 size=big（见 systemthumb.go）。
//
// 为什么不共用 thumbGenSem：那是一个普通带缓冲 channel，取槽顺序是**严格 FIFO**。
// 打开一个未缓存的目录时，文件管理器会往 /thumb/getIcon 灌一整页 list 请求，它们先到先得
// 地排在 channel 上；大图请求最后到达，只能排在整页缩略图后面 —— 用户看到的现象就是
// 「必须先跑完一页缩略图，大图浏览窗口才出图」。
// 单张 320px 缩略图要从 6000+ 像素的原图整帧解码，本机实测约 0.6s（NAS 更慢），
// 一页按 50 张、8 并发算就是 4s 起步的纯排队时间。预留一个独立槽后大图不必排任何队，
// 最坏只等当前在跑的那几张解码跑完。
// 代价：极端情况下总并发是 ThumbConcurrency+1，只多一张，而且只给用户正在等的那一张。
var thumbImmediateSem = make(chan struct{}, 1)

// acquireImmediateSlot 取得预留槽，返回释放函数。
// 阻塞等待是安全的：能走这条路的只有「大图预览」，同时最多一两张（查看器会为相邻图
// 预取），等待时间有界；而它换到的是不被整页网格挡住的确定性。
func acquireImmediateSlot() func() {
	thumbImmediateSem <- struct{}{}
	return func() { <-thumbImmediateSem }
}

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
// 峰值并发被放大，低功耗 NAS 上把 CPU 与内存带宽打满，用户此时点开大图只能排队等。
// 这里给后台立两条规矩：
//  1. 后台只用「前台用不到的槽」（非阻塞抢槽），不再与前台叠加；
//  2. 最近有前台请求时后台主动静默，等用户停手（静默期）再干活。
//
// 1.8.66 起 PreloadConcurrency 默认 0（关闭），第 2 条只在用户主动开预生成时才走得到。
const maxThumbConcurrency = 6

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

// ---- 「用户正在看大图」期间，文件管理器的缩略图请求让路 ----
//
// 两个触发源，都指向同一套状态（thumbPaused / thumbPauseUntil）：
//  1. mediaview 自己的前端打开查看器 → POST /api/thumb/pause；
//  2. 飞牛文件管理器打开大图预览 → GET /thumb/getIcon?size=big（见 systemthumb.go）。
//
// 第 2 条曾被误删（1.8.56），理由是「文件管理器从不请求 size=big」。该结论已证伪：
// 飞牛前端 ImagePlayer 组件与预览入口都明确请求 big
// （ImagePlayer-*.js: `{size:B.Big,path:e[t]}`；index-*.js: `gX({size:dX.Big,path:e})`）。

// foregroundPreviewActive 是否处于「用户正在看大图」的暂停窗口内。
func foregroundPreviewActive() bool {
	thumbPauseMu.Lock()
	defer thumbPauseMu.Unlock()
	return thumbPaused && time.Now().Before(thumbPauseUntil)
}

// ---- 大图浏览期间：不生成、快速返回占位 ----
//
// 1.8.60 曾用「把请求挂住」来让路，1.8.66 废弃 —— 它有一条内在矛盾：
// 浏览器对同一 origin 只有 6 条 HTTP/1.1 连接，而在 handler 里挂住 = 占着连接不放，
// 结果是**大图请求自己也发不出去**（浏览器排不到空连接）。挂得越久大图越打不开，
// 正好挡住它要保护的那张图。把窗口从 10s 缩到 1.5s 只是减轻，不能消除。
//
// 正确做法：暂停期间未命中的请求「立刻返回占位图」，把连接马上还回去 ——
// 网格请求秒回、连接释放，大图请求立刻拿得到连接；CPU 也不再跑 ffmpeg。
// 代价是网格里没生成出来的位置暂时是灰块，所以把这些请求记进待补齐队列，
// resume（用户关掉大图窗口）后由后台补生成，用户滚动回来就是缓存命中。

// 前台按需生成兜底；设上限是避免一次长时间大图浏览攒出巨大的补生成风暴。

var ()

// lookupThumbCache 只查缓存、不生成。命中返回产物路径，未命中返回 ""。
// 判据必须与 ensureThumbInternal 一致（存在 + 非空 + 内容标识匹配），
// 否则会出现「这里判命中、生成层判要重做」的分裂。
func lookupThumbCache(path string, size int) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	out := thumbPathFor(path, size)
	ti, err := os.Stat(out)
	if err != nil || ti.Size() <= 0 {
		return ""
	}
	cacheID := cacheKey(path, info) + "|q" + strconv.Itoa(getSettings().ThumbQuality)
	if !thumbCacheKeyMatches(out, cacheID) {
		return ""
	}
	return out
}

// serveThumbFileWithCache 把已生成的缩略图产物发出去（支持 Range）。
// cacheControl 由调用方给：文件管理器那条是 max-age=86400，
// 我们自己前端那条是 immutable 一年。
func serveThumbFileWithCache(w http.ResponseWriter, r *http.Request, path, cacheControl string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "thumb unavailable", http.StatusNotFound)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "thumb unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, filepath.Base(path), stat.ModTime(), f)
}

// thumbSuspendMaxWait 单个 list 请求最多挂起多久。
//
// 为什么必须有上限（1.8.129 采纳代码审查 P1-1）：
//
//	飞牛关闭大图预览时**不发任何请求**，服务端收不到「关闭」事件，暂停只能等
//	thumbPreviewPauseTTL 过期。于是挂起的 list 请求会一直等到 TTL 结束（最长 20 秒），
//	期间**占着浏览器同源 6 条连接**：用户在网格里继续操作、或预览窗口加载相邻大图，
//	都会拿不到连接 —— 这正是 1.8.66 判定为致命并因此删掉「挂住式让路」的那个模式。
//	更糟的是系统缩略图 server 的 WriteTimeout=30s，挂太久会被服务端掐断连接，
//	而飞牛的 <img> 收到错误**不会重试** → 永久破图。
//
// 有界之后：等待中的请求最多占连接这么久，超时就**走正常生成**（不是返回占位图，
// 所以不会出现白格）。既保住「错峰」，又不会长时间冻结网格。
const thumbSuspendMaxWait = 2500 * time.Millisecond

var thumbSuspendedNow int32 // 当前处于挂起中的 list 请求数（供 /api/health 观测）

// waitWhileForegroundPreview 在「大图浏览」期间**挂起**缩略图请求（有上限）。
//
// 为什么是挂起而不是返回占位图：飞牛的 <img> 一旦收到 200（哪怕是 1×1 占位图）
// 就认为「这张加载完了」，**不会再重发请求** —— 那些格子就永久空白。用户实测反馈过
// 「打开大图后后面的缩略图都是白图」。
//
// 与 1.8.66 那版的区别：那时是**无差别挂起**，连「打开大图的导航请求」本身也被挂住
// （它与 list 抢同 6 条连接），结果把大图自己挡死。现在只在大图**已经打开之后**挂起，
// 且加上 thumbSuspendMaxWait 上限，不再出现「关掉大图后网格冻结」。
//
// 返回 true = 可以继续走正常生成；false = 客户端已断开，无需再处理。
func waitWhileForegroundPreview(ctx context.Context) bool {
	if !foregroundPreviewActive() {
		return true
	}
	atomic.AddInt32(&thumbSuspendedNow, 1)
	defer atomic.AddInt32(&thumbSuspendedNow, -1)
	deadline := time.Now().Add(thumbSuspendMaxWait)
	for foregroundPreviewActive() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(150 * time.Millisecond):
		}
	}
	return true
}

// tryServeDeferredThumb 在「正在看大图」期间接管缩略图请求。
//
//   - 命中缓存 → 照常把产物发出去（省的是 CPU，命中本来就不耗 CPU，卡住只会白等）；
//   - 未命中   → 立刻返回占位图，并把这条记进待补齐队列（resume 后补生成）。
//
// 两条路径都返回 true，表示请求已被接管、调用方不要再走生成。
// 不在暂停窗口内返回 false，调用方走正常生成。
//
// 抽成函数而不是在两个 handler 里各写一遍：**判定顺序**（先缓存、且在生成之前）是
// 这条改动的全部要害，散在两处很容易被改歪其中一处。
func tryServeDeferredThumb(w http.ResponseWriter, r *http.Request, path string, size int, cacheControl string) bool {
	if !foregroundPreviewActive() {
		return false
	}
	// 已生成的直接发（不耗 CPU，也不该让用户白等）
	if hit := lookupThumbCache(path, size); hit != "" {
		serveThumbFileWithCache(w, r, hit, cacheControl)
		return true
	}
	// 未生成：**挂起等待**，大图关闭后继续生成并返回真图。
	// 不返回占位图 —— 那会让飞牛的 <img> 认为「已加载完成」而永不重发，留下永久白格。
	if !waitWhileForegroundPreview(r.Context()) {
		return true // 客户端已断开，不需要回任何东西
	}
	return false // 恢复 → 调用方走正常生成路径
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

// thumbPreviewPauseTTL 飞牛文件管理器「正在看大图预览」时的后台暂停时长。
//
// 为什么不用 thumbPauseTTL（2 分钟）：那是给「前端异常关窗、收不到 resume」兜底用的，
// 用在预览场景会让后台预生成停摆过久。
// 为什么不用 thumbQuietPeriod（1.5 秒）：用户逐张翻图时两张之间的间隔常常就超过 1.5 秒，
// 用 1.5 秒会在每次翻页的间隙里让后台复活、下一张又卡。
// 这个窗口是**滑动**的——每次 big 请求都往后推，表达的是「最后一次看大图之后再停 20 秒」。
// 之所以只能滑动、不能精确到「关闭瞬间」：飞牛文件管理器关闭预览时**不发出任何请求**，
// 服务端收不到「关闭」事件（见 systemthumb.go 的端点清单）。
const thumbPreviewPauseTTL = 20 * time.Second

// noteForegroundPreview 标记「用户正在看大图预览」：后台预生成整体停摆，并抢占在跑的任务。
//
// 与 handlePauseThumb 共用同一套状态（thumbPaused / thumbPauseUntil），不新造第二套，
// 否则「前端点开查看器」与「文件管理器打开大图」两处暂停会互相覆盖。
func noteForegroundPreview() {
	thumbPauseMu.Lock()
	thumbPaused = true
	if until := time.Now().Add(thumbPreviewPauseTTL); until.After(thumbPauseUntil) {
		thumbPauseUntil = until // 滑动续期
	}
	thumbPauseMu.Unlock()
	// 抢占：kill 掉在跑的 ffmpeg，把资源真正让出来（只打时间戳停不了已开始的任务）。
	// 被抢占的任务由 worker 重新入队，恢复后重试。
	cancelBackgroundCtx()
}

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

	// 0 = 关闭后台预生成：不启动任何 worker（队列照建，代码路径保持一致；
	// enqueueThumb 在 0 时也直接返回，所以队列不会被投递撑满）。
	n := getSettings().PreloadConcurrency
	if n < 0 {
		n = 0
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
		return
	}
	// 关闭时停止 worker。
	// thumbWorkerCancel 由 spawnThumbWorkers 在 thumbWorkerMu 保护下写入，
	// 这里必须用同一把锁读写，否则与 spawn 并发时构成 data race
	// （原先直接读全局变量 + 赋 nil）。cancel() 放到锁外调用，
	// 避免在持锁期间执行可能阻塞的回调。
	thumbWorkerMu.Lock()
	cancel := thumbWorkerCancel
	thumbWorkerCancel = nil
	thumbWorkerMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// currentThumbQueue 在锁内取出当前的后台队列。
//
// thumbQueue 会被 spawnThumbWorkers 在 thumbWorkerMu 保护下**整体替换**，
// 所以任何读取点都必须走这里。enqueueThumb / requeueThumb 原先直接读全局变量，
// 与替换构成 data race（`go test -race` 可复现）。
func currentThumbQueue() chan thumbReq {
	thumbWorkerMu.Lock()
	q := thumbQueue
	thumbWorkerMu.Unlock()
	return q
}

// preloadEnqueuedCount 进程启动以来**真正投进后台队列**的预生成任务数。
//
// 关掉预生成后这个数恒定不动。它是给用户看的证据：在 NAS 上打 /api/health
// 看到 preload_enqueued 不再增长，比读代码可信得多。
var preloadEnqueuedCount int64

// preloadEnqueued 返回累计投递数（供 /api/health 观测）。
func preloadEnqueued() int64 { return atomic.LoadInt64(&preloadEnqueuedCount) }

// preloadBatch 投递一批后台预生成任务，最多 limit 个，返回**实际入队**的数量。
//
// 这是后台预生成的**唯一投递入口**：进入目录（/api/list）、递归扫描（/api/scan）、
// 清完缓存后重建，三处都走它。开关判断收在本函数最前面 —— 关掉预生成时整批直接
// 跳过，调用方不需要（也不允许）自己再判一次，所以不会出现「某个入口漏判、
// 看起来关了其实还在投」这种让人反复怀疑的情况。
// limit 按场景由调用方给（12 / 5 / 50）。
func preloadBatch(files []FileItem, limit int) int {
	// 缩略图功能整体关闭时同样不投。
	if !getSettings().ThumbEnabled {
		return 0
	}
	// PreloadConcurrency=0 = 关闭后台预生成：这是「进目录别预生成」的落点，必须最先拦住。
	if getSettings().PreloadConcurrency <= 0 {
		return 0
	}
	if limit <= 0 {
		return 0
	}
	size := thumbRequestSize()
	n := 0
	for i := range files {
		if n >= limit {
			break
		}
		// 1.8.153：队列满（enqueueThumb 非阻塞失败）就**停止**继续尝试 ——
		// 反正后面也是失败，白耗 CPU；剩下的等你滚动时前台按需生成。
		if !enqueueThumb(files[i].Path, files[i].Kind, size) {
			break
		}
		n++
	}
	return n
}

// preloadBatchManual 是「手动生成」专用通道：**只受总开关约束，不受后台预生成开关约束**。
//
// 背景（1.8.152 审计发现的真失效）：
//
//	设置面板里「手动生成当前目录缩略图」原本复用 preloadBatch，
//	而 preloadBatch 有两道闸门（ThumbEnabled / PreloadConcurrency<=0），
//	于是**后台预生成关闭时，这个按钮完全无效，却仍然提示"已请求后台生成"** ——
//	实现与 UI 承诺直接矛盾。
//
// 手动生成是用户明确的即时意图，与"要不要在后台常驻预生成"是两件事，所以这里：
//
//	· 只检查 ThumbEnabled；
//	· 需要时**临时拉起 worker**（跑完这批由 restartThumbWorkers 收掉），
//	  否则 PreloadConcurrency=0 时没人消费队列，投了也白投。
func preloadBatchManual(files []FileItem, limit int) int {
	if !getSettings().ThumbEnabled {
		return 0
	}
	ensureManualWorkers()
	size := thumbRequestSize()
	n := 0
	for i := range files {
		if n >= limit {
			break
		}
		if !enqueueThumbForced(files[i].Path, files[i].Kind, size) {
			break
		}
		n++
	}
	return n
}

// ensureManualWorkers 保证队列有消费者。
// 后台预生成开着时本来就有 worker；关着时临时起 2 个，并安排一次收尾。
//
// 1.8.160：**改用互斥量 + 显式标志**，不再依赖可重置的 sync.Once。
//
// 原实现在收尾的 goroutine 里写 `= sync.Once{}` 来"允许下一次手动再拉起"。两点都不对：
//
//	· sync.Once **不是设计来重置的**（Do 过后复位是 hack）；
//	· 那个赋值发生在**另一个 goroutine**，而下次调用会在主 goroutine 读它 → **data race**。
//
// race 的后果不是崩溃，而是**可能丢掉一次拉起**：worker 没起来，而这次的投递又会
// 因为 `enqueueThumbForced` 不看 PreloadConcurrency 而成功 → 任务进了队列却没人消费，
// 表现为"点了立即生成，但一直没动静"，且难以复现。
var (
	manualWorkerMu     sync.Mutex
	manualWorkerActive bool
)

func ensureManualWorkers() {
	if getSettings().PreloadConcurrency > 0 {
		return // 已有常驻 worker
	}
	// 只在"当前没有手动临时 worker 期"时拉起一次
	manualWorkerMu.Lock()
	if manualWorkerActive {
		manualWorkerMu.Unlock()
		return
	}
	manualWorkerActive = true
	manualWorkerMu.Unlock()

	spawnThumbWorkers()
	go func() {
		time.Sleep(30 * time.Second) // 给手动这批留足时间
		restartThumbWorkers()        // 按当前设置重建（PreloadConcurrency=0 → 收掉）
		manualWorkerMu.Lock()
		manualWorkerActive = false // 允许下一次手动再拉起
		manualWorkerMu.Unlock()
	}()
}

// dropQueuedThumbs 丢弃队列里**尚未被 worker 取走**的预生成任务，返回丢弃数量。
//
// 用途：进入新目录时，队列里可能还堆着上一个目录的低优先级任务。
// 网格缩略图优先级本来就低，没必要为了旧目录一直生成 —— 清掉它们，把 worker 让给新目录。
//
// 为什么"不会残存坏图"（这是设计上必须保证的）：
//
//	· 只丢弃**还没被取走**的任务 —— worker 没碰过它们，磁盘上没有任何痕迹；
//	· **正在生成的那一张不打断**：产物是「先写 <name>.tmp，成功后 os.Rename 成正式文件」，
//	  rename 是原子的。所以即便它在半途被中断，残留的也只是一个 .tmp，
//	  而 .tmp 永远不会被当作成品命中（缓存查找只认正式文件名），并会被清理逻辑回收。
//	· 已经被 worker 取走并跑完的任务不受影响：那些缩略图是**有效产物**，
//	  下次进入那个目录反而会命中缓存 —— 不算浪费。
func dropQueuedThumbs() int {
	q := currentThumbQueue()
	if q == nil {
		return 0
	}
	n := 0
	for {
		select {
		case <-q:
			n++
		default:
			return n
		}
	}
}

// enqueueThumb 把单个预生成任务投进后台队列；返回是否真的投进去了。
// 开关关闭、worker 未启动、队列已满都返回 false（那些情况由前台按需生成兜底）。
func enqueueThumb(path, kind string, size int) bool {
	if !getSettings().ThumbEnabled {
		return false
	}
	// 后台预生成已关闭（PreloadConcurrency=0）：直接不投递。
	// 没有 worker 消费，投进去只会白占队列内存，而且这正是用户要的「别进目录就预生成」。
	// 这里与 preloadBatch 重复判断是**有意的**：preloadBatch 是正常入口，
	// 这一层是兜底，保证以后新加的调用点也漏不掉。
	if getSettings().PreloadConcurrency <= 0 {
		return false
	}
	return enqueueThumbTo(path, kind, size)
}

// enqueueThumbForced 与 enqueueThumb 的唯一区别：**不看 PreloadConcurrency**。
// 供「手动生成」使用 —— 用户点按钮是明确的即时意图，与"后台是否常驻预生成"是两件事。
// （1.8.153 修：原来自动/手动共用同一道闸门，导致开关关闭时手动按钮完全无效却提示成功。）
func enqueueThumbForced(path, kind string, size int) bool {
	if !getSettings().ThumbEnabled {
		return false
	}
	return enqueueThumbTo(path, kind, size)
}

// enqueueThumbTo 只做"投递"本身（非阻塞；队列满返回 false）。
func enqueueThumbTo(path, kind string, size int) bool {
	q := currentThumbQueue()
	if q == nil {
		return false
	}
	select {
	case q <- thumbReq{path, kind, size}:
		atomic.AddInt64(&preloadEnqueuedCount, 1)
		return true
	default:
		return false
	}
}

func requeueThumb(req thumbReq) {
	q := currentThumbQueue()
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
	return ensureThumbInternal(context.Background(), path, kind, size, true, false)
}

// ensureThumbSys 系统缩略图接管路径（飞牛文件管理器的 /thumb/getIcon）专用。
//
// immediate=true（大图预览 size=big）：走预留槽、不排 thumbGenSem 的 FIFO。
// 打开未缓存目录时 FileManager 会往这个 socket 灌一整页 list 请求，它们先到先得地
// 排在 channel 上；大图若一起排队就得等整页跑完 —— 这正是预留槽要消除的等待。
//
// 「正在看大图期间不生成」不在这里做，而在 handler 层做（见 handleSystemThumbGetIcon
// 与"占位图快速返回"的思路相关）：那边能在真解码之前就提前返回、把浏览器连接立刻还回去；
// 放在这一层只能挂住请求，反而会占满同源 6 条连接把大图自己挡住。
//
// ctx 同样用 Background（理由见 ensureThumb）。
func ensureThumbSys(path, kind string, size int, immediate bool) (string, error) {
	return ensureThumbInternal(context.Background(), path, kind, size, true, immediate)
}

// ensureThumbBG 后台预生成专用。
// 调用方（worker）必须已持有前台信号量的一个空余槽（见 waitBackgroundTurn）——
// 后台不再「额外」占并发，所以前台+后台的总并发始终不超过 ThumbConcurrency。
// ctx 可被 pause 取消：取消后正在跑的 ffmpeg 被 kill，半成品不会落进缓存。
func ensureThumbBG(ctx context.Context, path, kind string, size int) (string, error) {
	return ensureThumbInternal(ctx, path, kind, size, false, false)
}

// immediate=true 表示「用户正在等的那一张」：取槽时走预留槽、不排网格的队（见 acquireImmediateSlot）。
func ensureThumbInternal(ctx context.Context, path, kind string, size int, foreground, immediate bool) (string, error) {
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
		return generateThumb(ctx, path, kind, size, info, key, cacheID, foreground, immediate)
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

func generateThumb(ctx context.Context, path, kind string, size int, info os.FileInfo, key, cacheID string, foreground, immediate bool) (string, error) {
	// 前台请求在这里阻塞取槽（防止快速滚动时几十个大图同时解码导致 OOM）。
	// 后台任务的槽已由 worker 在进入 singleflight 之前用 tryAcquireThumbSlot 拿到，
	// 这里不再取槽 —— 否则「前台 6 + 后台 3」会重新叠加成 9 个并发解码。
	if immediate {
		// 用户正在等的那一张（文件管理器大图预览）：走预留槽，不排 thumbGenSem 的 FIFO。
		// 打开未缓存目录时整页 list 请求已经先到先得地排在那条 channel 上，
		// 大图若一起排队，用户就得等整页缩略图跑完 —— 这正是本函数要消除的等待。
		release := acquireImmediateSlot()
		defer release()
	} else if foreground {
		sem := getThumbSem()
		// // 前台取槽是**阻塞式**排队（channel 发送）。
		// 注意：前台 ctx 恒为 context.Background()（见 ensureThumbInternal 的调用），
		// 所以下面的 `case <-ctx.Done()` 在当前调用方式下**永远不会触发** ——
		// 保留它是为了让签名对"可取消的调用方"仍然安全。
		// 结论：暂停并不拦截前台取槽；让路由 tryServeDeferredThumb 在更外层完成。
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
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

	if kind == "image" {

		// 回退 Go 原生解码（Go 解码不可中断，开始前先确认没被取消）
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// 优先 libvips（shrink-on-load），失败自动回退 goImageThumb。
		// 实测（320 档、正方形裁剪）：vips 246ms vs ffmpeg 371ms；goImageThumb 更慢。
		// 引擎分流（见 thumbUseFFmpegForImage）：
		//   auto  → 渐进式走 ffmpeg、普通 baseline 走 vips
		//   ffmpeg→ 图片都走 ffmpeg（能吃到 -lowres）
		//   vips  → 只用 vips
		// 实测依据（中位 3 次，均裁剪成正方形）：
		//   普通 baseline 6000x4000 ：320 档 vips 246ms < ffmpeg 371ms（vips 快 34%）
		//   渐进式 102MP 8736x11648：320 档 vips 1682ms > ffmpeg 1670ms，
		//                             1920 档 vips 2247ms > ffmpeg 1775ms（ffmpeg 快 26%）
		// 所以按类型分流，两边各取所长。
		if thumbUseFFmpegForImage(path) && ffmpegPath != "" && info != nil && info.Size() > 1024*1024 {
			ftmp := out + ".gen.jpg"
			_ = os.Remove(ftmp)
			// L1-E：ffmpeg 返回 0 不代表产物可用（磁盘满、被信号 kill 但 Run 未报错、
			// 文件系统错误都可能留下空文件）。补一次 stat + 非空校验，避免坏图进缓存后永久命中。
			// 只 stat 不解码（软件解码不会出绿图，无需像素级检测）。
			if w, h, err := extractImageFrame(ctx, path, size, ftmp); err == nil {
				if st, serr := os.Stat(ftmp); serr == nil && st.Size() > 0 {
					if err := os.Rename(ftmp, out); err == nil {
						if w > 0 && h > 0 {
							saveMeta(key, path, size, &Meta{W: w, H: h})
						}
						return finishThumb(out, cacheID)
					}
				}
			}
			_ = os.Remove(ftmp)
		}
		var ok bool
		var meta *Meta
		if tryVipsImageThumb(ctx, path, size, tmp) {
			ok = true
		} else {
			ok, meta = goImageThumb(path, size, tmp)
		}
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

	// video 或 Go/vips 都解不了的图片格式：交给 ffmpeg
	if ffmpegPath != "" {
		ftmp := out + ".gen.jpg"
		_ = os.Remove(ftmp)
		var ferr error
		if kind == "image" {
			_, _, ferr = extractImageFrame(ctx, path, size, ftmp) // 图片不用 -ss
		} else {
			_, _, ferr = extractVideoFrame(ctx, path, size, ftmp) // 视频 seek 到 0.05s
		}
		if ferr == nil {
			// 与图片分支（L1-E）对齐：ffmpeg 返回 0 不代表产物可用。
			// 这里是**视频**与 HEIC/AVIF 等兜底格式的唯一出口，缺校验时一个 0 字节文件
			// 会被 rename 进缓存并返回给网格/文件管理器 —— 用户看到一张破图，
			// 而且因为缓存判据要求 size>0，之后每次请求都会重烧一次完整解码。
			if st, serr := os.Stat(ftmp); serr != nil || st.Size() == 0 {
				_ = os.Remove(ftmp)
				// 1.8.164：用 errEmptyProduct 而不是 errNoFFmpeg —— ffmpeg 是可用的，
				// 只是产物为空（磁盘满/被杀/文件系统错误），日志不该说成"ffmpeg 不可用"。
				return "", errEmptyProduct
			}
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

// validateJPEG 验证 JPEG 文件能否正常解码，且不是 VAAPI 输出的绿色方块。
// 绿图能正常解码、尺寸也正常，所以必须做像素级检查。
// jpegHeaderOK 只校验 JPEG 文件头有效（不解码像素、不做绿图检测）。
//
// 为什么 vips 路径不需要 validateJPEG：
//
//	validateJPEG 会 jpeg.Decode() **完整解码整张图**，再采样 1024 个像素做**绿图检测**。
//	而绿图是 **VAAPI 硬件解码失败**的特有症状（见 validateJPEG 的注释），
//	vips 是纯软件处理，**不可能产出绿图**；坏文件用 DecodeConfig 也能查出来。
//	成本：DecodeConfig <0.5ms vs 完整解码 2~5ms + 1024 次 img.At() 接口调用。
//
// 绿图检测保留在它该在的地方 —— extractFrameVAAPI 的输出校验。
func jpegHeaderOK(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return false
	}
	return cfg.Width > 0 && cfg.Height > 0
}

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

// jpegIsProgressive 判断 JPEG 是否为渐进式（Progressive DCT）。
//
// 为什么需要它：渐进式 JPEG 把 DCT 系数分散在多次扫描里，**无法使用 DCT 分级解码** ——
// vips 的 shrink-on-load 完全失效（实测 [shrink=8] 与不 shrink 同样耗时 1660ms），
// 而 ffmpeg 的常规解码在渐进式上更快（大图档最多快 58%），且只有它能吃 -lowres。
// 所以 auto 引擎按这个判据分流。
//
// JPEG 的 SOF 标记（0xFFC0~0xFFCF，但要排除不是 SOFn 的三个）：
//
//	0xC0 baseline          0xC1 extended sequential   0xC2 **progressive**
//	0xC3 lossless          0xC4 不是 SOFn（DHT）      0xC5 differential sequential
//	0xC6 **differential progressive**                 0xC7 differential lossless
//	0xC8 不是 SOFn（JPG）   0xC9 extended arithmetic  0xCA **progressive arithmetic**
//	0xCB lossless arith    0xCC 不是 SOFn（DAC）      0xCD dif. sequential arith
//	0xCE **differential progressive arith**           0xCF differential lossless arith
//
// 只读文件头（遇到第一个 SOFn、SOS 或 EOI 就返回），开销极小。
// ---- 文件头探测缓存（L1-D）----
//
// imageDim 与 jpegIsProgressive 都是"只读文件头"的探测，但同一张图在一次会话里会被
// 反复问：列目录算尺寸、生成缩略图时算 -lowres 档位与写 meta、list/medium/big 三档
// 各自问一次 progressive…… 这里按 **path+mtime+size** 缓存，把 N 次读头压成 1 次。
// 键里带 mtime+size 是为了原图被替换/修改时缓存自然失效（与 .key sidecar 同一判据）。
type fileProbe struct {
	w, h        int
	progressive bool
	probed      bool
}

var (
	probeMu    sync.Mutex
	probeCache = make(map[string]fileProbe)
)

const probeCacheMax = 1024

func probeKey(path string, info os.FileInfo) string {
	return path + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
}

// imageDimCached 是 imageDim 的带缓存版本，供缩略图热点路径使用。
func imageDimCached(p string) (int, int) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, 0
	}
	k := probeKey(p, info)
	probeMu.Lock()
	pr, ok := probeCache[k]
	probeMu.Unlock()
	if ok && (pr.w > 0 || pr.h > 0) {
		return pr.w, pr.h
	}
	w, h := imageDim(p)
	probeMu.Lock()
	if len(probeCache) >= probeCacheMax {
		// 探测很便宜，命中率比精确 LRU 更重要，满了就整体清空
		probeCache = make(map[string]fileProbe)
	}
	pr.w, pr.h = w, h
	probeCache[k] = pr
	probeMu.Unlock()
	return w, h
}

// jpegIsProgressiveCached 是 jpegIsProgressive 的带缓存版本。
func jpegIsProgressiveCached(p string) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	k := probeKey(p, info)
	probeMu.Lock()
	pr, ok := probeCache[k]
	probeMu.Unlock()
	if ok && pr.probed {
		return pr.progressive
	}
	v := jpegIsProgressive(p)
	probeMu.Lock()
	if len(probeCache) >= probeCacheMax {
		probeCache = make(map[string]fileProbe)
	}
	pr.progressive = v
	pr.probed = true
	probeCache[k] = pr
	probeMu.Unlock()
	return v
}

func jpegIsProgressive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var soi [2]byte
	if _, err := io.ReadFull(f, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return false // 不是 JPEG
	}
	var one [1]byte
	for {
		var marker [2]byte
		if _, err := io.ReadFull(f, marker[:]); err != nil {
			return false
		}
		if marker[0] != 0xFF {
			return false
		}
		// 允许重复的填充字节 0xFF
		for marker[1] == 0xFF {
			if _, err := io.ReadFull(f, one[:]); err != nil {
				return false
			}
			marker[1] = one[0]
		}
		m := marker[1]
		// SOFn 家族（排除 0xC4/0xC8/0xCC 这三个非 SOFn 标记）
		if m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC {
			switch m {
			case 0xC2, 0xC6, 0xCA, 0xCE:
				return true // Progressive DCT（Huffman 或算术编码）
			default:
				return false // Baseline / Extended / Lossless / Differential
			}
		}
		// SOS：压缩数据开始；EOI：文件结束 —— 都不可能再有 SOF 了
		if m == 0xDA || m == 0xD9 {
			return false
		}
		// 跳过本段
		var lb [2]byte
		if _, err := io.ReadFull(f, lb[:]); err != nil {
			return false
		}
		if segLen := int(binary.BigEndian.Uint16(lb[:])) - 2; segLen > 0 {
			if _, err := f.Seek(int64(segLen), io.SeekCurrent); err != nil {
				return false
			}
		}
	}
}

// isJPEGPath 按扩展名判断是否 JPEG。
func isJPEGPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".jfif", ".jpe":
		return true
	}
	return false
}

// thumbUseFFmpegForImage 决定这张图片是否交给 ffmpeg（受设置的「缩略图压缩引擎」控制）。
//
//	auto（默认）：**按 JPEG 类型自动分流**
//	  渐进式（Progressive DCT）→ ffmpeg
//	    渐进式无法用 DCT 分级解码：vips 的 shrink-on-load 完全失效；
//	    ffmpeg 在渐进式上更快，且只有它能吃 -lowres 这个提速杠杆。
//	  普通 baseline / 非 JPEG → vips
//	    baseline 能用 shrink-on-load，vips 略快，还省一次进程开销。
//	ffmpeg：图片一律交给 ffmpeg。
//	vips  ：不主动交给 ffmpeg（vips 失败时仍会走下面的通用 ffmpeg 兜底）。
func thumbUseFFmpegForImage(path string) bool {
	switch getSettings().ThumbEngine {
	case "ffmpeg":
		return true
	case "vips":
		return false
	default: // auto
		// P1-1：lowres 开启时 **ffmpeg 全面更快** ——
		// 实测（320 档，中位 3 次）：
		//   baseline 24MP：vips 243ms  vs  ffmpeg+lowres3 109ms（快 2.2×）
		//   渐进 102MP  ：vips 1658ms vs  ffmpeg+lowres3 1390ms（快 19%）
		// 而且**只有 ffmpeg 能吃 -lowres**（vips 的命令行里根本没有这个参数）。
		// 所以此时不再按类型分流 —— 否则 baseline 图被分给 vips，白丢 2.2×。
		if getSettings().ThumbLowres {
			return true
		}
		// 只有 lowres 关闭时，才回到"按类型分流"（那时 baseline 走 vips 略快）
		return isJPEGPath(path) && jpegIsProgressiveCached(path)
	}
}

// readJPEGOrientationFrom 从**已打开**的文件读 EXIF Orientation（不自己 open）。
// L1-C：goImageThumb 里已经打开了同一个文件，没必要再 open 一次。
func readJPEGOrientationFrom(f *os.File) int {
	return readJPEGOrientationBody(f)
}

// readJPEGOrientationBody 在已定位到开头的文件上扫描 EXIF。
func readJPEGOrientationBody(f *os.File) int {
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

// vips 探测一次 vipsthumbnail 路径（libvips 未安装则为空，功能自动回退）。
var (
	vipsOnce sync.Once
	vipsPath string
)

// vipsQualitySuffix 构造 vips 的输出选项后缀，画质取自设置里的 ThumbQuality。
//
// 为什么需要它：原来这里硬编码 `[Q=<设置值>,strip]`，于是「缩略图质量」这个设置
// （50~95，默认 80）对 **vips 路径完全无效** —— 只有 ffmpeg（qualityToQScale）
// 和 Go（jpeg.Options{Quality}）两条路径响应它。用户调画质时 vips 出的图纹丝不动。
//
// 缓存 key 里已含画质（cacheID = key + "|q" + ThumbQuality），所以改画质会
// 正常触发重新生成，不存在"改了画质但命中旧缓存"的问题。
func vipsQualitySuffix() string {
	q := getSettings().ThumbQuality
	if q < 50 || q > 95 {
		q = 80 // 与 config.normalize 的兜底保持一致
	}
	return "[Q=" + itoa(q) + ",strip]"
}

func vipsBin() string {
	vipsOnce.Do(func() {
		if p, err := exec.LookPath("vipsthumbnail"); err == nil {
			vipsPath = p
		}
	})
	return vipsPath
}

// tryVipsImageThumb 用 libvips 生成图片缩略图。
//
// 为什么值得单独一条路径：libvips 读 JPEG 时默认 shrink-on-load（按目标尺寸选 DCT
// 缩放尺度），而 goImageThumb 走 image/jpeg 是**整帧解码**再缩放。飞牛 NAS 实测
// （6000x4000 的 7MB JPEG → 320px）：
//
//	vipsthumbnail   210ms
//	ffmpeg          396ms
//	ffmpeg(fast)    365ms
//
// 一屏 50 张缩略图的场景下这是 2.6s 与 7.5s 的差别 —— 用户感知的「打开目录要等两页
// 缩略图」主要就是这段 CPU + 磁盘时间（50 x 7MB = 350MB 的读取量）。
//
// 工具不存在或失败时返回 false，调用方回退原有路径，不改变既有行为。
func tryVipsImageThumb(ctx context.Context, path string, size int, out string) bool {
	bin := vipsBin()
	if bin == "" {
		return false
	}
	tmp := out + ".tmp"
	// -s：长边缩放到 size；--crop：裁成正方形（与 ffmpeg 路径的 increase,crop 语义对齐）；
	// [Q=<设置值>,strip]：去掉元数据并控制体积（画质跟随设置，见 vipsQualitySuffix）。
	cmd := exec.CommandContext(ctx, bin, path, "-s", itoa(size), "--crop",
		"-o", tmp+vipsQualitySuffix())
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return false
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		os.Remove(tmp)
		return false
	}
	// L1-A：只校验文件头。vips 是纯软件处理，不会产出 VAAPI 那种绿图，
	// 没必要为此完整解码一遍（原来这里是 validateJPEG，每张白花 2~5ms）。
	if !jpegHeaderOK(tmp) {
		os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return false
	}
	return true
}

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
	// EXIF 方向非 1（竖拍照片等）：Go image 包不自动旋转，交给下面通用的 ffmpeg 分支
	if _, err := f.Seek(0, 0); err == nil && readJPEGOrientationFrom(f) != 1 {
		return false, &Meta{W: w, H: h}
	}
	// 超大图交给 ffmpeg（分块解码+scale，内存占用低）—— 同样 fall through 到通用分支
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
	// 大图浏览期间不生成：命中缓存的照常返回，未命中的立刻回一张占位图，
	// 把浏览器连接马上还回去（挂住会占满同源 6 连接、反而挡住大图，见上方说明）。
	// 1.8.100：同上，不再用占位图接管（避免白图 + 缓存）
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
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(placeholderThumbGIF)
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

// placeholderThumbGIF 生成失败时的兜底图：1×1 **透明** GIF（43 字节）。
//
// 为什么必须是透明而不是浅色（原实现是一张灰度 1×1 JPEG，291 字节）：
// 深色网格上一块浅色就是「缩略图坏了」的观感，用户会把生成失败当成显示 bug。
// 透明与「还没加载」一致，不引人注意。
//
// 代码审查 P0-1 顺带发现：原来的契约测试 grep 源码里有没有 "image/gif"，
// 而 thumb.go 顶部有一句 `_ "image/gif" // 注册 GIF 解码器` —— 断言被它蒙混成永远为真，
// 真实的占位图其实是浅色 JPEG。本版把**内容**与**断言**一起修正。
var placeholderThumbGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
	0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0x21, 0xF9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00,
	0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x44, 0x01,
	0x00, 0x3B,
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

		// 清除后重建当前目录的缩略图（后台预生成关闭时这里一个都不投）
		time.Sleep(200 * time.Millisecond)
		resp, err := listDir(absPath, "name")
		if err != nil {
			log.Printf("rebuild: list %s failed: %v", absPath, err)
			return
		}
		// 1.8.164：改走 preloadBatchManual。
		// 「清空缓存后立即重建」是**用户刚点了那个按钮**的即时动作，
		// 与「要不要在后台常驻预生成」是两件事 —— 用 preloadBatch 会被
		// PreloadConcurrency<=0 的闸门挡住（返回 0），与本 handler 文档承诺的
		// 「立即触发重新生成」矛盾（与 1.8.153 修过的「手动生成」同一类问题）。
		// 注：功能上不停摆 —— 前端重新加载目录时每个图片的 /api/thumb 前台请求
		// 本就会直接生成；但后台这步"投了 0 个"是实打实的冗余与承诺不符。
		enqueued := preloadBatchManual(resp.Files, 50)
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
	// 用户关掉了大图窗口：把暂停期间被占位挡回去的缩略图补生成，
	// 网格滚动回去就是缓存命中 —— 这就是「关掉大图浏览窗口后再继续生成」。
	writeJSON(w, r, map[string]any{"ok": true})
}
