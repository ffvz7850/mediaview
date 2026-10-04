package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// scaledSem 限制大图转码（ffmpeg lanczos 缩放）的并发数。
// 每张 7000px 级照片的转码都要全解码（内存数百 MB），不设上限时
// 快速翻页 + 预加载会同时 fork 多个 ffmpeg，在低功耗 NAS 上互相抢 CPU，
// 首图反而更慢。默认 2：一张前台 + 一张后台预加载，足够流畅。
var scaledSem = make(chan struct{}, 2)

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
	resp, err := listDir(path, q.Get("sort"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// enqueue pre-generation for the first batch（缩略图关闭时 enqueueThumb 自身是 no-op）
	// 限制预生成数量，避免进入未缓存目录时大量并发 ffmpeg 导致卡顿
	for i := range resp.Files {
		if i >= 12 {
			break
		}
		enqueueThumb(resp.Files[i].Path, resp.Files[i].Kind, thumbRequestSize())
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
	// enqueue pre-generation for the first batch（缩略图关闭时 enqueueThumb 自身是 no-op）
	for i := range files {
		if i >= 5 {
			break
		}
		enqueueThumb(files[i].Path, files[i].Kind, thumbRequestSize())
	}
	writeJSON(w, r, map[string]any{"files": files, "count": len(files)})
}

// maxAllowedMaxdim 是 /api/raw 接受的缩放预算上限。
// 与前端 app.js 的 PICK_MAX 一致：前端最多只会要 4096，更大的值只可能来自手工拼 URL。
const maxAllowedMaxdim = 4096

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
	out := filepath.Join(os.TempDir(), "mediaview-scaled-"+scaledCacheTag+"-"+cacheKey+"-"+strconv.Itoa(maxdim)+".jpg")
	// 缓存判据只取"存在且非空"：cacheKey 里已含原图的 path + mtime + size，
	// 原图一改就是新的文件名，所以产物存在就说明它是当前原图的结果。
	// （旧代码比较产物 mtime 与原图 mtime，两者永不可能相等 → 缓存恒失效，
	//   每次请求都要重跑一遍 ffmpeg。）
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		serveScaledFile(w, r, out, info.ModTime(), cacheKey)
		return
	}

	mu := scaledMutex(out)
	mu.Lock()
	defer mu.Unlock()
	// 等锁期间可能已被其它请求生成好
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		serveScaledFile(w, r, out, info.ModTime(), cacheKey)
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
		select {
		case scaledSem <- struct{}{}:
			slotHeld = true
		default:
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent) // 204：本次预取作废，用户点击时会重新请求
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
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-i", path, "-vf", vf,
		"-f", "image2", "-update", "1", "-q:v", "2", "-y", tmp)
	if err := cmd.Run(); err != nil {
		// 缩放失败回退到原图直出（先把转码槽还回去，别让写响应继续占着它）
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
	cleanupScaledTempFiles()
	// 转码已结束、产物已落盘：立刻归还转码槽，后面的文件传输不再占用它
	releaseSlot()
	serveScaledFile(w, r, out, info.ModTime(), cacheKey)
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
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// cleanupScaledTempFiles 清理 /tmp 下超过 1 小时的 mediaview-scaled-*.jpg。
// 只在生成新缩放图时调用（缓存命中时不触发），开销可忽略。
func cleanupScaledTempFiles() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mediaview-scaled-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(os.TempDir(), e.Name()))
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
	w.Header().Set("Cache-Control", "no-cache")
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
	w0, h0 := imageDim(path)
	m := &Meta{W: w0, H: h0}
	saveMeta(cacheKey(path, info), path, thumbRequestSize(), m)
	writeJSON(w, r, m)
}

func isStopErr(err error) bool {
	return err != nil && err.Error() == "stop"
}
