package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 系统缩略图接管
//
// 飞牛 fnOS 的 auto_thumbnailer 监听 /var/run/auto_thumbnailer.socket，
// 文件管理器通过 GET /thumb/getIcon?path=volN/uid/relpath&size=list|medium|big
// 获取缩略图。缩略图缓存在 /vol{n}/thumb/{uid}/{size}/{relpath}。
//
// 开启「接管系统缩略图」后，mediaview 停止 auto_thumbnailer，并自己监听
// 同一个 socket，实现相同 API，用自己的缩略图引擎响应文件管理器的请求。
// ---------------------------------------------------------------------------

const systemThumbSocket = "/var/run/auto_thumbnailer.socket"

// sysThumbDebug：设 MEDIAVIEW_SYS_THUMB_DEBUG=1 后记录每一次文件管理器缩略图请求。
// 真机取证用（确认「用文件管理器打开大图浏览」时飞牛到底发的是哪个 size），平时零开销。
var sysThumbDebug = os.Getenv("MEDIAVIEW_SYS_THUMB_DEBUG") == "1"

// sizeToPixels 把飞牛的 size 参数映射为缩略图边长
func sizeToPixels(size string) int {
	switch size {
	case "list":
		return 320
	case "medium":
		// 1.8.126 起 medium 在 handleSystemThumbGetIcon 里已被归一化成 list，
		// 正常流程到不了这里；保留分支只为防御（万一有别的调用方直接调 sizeToPixels）。
		return 800
	case "big":
		return 1920
	default:
		return 320
	}
}

// parseSystemThumbPath 解析 API path 参数，返回原文件绝对路径和系统缩略图路径。
// API path 格式：vol3/1000/备份/照片/IMG.jpg（不带前导 /）
// 原文件路径：/vol3/1000/备份/照片/IMG.jpg
// 缩略图路径：/vol3/thumb/1000/list/备份/照片/IMG.jpg
func parseSystemThumbPath(apiPath, size string) (origPath, thumbPath string, err error) {
	apiPath = strings.TrimSpace(apiPath)
	if apiPath == "" {
		return "", "", fmt.Errorf("empty path")
	}
	// 去掉前导 /
	apiPath = strings.TrimPrefix(apiPath, "/")
	parts := strings.SplitN(apiPath, "/", 3)
	if len(parts) < 3 {
		return "", "", fmt.Errorf("invalid path format: %s", apiPath)
	}
	vol := parts[0] // vol3
	uid := parts[1] // 1000
	rel := parts[2] // 备份/照片/IMG.jpg

	if !strings.HasPrefix(vol, "vol") {
		return "", "", fmt.Errorf("invalid volume: %s", vol)
	}

	origPath = "/" + vol + "/" + uid + "/" + rel
	// 必须走统一入口做范围校验：拼出来的路径直接交给 os.Stat / 缩略图引擎，
	// 而本进程 run-as=root。旧实现只检查了 vol 前缀，rel 里的 ".." 原样保留。
	// 改用 resolveMediaPath 后：Clean 折叠 ".."、落到白名单外即 403。
	origPath, sc := resolveMediaPath(origPath)
	if sc != 0 {
		return "", "", fmt.Errorf("path not allowed: %s", apiPath)
	}
	thumbPath = "/" + vol + "/thumb/" + uid + "/" + size + "/" + rel
	return origPath, thumbPath, nil
}

// ensureSystemThumb 确保缩略图存在，不存在则用 mediaview 引擎生成。
// 缩略图统一存放在用户自定义的缩略图目录（mediaview 缓存），不再写入飞牛系统位置。
// 返回缩略图文件路径。
// immediate=true：这是用户正在等的那一张（大图预览），不让路、不排队。
func ensureSystemThumb(origPath string, px int, immediate bool) (string, error) {
	// 原文件必须存在
	if _, err := os.Stat(origPath); err != nil {
		return "", fmt.Errorf("source not found: %s", origPath)
	}

	// 用 mediaview 的 ensureThumbSys 生成（自带缓存和去重，缓存位于用户自定义缩略图目录）。
	// 用 Sys 而不是普通 ensureThumb：这条路径是文件管理器的网格在拉缩略图，用户此刻
	// 可能在 mediaview 里看大图，要在这里给大图让路（只挡新增解码，不掐断在飞请求）；
	// 反过来，用户正在文件管理器里看大图预览时（immediate=true），这一张自己不让路。
	kind := classify(filepath.Ext(origPath))
	if kind == "" {
		return "", fmt.Errorf("unsupported type: %s", origPath)
	}
	return ensureThumbSys(origPath, kind, px, immediate)
}

// handleSystemThumbGetIcon 处理飞牛文件管理器的缩略图请求
// GET /thumb/getIcon?path=vol3/1000/...&size=list&token=...
func handleSystemThumbGetIcon(w http.ResponseWriter, r *http.Request) {
	// 文件管理器正在渲染缩略图 = 用户此刻就在前台。
	// 必须与 /api/thumb、/api/list 一样打点：否则「接管系统缩略图」开启后，
	// 文件管理器整页请求（list/medium/big 三档）会和后台预生成同时抢 CPU 与信号量槽，
	// 表现为打开目录 / 打开大图预览时要干等。
	touchThumbActivity()

	apiPath := r.URL.Query().Get("path")
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "list"
	}
	// 图标卡片视图（medium）与列表/网格视图（list）**复用同一份产物**。
	//
	// 飞牛文件管理器有两套视图：列表与网格都请求 size=list（320）；
	// 切成图标卡片视图时会请求 size=medium（原实现是 800）——
	// 于是同一张图被生成**两份**，既白占磁盘，切换视图时又要多跑一轮解码。
	//
	// 而 320 在图标视图下已经够看，所以直接把 medium 归一化成 list：
	// 缓存目录名与像素都用 list 的，天然命中已生成的产物，不再产生第二份。
	// （旧实现留下的 /vol*/thumb/<uid>/medium/ 目录会变成无用文件，清一次缓存即可。）
	//
	// size=big（1920，大图预览）**不归一化** —— 那是要看细节的场景。
	if size == "medium" {
		size = "list"
	}
	// 大图预览（size=big）= 用户此刻正在等这一张。这个钩子 1.8.56 曾被删掉，
	// 理由是「文件管理器从不请求 size=big」——**该结论已证伪**，证据是飞牛前端源码：
	//   ImagePlayer-*.js : `{size:B.Big,path:e[t]}`   ← 预览窗口当前图与相邻图都请求 big
	//   index-*.js       : `uW.includes(t)?gX({size:dX.Big,path:e})` ← 预览入口
	// 而网格瓦片用的是 `yX({size: imageSize ?? dX.List})`，imageSize 在全部飞牛 chunk 里
	// 没有任何调用方传值 → 网格恒为 size=list（320）。所以 list=网格、big=大图预览，
	// 两者语义清晰可分。（图标卡片视图的 medium 已被上面的归一化并入 list。）
	//
	// 恢复这个钩子的目的：打开大图预览时**不必先跑完一整页缩略图**。
	//   ① noteForegroundPreview()：后台预生成整体停摆 + kill 掉在跑的预生成 ffmpeg；
	//   ② immediate=true：这一张自己不让路（否则触发暂停后自己再等一轮让路），
	//      并且走预留槽，不排整页 list 请求已经占住的 thumbGenSem FIFO。
	// 注意归一化后的 list（含原 medium 卡片视图）**不**触发：卡片视图一次会来很多张，触发就成了自己挡自己。
	if size == "big" {
		noteForegroundPreview()
	}
	if sysThumbDebug {
		started := time.Now()
		defer func() {
			log.Printf("sys-thumb size=%s path=%s took=%v", size, apiPath, time.Since(started))
		}()
	}

	origPath, _, err := parseSystemThumbPath(apiPath, size)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	px := sizeToPixels(size)

	// 大图浏览期间（size=big 刚触发暂停，或 mediaview 查看器发了 /api/thumb/pause）：
	// 一律**挂起等待**（不返回占位图），把浏览器连接让给正在看的大图。
	//   - 必须放在 ensureSystemThumb **之前**：晚了生成已经跑起来，让路就失去意义；
	//   - size=big 不走这里 —— 它正是触发暂停的那个请求，被自己的暂停挡住就永远出不来图。
	// 大图浏览期间（size=big 刚触发暂停，或 mediaview 查看器发了 /api/thumb/pause）：
	// 【历史 · 已废弃的第一代方案，勿照此实现】list / medium 一律**不生成**，
	// 立刻返回一个 1×1 透明图把响应还回去。
	// ⚠️ 当前实现**不是**这样：现在是上面那段「挂起等待」（见 tryServeDeferredThumb）。
	// 保留这段只为解释下面 1.8.100 那次撤掉的缘由，**不要按它改代码**。
	//
	// 1.8.100 曾撤掉过这个接管，原因是当时的占位图是**白色的 1×1 JPEG**，
	// 用户看到一片白块，观感像坏了。现在改成**透明 GIF**（视觉上就是「还没加载」），
	// 问题消失，而收益保留 —— 这个收益对**飞牛文件管理器**尤其关键：
	//
	//   飞牛打开一个未生成缩略图的目录时会一次发出整页/两页的 list 请求，
	//   mediaview 的前端并发闸管不到它（那是飞牛自己的 JS 在发请求）。几十个响应
	//   回到飞牛后会变成几十个 onload 回调，把它的 JS 主线程占满 —— 此时用户点开
	//   大图，那个点击事件只能排在这些回调后面，表现就是「要等两页缩略图，大图窗口
	//   才出来」。实测后端 size=big 只要 11ms，慢的不是后端，是飞牛主线程在排队。
	//
	//   让后端在大图期间**立刻**把这些请求还回去（响应体仅 43 字节），飞牛的主线程
	//   就不会堆积回调，点击事件能被及时处理。这是应用层唯一能影响飞牛那一侧的手段。
	//
	// 注意 size=big 不走这里 —— 它正是触发暂停的那个请求，被自己的暂停挡住就永远出不来图。
	if size != "big" {
		// ① 大图浏览期间：一律挂起等待（**不是**旧方案的"占位不生成"，见上方历史说明）
		if tryServeDeferredThumb(w, r, origPath, px, "public, max-age=86400") {
			return
		}
	}

	finalPath, err := ensureSystemThumb(origPath, px, size == "big")
	if err != nil {
		http.Error(w, "thumb unavailable", http.StatusNotFound)
		return
	}
	// 用 http.ServeContent 支持 Range 和缓存
	serveThumbFileWithCache(w, r, finalPath, "public, max-age=86400")
}

// handleSystemThumbMeta 处理视频/图片元数据请求（auto_thumbnailer 也提供这些 API）
// 简单返回 204 或空，文件管理器主要用 getIcon
func handleSystemThumbMeta(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// startSystemThumbServer 启动系统缩略图接管服务，监听 /var/run/auto_thumbnailer.socket
func startSystemThumbServer() error {
	// 移除旧 socket（如果存在）
	_ = os.Remove(systemThumbSocket)

	mux := http.NewServeMux()
	mux.HandleFunc("/thumb/getIcon", handleSystemThumbGetIcon)
	mux.HandleFunc("/thumb/getVideoMeta", handleSystemThumbMeta)
	mux.HandleFunc("/thumb/getImageMeta", handleSystemThumbMeta)
	// 兜底：其他路径返回 404，避免 GIN debug 信息泄露
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	ln, err := net.Listen("unix", systemThumbSocket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", systemThumbSocket, err)
	}
	// 飞牛的 socket 权限是 srw-rw-rw-，所有进程都可访问
	_ = os.Chmod(systemThumbSocket, 0o666)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	sysThumbListening.Store(true) // 1.8.164：接管 socket 已开始服务（供 /api/health 观测）
	go func() {
		// 1.8.164：原来这里是 `_ = srv.Serve(ln)` —— **错误被整个丢弃**。
		// Serve 返回（正常关闭或异常）都意味着接管已经停止服务，
		// 此时文件管理器的缩略图会全空，而这正是最难排查的一类故障。
		err := srv.Serve(ln)
		sysThumbListening.Store(false)
		if err != nil && err != http.ErrServerClosed {
			log.Printf("system thumb serve stopped: %v", err)
		}
	}()

	return nil
}

// sysThumbListening 接管 socket 是否正在服务。跨 goroutine 读写，用 atomic。
var sysThumbListening atomic.Bool
