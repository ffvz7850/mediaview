package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// sizeToPixels 把飞牛的 size 参数映射为缩略图边长
func sizeToPixels(size string) int {
	switch size {
	case "list":
		return 320
	case "medium":
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
func ensureSystemThumb(origPath string, px int) (string, error) {
	// 原文件必须存在
	if _, err := os.Stat(origPath); err != nil {
		return "", fmt.Errorf("source not found: %s", origPath)
	}

	// 用 mediaview 的 ensureThumb 生成（自带缓存和去重，缓存位于用户自定义缩略图目录）
	kind := classify(filepath.Ext(origPath))
	if kind == "" {
		return "", fmt.Errorf("unsupported type: %s", origPath)
	}
	return ensureThumb(origPath, kind, px)
}

// handleSystemThumbGetIcon 处理飞牛文件管理器的缩略图请求
// GET /thumb/getIcon?path=vol3/1000/...&size=list&token=...
func handleSystemThumbGetIcon(w http.ResponseWriter, r *http.Request) {
	apiPath := r.URL.Query().Get("path")
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "list"
	}

	origPath, _, err := parseSystemThumbPath(apiPath, size)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	px := sizeToPixels(size)
	finalPath, err := ensureSystemThumb(origPath, px)
	if err != nil {
		http.Error(w, "thumb unavailable", http.StatusNotFound)
		return
	}

	// 用 http.ServeContent 支持 Range 和缓存
	f, err := os.Open(finalPath)
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

	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, filepath.Base(finalPath), stat.ModTime(), f)
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

	go func() {
		_ = srv.Serve(ln)
	}()

	return nil
}
