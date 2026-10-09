package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// scaledSem 限制大图转码（ffmpeg lanczos 缩放）的并发数。
// 每张 7000px 级照片的转码都要全解码（内存数百 MB），不设上限时
// 快速翻页 + 预加载会同时 fork 多个 ffmpeg，在低功耗 NAS 上互相抢 CPU，
// 首图反而更慢。默认 2：一张前台 + 一张后台预加载，足够流畅。
// scaledGenCount 生成计数，用于给 /tmp 清理降频（A5）。
var scaledGenCount int64

// scaledSem 容量 3（原为 2）。实测连切 3 张时第 3 张要等到 6.34s —— 明显是排队，
// 而不是真的慢。3 槽约需 900MB 峰值内存，本机 available 5.3G，有余量。
var scaledSem = make(chan struct{}, 3)

func init() {
	// ensure common media mime types are registered
	extra := map[string]string{
		".mkv": "video/x-matroska", ".mov": "video/quicktime",
		".m4v": "video/x-m4v", ".ts": "video/mp2t", ".mts": "video/mp2t",
		".m2ts": "video/mp2t", ".flv": "video/x-flv", ".wmv": "video/x-ms-wmv",
		".avi": "video/x-msvideo", ".webm": "video/webm", ".heic": "image/heic",
		".heif": "image/heif", ".avif": "image/avif", ".webp": "image/webp",
		".3gp": "video/3gpp", ".rmvb": "application/vnd.rn-realmedia-vbr",
	}
	for ext, t := range extra {
		mime.AddExtensionType(ext, t)
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 大目录（数千文件）的 JSON 可达 1~3 MB，gzip BestSpeed 通常降 70~85%。
	// 前端 fetch 自动带 Accept-Encoding，无需改前端。
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		json.NewEncoder(gz).Encode(v)
		return
	}
	json.NewEncoder(w).Encode(v)
}

// lastListedDir 记录上一次列出的目录。只有当目录**真的换了**才清空后台队列 ——
// 同一目录的重复请求（刷新、排序变化）不应该白清一次。
var lastListedDir atomic.Value // string

func handleList(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity() // 用户正在浏览目录：后台预生成先让路
	q := r.URL.Query()
	path, sc := resolveMediaPath(q.Get("path"))
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "path not found", http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}
	// 1.8.147：换目录时清掉上个目录遗留的缩略图待办。
	// 网格缩略图优先级低，没必要为了旧目录一直生成 —— 把 worker 让给新目录。
	// 安全性见 dropQueuedThumbs 的注释（只丢弃未开始的任务，产物 rename 是原子的）。
	if prev, _ := lastListedDir.Load().(string); prev != path {
		if dropped := dropQueuedThumbs(); dropped > 0 {
			log.Printf("换目录 %q → %q：清掉 %d 个遗留的缩略图待办", prev, path, dropped)
		}
		lastListedDir.Store(path)
	}
	resp, err := listDir(path, q.Get("sort"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 预生成前 12 张（后台预生成关闭时 preloadBatch 直接返回 0，一个都不投）。
	// 限制数量避免进入未缓存目录时大量并发 ffmpeg 导致卡顿。
	// 1.8.146：?preload=N 允许前端「手动生成」按钮请求更大的批量（默认仍是 12）。
	// 上限 200：再大就会一次性堆很多 ffmpeg，反而拖慢当前这一屏。
	preloadN := 12
	if v := q.Get("preload"); v != "" {
		if iv, err := strconv.Atoi(v); err == nil && iv > 0 && iv <= 200 {
			preloadN = iv
		}
	}
	// 1.8.153：进目录时把**整个目录按排序入队**（原来只投前 12 张，覆盖率 2.6%~17%，
	// 观感就是"后台预生成没起作用"）。并发由 worker 数（PreloadConcurrency，默认 2）
	// 限住 —— 一次只有 2 张在跑，所以"全部入队"不会造成 ffmpeg 风暴，只是按顺序慢慢做掉。
	// ?preload=N 是「手动生成」：走独立通道，只受总开关约束（见 preloadBatchManual）。
	if q.Get("preload") != "" {
		preloadBatchManual(resp.Files, preloadN)
	} else {
		preloadBatch(resp.Files, len(resp.Files))
	}
	writeJSON(w, r, resp)
}

// thumbRequestSize 取当前生效的缩略图边长（网格请求大小）
func thumbRequestSize() int {
	s := getSettings().ThumbSize
	if s < 64 || s > 640 {
		return 320
	}
	return s
}

func handleScan(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity() // 用户正在浏览：后台预生成先让路
	q := r.URL.Query()
	path, sc := resolveMediaPath(q.Get("path"))
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	max := 5000
	if m, err := strconv.Atoi(q.Get("max")); err == nil && m > 0 {
		max = m
	}
	files, err := scanRecursive(path, max)
	if err != nil && !isStopErr(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 递归扫描结果里预生成前 5 张（后台预生成关闭时 preloadBatch 直接返回 0）
	preloadBatch(files, 5)
	writeJSON(w, r, map[string]any{"files": files, "count": len(files)})
}

// maxAllowedMaxdim 是 /api/raw 接受的缩放预算上限。
// 与前端 app.js 的 PICK_MAX 一致：前端最多只会要 4096，更大的值只可能来自手工拼 URL。
const maxAllowedMaxdim = 8192

// clampMaxdim 把请求里的缩放预算收敛到服务端允许区间（返回 0 表示「未指定」）。
func clampMaxdim(m int) int {
	if m <= 0 {
		return 0
	}
	if m > maxAllowedMaxdim {
		return maxAllowedMaxdim
	}
	return m
}

func handleRaw(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity() // 用户正在看图：后台预生成让路
	// 光让「后台预生成」让路不够 —— 网格的 size=list 是**前台**请求，会继续占满 CPU。
	// 实测本机 4 核、thumbConcurrency=4 时，4 个缩略图转码把核占满，大图转码几乎
	// 拿不到 CPU，用户看到的就是「要等跑完两页缩略图，大图才出来」。
	// 用与 systemthumb.go 里 size=big 相同的机制：暂停窗口内到达的缩略图请求直接
	// 返回占位图（tryServeDeferredThumb），CPU 全部让给大图。
	// /api/raw 自己走 scaledSem，与缩略图信号量相互独立，不会被这次暂停挡住。
	noteForegroundPreview()
	path, sc := resolveMediaPath(r.URL.Query().Get("path"))
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// 类型闸门：本进程 run-as=root，而白名单是**整个 /vol{n}**。
	// 不加闸门就等于给任何能打开应用的用户一个「读整卷任意文件」的接口
	// （私钥、配置、别人家目录…），而 .html/.svg 还会因同源执行变成存储型 XSS。
	if classify(strings.ToLower(filepath.Ext(path))) == "" {
		http.Error(w, "unsupported type", http.StatusForbidden)
		return
	}
	// 非普通文件一律拒绝：对 FIFO/设备节点 os.Open 会永久阻塞，
	// 而服务端没有写超时 —— 一个请求就能永久占住 goroutine、fd 与转码槽。
	if !info.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusForbidden)
		return
	}
	// 图片查看器可传 maxdim，由后端用 ffmpeg 缩放后输出，
	// 避免浏览器把几万像素的原图全量解码导致内存爆炸。
	maxdim := 0
	if m, err := strconv.Atoi(r.URL.Query().Get("maxdim")); err == nil {
		// 上限必须卡在服务端：本进程 run-as=root，而 maxdim 直接决定 ffmpeg 的
		// 输出尺寸。手工拼 ?maxdim=99999 会按 min(99999, iw) 做一次全尺寸重编码，
		// CPU 与内存随尺寸线性上涨（前端只会传 ≤4096）。
		maxdim = clampMaxdim(m)
	}
	ext := strings.ToLower(filepath.Ext(path))
	// GIF 是动画格式，ffmpeg 缩放会输出静态 JPEG，丢失动画。
	// GIF 直接走原图输出（http.ServeContent 支持 Range），浏览器原生播放动画。
	isGIF := ext == ".gif"

	// ★ 兜底转码必须放在最前面，且【与 maxdim 无关】。
	// heic/heif/tif/tiff 浏览器根本解不了，只能转码。这里的判断刻意不看 maxdim：
	// 无论前端带不带 maxdim，这批格式都必须走 ffmpeg，否则会把 .heic 原样吐给
	// 浏览器 → 表现为「无法加载图片」。（历史上曾经因为这条分支排在 maxdim<=0
	// 之后而永远走不到，1.8.20 起提到最前。）
	if classify(ext) == "image" && !isGIF && ffmpegPath != "" && !browserNativeImage(ext) {
		md := maxdim
		if md <= 0 {
			md = transcodeFallbackMaxdim
		}
		serveScaledImage(w, r, path, md)
		return
	}

	// 是否需要 ffmpeg 转码，分三种情况：
	//   1. 视频 / GIF / 没给 maxdim / 机器上没有 ffmpeg → 不需要，原文件直出
	//   2. 尺寸不超 maxdim 的图 → 直出原文件（转码只会白掉一档画质）
	//   3. 其余 → 交给 ffmpeg 用 lanczos 缩放到 maxdim 再输出
	// 说明：分支 2 的直出既省掉一次 ffmpeg（小图实测约 150ms/张），
	// 也避免"尺寸不变却重新编码"导致的画质损失。
	//
	// 注：1.8.22 起移除了设置页的「源文件直出」开关。大图一律走 ffmpeg 缩放 ——
	// 由服务端 lanczos 降采样，画面明显优于把上万像素的原图丢给浏览器自己缩。
	// 只有"本来就不超过目标尺寸"的图才原样输出，此时 ffmpeg 的 scale 是恒等的，
	// 转一道只会多一次 JPEG 二次编码、白掉一档画质，没有任何收益。
	needScale := false
	switch {
	case maxdim <= 0 || classify(ext) != "image" || ffmpegPath == "" || isGIF:
		needScale = false
	default:
		needScale = !canSkipScale(path, info.Size(), maxdim)
	}
	if needScale {
		serveScaledImage(w, r, path, maxdim)
		return
	}
	// 原文件直出（视频拖动进度 / GIF 动画 / 尺寸未超 maxdim 的图片；支持 Range / 304）
	serveOriginal(w, r, path, info)
}

// browserNativeImage 判断该扩展名的图片能否被浏览器（飞牛桌面内嵌 Chromium）直接解码。
// heic/heif 在 Chromium 下不支持；tif/tiff 除 Safari 外基本都不支持。
// 这两类必须转码，否则会从"清晰"变成"看不见"。
func browserNativeImage(ext string) bool {
	switch ext {
	case ".heic", ".heif", ".tif", ".tiff":
		return false
	}
	return true
}

// transcodeFallbackMaxdim 是「必须转码」但请求没给 maxdim 时的兜底目标尺寸。
// 场景：直接请求 .heic/.tif 却没有带 maxdim（正常情况下前端一定会带，
// 这里只是防御手工拼 URL）。此时不能拿 maxdim=0 去调 ffmpeg
// （scale=min(0,iw) 会产出 0 尺寸），取 4096 作为兜底上限：
// 既不会把图缩过头，也避免超大图全量解码。
const transcodeFallbackMaxdim = 4096

// scaledCacheTag 参与缩放缓存的文件名。缩放语义一旦变化就递增它，
// 让旧算法留下的产物自然失效 —— 否则在 maxdim 相同的情况下
// （例如大屏始终算出 4096）会继续命中旧算法生成的图。
const scaledCacheTag = "v3"

// withinMaxdim 只读图片文件头判断宽高是否都不超过 maxdim，不解码全图，实测约 0.5ms。
// 认不出的格式（webp/heic 等）返回 false，回落到转码分支，行为与改动前一致。
func withinMaxdim(path string, maxdim int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return false
	}
	return cfg.Width > 0 && cfg.Height > 0 && cfg.Width <= maxdim && cfg.Height <= maxdim
}

// canSkipScale 判断这张图能否跳过 ffmpeg 直接输出原图。
//
// 唯一条件是【尺寸不超 maxdim】。这里刻意不看文件体积 ——
// 尺寸不超 maxdim 时，ffmpeg 的 scale 是恒等的（min(N,iw)=iw），转码唯一的效果
// 就是做一次 JPEG 二次编码：白掉一档画质、文件小一点，而客户端完全察觉不到
// （naturalWidth 没变，前端会判定"后端没缩过"，于是放大时也不触发升级，
// 用户只能一直看这张掉了画质的图）。
//
// 原先还要求 size <= 2MiB（本意是"弱网省流量"），但这条阈值的实际命中面很大：
// 本机照片库里 64% 的 JPG 超过 2MiB，其中凡是长边不超过 maxdim 的都会踩中，
// 收益（省几 MB 局域网流量）远小于代价（永久画质损失 + 前端无法自愈）。
// 真正需要压体积的大图，其尺寸必然超过 maxdim，仍会走缩放分支。
func canSkipScale(path string, size int64, maxdim int) bool {
	if size <= 0 {
		return false
	}
	return withinMaxdim(path, maxdim)
}

// 同一张图被并发请求时（预加载与手动点击同时到达）只让一个 goroutine 跑 ffmpeg，
// 其余等锁后直接命中缓存。按 cacheKey 记锁；表超过上限就整体重置
// （重置最坏只会让极端情况多跑一次 ffmpeg，不影响正确性）。
var (
	scaledMuMu  sync.Mutex
	scaledMuMap = map[string]*sync.Mutex{}
)

const scaledMuMapMax = 256

func scaledMutex(key string) *sync.Mutex {
	scaledMuMu.Lock()
	defer scaledMuMu.Unlock()
	if len(scaledMuMap) >= scaledMuMapMax {
		scaledMuMap = map[string]*sync.Mutex{}
	}
	m, ok := scaledMuMap[key]
	if !ok {
		m = &sync.Mutex{}
		scaledMuMap[key] = m
	}
	return m
}

// serveScaledImage 用 ffmpeg 把图片缩放到最长边不超过 maxdim，输出 JPEG。
// 缩放的图先写临时文件再原子 rename 到缓存路径，避免并发读到半成品。
// imageScaleFilter 生成缩图用的滤镜链，语义是"只缩不放"：
//   - min(maxdim,iw/ih) 夹一层：scale 的目标框本身只负责"适应"，
//     直接写 scale=2400:2400 会把 1200px 的小图放大到 2400px（重编码后变糊）；
//   - force_original_aspect_ratio=decrease：保持长宽比，取能放进框的最大尺寸；
//   - flags=lanczos：6000→2400 这种大幅降采样下 bicubic 会产生混叠（摩尔纹/锯齿），
//     lanczos 保留更多细节（实测高频能量 +1.1%）。
//
// 旧版 ffmpeg 不认识 flags=lanczos 时会直接执行失败，调用方的失败分支随即回退到
// 原图直出 —— 结果是更清晰，不会出现坏图。
// bigQualityQScale 大图浏览档的 ffmpeg -q:v。
//
// A2：跟随设置里的 ThumbQuality，但**钳到 [82,86]**（→ 稳定落在 -q:v 4）。
//
//	理由：大图幅面大，编码开销与传输体积都更敏感；82~86 对应 -q:v 4，
//	实测 PSNR > 50 dB（视觉无差别），而原来的硬编码 -q:v 2（≈质量 95）
//	只是白花 18% 时间与 42% 体积。
//	注意：这条只作用于**会走 ffmpeg 转码的预览档**；放大到原图是原图直出、不转码，
//	所以降质量不会影响"放大看原图"的画质。
func bigQualityQScale() int {
	q := getSettings().ViewerQuality
	if q == -1 {
		// 跟随「缩略图质量」，**不钳制** —— 用户在设置里调多少就用多少
		return qualityToQScale(getSettings().ThumbQuality)
	}
	if q == 0 {
		q = 88 // 自动：兼顾清晰与体积
	}
	return qualityToQScale(q)
}

// kindIsImage 判断按扩展名是否为图片（-lowres 只对图片生效）。
func kindIsImage(p string) bool { return classify(filepath.Ext(p)) == "image" }

// scaledParams 返回影响缩放产物内容的两个参数：
// ffmpeg 的 -q:v（由「大图画质」决定）与 -lowres 档位（由「lowres 档位」决定）。
//
// 把这两个值抽出来是必需的，不是整理代码的洁癖：它们必须进入**产物文件名与 ETag**，
// 否则用户改了设置却一直看到旧产物（服务端缓存命中 + 浏览器 immutable 一年）。
func scaledParams(path string, maxdim int) (qScale, lrLevel int) {
	qScale = bigQualityQScale()
	if kindIsImage(path) {
		if ow, oh := imageDimCached(path); ow > 0 && oh > 0 {
			lrLevel = lowresForViewer(ow, oh, maxdim)
		}
	}
	return qScale, lrLevel
}

// scaledOutPath 缩放产物的完整路径。**所有影响输出的参数都要出现在名字里**：
// 算法 tag + 原图标识 + maxdim + 画质 + lowres 档位。
func scaledOutPath(cacheKey string, maxdim, qScale, lrLevel int) string {
	return filepath.Join(os.TempDir(), "mediaview-scaled-"+scaledCacheTag+"-"+cacheKey+"-"+
		strconv.Itoa(maxdim)+"-q"+itoa(qScale)+"-l"+itoa(lrLevel)+".jpg")
}

// scaledETag 与产物名同源的 ETag。前端带 ?v= 时响应是 `immutable, max-age=1年`，
// ETag 不随设置变化的话浏览器连 304 都不会来问。
func scaledETag(cacheKey string, maxdim, qScale, lrLevel int) string {
	return cacheKey + "-" + strconv.Itoa(maxdim) + "-q" + itoa(qScale) + "-l" + itoa(lrLevel)
}

func imageScaleFilter(maxdim int) string {
	return "scale='min(" + strconv.Itoa(maxdim) + ",iw)':'min(" + strconv.Itoa(maxdim) +
		",ih)':force_original_aspect_ratio=decrease:flags=lanczos"
}

func serveScaledImage(w http.ResponseWriter, r *http.Request, path string, maxdim int) {
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cacheKey := cacheKey(path, info)
	// 产物名必须包含**所有影响输出的参数**：画质（-q:v）与 lowres 档位。
	// cacheKey 只含 path+mtime+size，不含设置 —— 少了这两维时，用户在设置里改
	// 「大图画质 / lowres 档位」后服务端判定缓存命中、直接返回旧产物，改了等于没改。
	qScale, lrLevel := scaledParams(path, maxdim)
	out := scaledOutPath(cacheKey, maxdim, qScale, lrLevel)
	// ETag 同样要含这两维（前端带 ?v= 时下发 immutable，若 ETag 不变则浏览器永不重取）
	etag := scaledETag(cacheKey, maxdim, qScale, lrLevel)
	// 缓存判据只取"存在且非空"：cacheKey 里已含原图的 path + mtime + size，
	// 原图一改就是新的文件名，所以产物存在就说明它是当前原图的结果。
	// （旧代码比较产物 mtime 与原图 mtime，两者永不可能相等 → 缓存恒失效，
	//   每次请求都要重跑一遍 ffmpeg。）
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		serveScaledFile(w, r, out, info.ModTime(), etag)
		return
	}

	mu := scaledMutex(out)
	mu.Lock()
	defer mu.Unlock()
	// 等锁期间可能已被其它请求生成好
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		serveScaledFile(w, r, out, info.ModTime(), etag)
		return
	}

	// 大图转码并发上限：每张 7000px 级照片全解码要数百 MB 内存，
	// 不设上限时快速翻页 + 预加载会 fork 多个 ffmpeg 互相抢 CPU。
	// 缓存命中不占槽，只有真正跑 ffmpeg 时才占。
	//
	// 悬停预取走「非阻塞抢槽」：抢不到就直接放弃这次预取，绝不排队 ——
	// 否则预取会挡住用户真正点击打开的那次请求。
	//
	// 「是不是预取」优先看请求头 X-MediaView-Prefetch：前端用 fetch 发预取，
	// URL 与正常打开时**完全一致**，这样打开的 <img> 能直接命中浏览器 HTTP 缓存；
	// 预取性质走请求头，不污染 URL。旧的 ?prefetch=1 形式继续兼容。
	// 注意这里读请求对象而不改函数签名：serveScaledImage 有既有测试直接调用。
	isPrefetch := r.Header.Get("X-MediaView-Prefetch") == "1" || r.URL.Query().Get("prefetch") == "1"

	// 走到这里 = 缓存没命中、真要起一次 ffmpeg 全解码：用户此刻正在等这一张。
	// 这是「用户正在看大图」的触发点**之一** —— /api/raw 是 mediaview 自己查看器的
	// 大图路径（前端 openSingle → showCurrent，含双击图片经 mediaview.open 文件关联
	// 进入的单文件模式）。
	//
	// 另有一条**互相独立**的路径，触发点不在本函数：飞牛文件管理器**原生预览窗口**
	// 直接请求 /thumb/getIcon?size=big，钩子在 systemthumb.go。1.8.56 曾以
	// 「文件管理器从不请求 size=big」为由删掉后者，该结论已证伪（飞牛前端 ImagePlayer
	// 与预览入口都明确请求 big），1.8.61 已恢复。两处触发点并存，分别覆盖
	// 「用 mediaview 看」与「用文件管理器看」，不存在谁替代谁。
	//
	// 触发后：后台预生成整体停摆（kill 在跑的 ffmpeg），文件管理器网格与本地网格的
	// **新增**缩略图请求一律不生成、改回占位图（见 thumb.go 的「大图浏览期间：不生成、
	// 快速返回占位」），把低功耗 NAS 的 CPU 与浏览器连接都让给用户正在等的这一张。
	//
	// 两条边界，都不能少：
	//   1) 放在缓存判定之后 —— 缓存命中的大图是毫秒级返回，让它去停后台毫无收益，
	//      反而会在用户连翻已缓存图片时把后台预生成一直摁住；
	//   2) 排除预取请求 —— 悬停预取会连发好几张，每次都 kill 一轮在跑的后台 ffmpeg
	//      会让预生成反复白干（解码到一半被杀 → 重新入队 → 又被杀）。
	//      用户只是把鼠标划过去，不该算「在看大图」。
	if !isPrefetch {
		noteForegroundPreview()
	}
	// 槽必须在「生成结束」时立刻释放，而不是等整个 handler 返回：
	// 函数末尾还要用 http.ServeContent 把几百 KB 写进连接，而服务端 WriteTimeout=0，
	// 于是一个停滞的客户端（窗口挂起、网关阻塞）就能把槽占死 —— 只有 2 个槽，
	// 全机所有大图请求都会无限排队，且无法自愈（只能重启应用）。
	slotHeld := false
	releaseSlot := func() {
		if slotHeld {
			slotHeld = false
			<-scaledSem
		}
	}
	defer releaseSlot()
	if isPrefetch {
		// 1.8.153：从「立刻 204」改成「**短等待 250ms** 再放弃」。
		//
		// 原来抢不到槽就直接作废 —— 那样虽然绝不挡用户，但忙的时候"预载后两张"约等于没做，
		// 观感就是"预载没起作用"。而 250ms 的等待远小于一次转码（1~3s），
		// 既能等到空档把服务端缓存做掉，又不会长时间占着槽。
		select {
		case scaledSem <- struct{}{}:
			slotHeld = true
		case <-time.After(250 * time.Millisecond):
			w.WriteHeader(http.StatusNoContent) // 忙：作废这次预取，绝不挡用户
			return
		}
	} else {
		scaledSem <- struct{}{}
		slotHeld = true
	}

	tmp := out + ".tmp" + strconv.FormatInt(time.Now().UnixNano(), 36)
	defer os.Remove(tmp)
	vf := imageScaleFilter(maxdim)
	// 用 Background 而非 r.Context()：客户端断开（翻页、窗口关闭）时
	// r.Context() 会被取消，exec.CommandContext 会 Kill 掉正在跑的 ffmpeg，
	// 导致已跑了一半的转码作废、缓存不生成，下一次请求从零重跑（首图等待翻倍）。
	// 改用 Background 后，已开始的转码会跑完并落缓存，60s 超时上限仍在，
	// 避免 ffmpeg 失控堆积。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// A4：图片预览档加动态 -lowres（只解 DCT 低频系数）。
	// 复用缩略图那套 lowresFor 公式 —— 它保证「长边 / 2^N ≥ 目标」，
	// 所以不会出现报告里那个 `min(N, iw)` 把输出缩小的坑（那是手写 N 才有的问题）。
	// 只对图片加：视频要 -ss 抽帧，语义不同。
	var ffArgs []string
	if lrLevel > 0 {
		ffArgs = append(ffArgs, "-lowres", itoa(lrLevel))
	}
	ffArgs = append(ffArgs, "-i", path, "-vf", vf,
		"-f", "image2", "-update", "1", "-q:v", itoa(qScale), "-y", tmp)
	cmd := exec.CommandContext(ctx, ffmpegPath, ffArgs...)
	if err := cmd.Run(); err != nil {
		// 缩放失败回退到原图直出（先把转码槽还回去，别让写响应继续占着它）
		releaseSlot()
		serveOriginal(w, r, path, info)
		return
	}
	// 产物校验：ffmpeg 退出码 0 不代表产物可用。缺这一步时会把 0 字节/残缺 JPEG
	// rename 进缓存并**直接返回给客户端**（浏览器得到破图，而且不像"转码失败"那样
	// 回退原图）；同时因为缓存判据要求 size>0，还会导致每次请求都重跑一遍完整解码。
	if st, serr := os.Stat(tmp); serr != nil || st.Size() == 0 {
		releaseSlot()
		serveOriginal(w, r, path, info)
		return
	}
	if err := os.Rename(tmp, out); err != nil {
		// 某些平台 rename 不覆盖已存在文件，先删再试一次
		_ = os.Remove(out)
		if err := os.Rename(tmp, out); err != nil {
			releaseSlot()
			serveOriginal(w, r, path, info)
			return
		}
	}
	// 生成新文件后顺手清理 /tmp 下超过 1 小时的旧缩放缓存，避免 /tmp 被占满
	// A5：降频到每 20 次生成清一次。原来每生成一张就 os.ReadDir(/tmp)
	// 全目录扫一遍，而 /tmp 是共享目录，文件一多这个 O(n) 扫描会随生成次数累积。
	if atomic.AddInt64(&scaledGenCount, 1)%20 == 0 {
		cleanupScaledTempFiles()
	}
	// 转码已结束、产物已落盘：立刻归还转码槽，后面的文件传输不再占用它
	releaseSlot()
	serveScaledFile(w, r, out, info.ModTime(), etag)
}

// serveOriginal 原文件直出：视频拖动进度、GIF 动画、以及"尺寸未超 maxdim"的图片
// 都走这里。支持 Range（视频 seek）与 If-Modified-Since（304）。
func serveOriginal(w http.ResponseWriter, r *http.Request, path string, info os.FileInfo) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "open failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	// 让浏览器严格按 Content-Type 处理：媒体目录里混进 .html 之类的文件时，
	// 不做嗅探可以避免它被当页面执行（同源 XSS 的第二道闸）。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// SVG 是「可以执行脚本的图片」：它是 XML 文档，直接导航到该 URL 时里面的
	// <script> 会以本应用的**同源**身份执行 —— 本进程 run-as=root，白名单又是整卷
	// /vol{n}，任何人都能往共享目录里放一个 evil.svg 再让人打开。
	// 类型闸门（classify）对 SVG 是放行的（它在 imageExts 里，且界面把 svg 列为
	// 支持格式），所以必须在这里补一道：CSP sandbox + default-src 'none'
	// 既禁止脚本执行、也禁止它去读同源接口，同时不影响把它当图片渲染出来。
	if strings.EqualFold(filepath.Ext(path), ".svg") {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; sandbox")
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// scaledCacheMaxFiles 缩放缓存数量上限。
//
// 1.8.163 修正：这里原来写「/tmp 在 fnOS 上是 tmpfs（内存盘）…占满内存」——
// **实测不成立**，/tmp 挂在根分区（磁盘）上，不会"吃满内存"。
// 前提错了但**上限保留**，换成实际成立的理由：大图转码产物单张可达数 MB，
// 长期不清理会持续占磁盘；而"按时间清理（1 小时）"在连续快速翻很多大图时
// 追不上产生速度 —— 数量上限是那一道兜底。**行为不变，只把理由写对。**
const scaledCacheMaxFiles = 300

// cleanupScaledTempFiles 清理 /tmp 下的 mediaview-scaled-*.jpg。
// 两道清理：① mtime 超过 1 小时的全删；② 剩余总数超过 300 时按 mtime 删最旧的。
// 只在生成新缩放图时调用（缓存命中时不触发）。
func cleanupScaledTempFiles() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	type fileInfo struct {
		path string
		mod  time.Time
	}
	var kept []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mediaview-scaled-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fp := filepath.Join(os.TempDir(), e.Name())
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(fp) // 超过 1 小时直接删
			continue
		}
		kept = append(kept, fileInfo{path: fp, mod: info.ModTime()})
	}
	// 数量超限：按 mtime 从旧到新删，直到不超过上限
	if len(kept) > scaledCacheMaxFiles {
		sort.Slice(kept, func(i, j int) bool { return kept[i].mod.Before(kept[j].mod) })
		for i := 0; i < len(kept)-scaledCacheMaxFiles; i++ {
			_ = os.Remove(kept[i].path)
		}
	}
}

// serveScaledFile 输出已生成的缩放图。
// 缓存策略：no-cache + ETag（ETag 直接用 cacheKey，它已含 path+mtime+size）。
// 旧版用 max-age=86400 但 URL 无版本号 → 同名文件被覆盖后 24h 内浏览器仍用旧图。
// 改 no-cache 后浏览器每次会带 If-None-Match 回来确认，命中即 304（零传输），
// 文件变了 ETag 就变，立刻拿到新图。
func serveScaledFile(w http.ResponseWriter, r *http.Request, out string, modTime time.Time, cacheKey string) {
	f, err := os.Open(out)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	// A7：带版本号（前端 ?v=<mtime>）时用长缓存 —— 翻页回到同一张图可以完全走
	// 浏览器缓存（0 请求、0 解码）。没有版本号（外部工具手工拼 URL）仍用 no-cache，
	// 避免"覆盖同名文件后 24h 内取旧图"。
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", "\""+cacheKey+"\"")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, "preview.jpg", modTime, f)
}

func handleMeta(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity() // 用户正在浏览：后台预生成先让路
	path, sc := resolveMediaPath(r.URL.Query().Get("path"))
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	kind := classify(filepath.Ext(path))
	if m, ok := loadMetaCache(path, thumbRequestSize(), info); ok {
		writeJSON(w, r, m)
		return
	}
	// 用 Background 而非 r.Context()：客户端切走时 r.Context() 会被取消，
	// exec.CommandContext 会 Kill 掉正在跑的 ffprobe，导致 saveMeta 不执行、
	// 下一次请求从零重跑（与转码那条链的修复同源）。改用 Background 后，
	// 已开始的探测会跑完并落缓存，20s 超时上限仍在。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if kind == "video" {
		m, err := probeMeta(ctx, path)
		if err != nil {
			http.Error(w, "probe failed", http.StatusInternalServerError)
			return
		}
		saveMeta(cacheKey(path, info), path, thumbRequestSize(), m)
		writeJSON(w, r, m)
		return
	}
	// image: header dimensions
	w0, h0 := imageDimCached(path)
	m := &Meta{W: w0, H: h0}
	saveMeta(cacheKey(path, info), path, thumbRequestSize(), m)
	writeJSON(w, r, m)
}

func isStopErr(err error) bool {
	return err != nil && err.Error() == "stop"
}
