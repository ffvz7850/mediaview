package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	appName  = "mediaview"
	version  = "1.8.43"
	gwPrefix = "/app/mediaview"
)

// global runtime directories
var (
	workDir  string // data dir (TRIM_PKGVAR)
	thumbDir string // thumbnail cache (mirror of currentThumbRoot, for logs/compat)
	appDest  string // installed target dir (for bundling ffmpeg)
)

func main() {
	socketPath := flag.String("socket", "", "unix socket path")
	wd := flag.String("workdir", "", "runtime data directory")
	port := flag.Int("port", 8080, "http port (when no socket)")
	dest := flag.String("dest", "", "installed target dir (locates bundled ffmpeg)")
	flag.Parse()

	workDir = *wd
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	appDest = *dest

	// settings must be loaded before anything that depends on them
	loadSettings()
	applyThumbRoot()
	// 根据配置同步系统缩略图服务状态（接管则停止 auto_thumbnailer）
	applySystemThumbTakeover()
	// CPU 核心数限制（cgroups v2 cpuset）
	applyCPUCores()
	// 接管模式下启动 system thumb server，监听 auto_thumbnailer socket 响应文件管理器请求
	if getSettings().TakeoverSystemThumb {
		if err := startSystemThumbServer(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: start system thumb server: %v\n", err)
		}
	}
	_ = os.MkdirAll(workDir, 0o755)

	// locate ffmpeg/ffprobe (bundled first, then system PATH)
	initFFmpeg()

	// start background thumbnail pre-generation worker pool
	// (no-op when thumbnail generation is disabled in settings)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initThumbWorkers(ctx)
	logStartup()

	mux := http.NewServeMux()
	registerRoutes(mux)

	// panic recovery middleware：任何 handler 里的 panic 都返回 500 而不是崩溃整个进程。
	// 缩略图生成、文件列表等复杂逻辑里难免有边界情况，单个请求 panic 不应拖垮服务。
	handler := recoveryMiddleware(mux)

	// common server tuning: timeouts kept generous for streaming Range requests
	// 没有给 --socket（调试/误启动）时只监听回环：本进程 run-as=root 且能读整个 /vol{n}，
	// 绑 0.0.0.0 等于把文件接口暴露到局域网。WriteTimeout 保持 0 是因为视频要长时间流式传输。
	addr := ":" + strconv.Itoa(*port)
	if *socketPath == "" {
		addr = "127.0.0.1:" + strconv.Itoa(*port)
	}
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	// 优雅关闭：飞牛卸载应用时发 SIGTERM，需优雅停止正在处理的请求
	// （缩略图生成、文件传输等），避免临时文件残留和客户端收到截断响应。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// server 在 goroutine 里启动，主 goroutine 等待终止信号
	go func() {
		if *socketPath != "" {
			os.Remove(*socketPath)
			l, err := net.Listen("unix", *socketPath)
			if err != nil {
				log.Fatalf("listen unix %s: %v", *socketPath, err)
			}
			if err := os.Chmod(*socketPath, 0o666); err != nil {
				log.Printf("chmod socket: %v", err)
			}
			log.Printf("mediaview v%s listening on unix:%s", version, *socketPath)
			if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
				log.Fatalf("serve: %v", err)
			}
		} else {
			log.Printf("mediaview v%s listening on http://0.0.0.0:%d", version, *port)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("listen: %v", err)
			}
		}
	}()

	// 阻塞等待终止信号
	sig := <-sigCh
	log.Printf("mediaview: received signal %v, shutting down gracefully...", sig)

	// 1. 停止后台缩略图 worker（cancel 会通过 thumbCtx 通知所有 worker 退出）
	cancel()

	// 2. 优雅关闭 HTTP server：等待正在处理的请求完成（最多 10 秒，超时后强制关闭）
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("mediaview: graceful shutdown error: %v", err)
	} else {
		log.Printf("mediaview: graceful shutdown complete")
	}
}

func registerRoutes(mux *http.ServeMux) {
	// API
	mux.HandleFunc(gwPrefix+"/api/list", handleList)
	mux.HandleFunc(gwPrefix+"/api/scan", handleScan)
	mux.HandleFunc(gwPrefix+"/api/thumb", handleThumb)
	mux.HandleFunc(gwPrefix+"/api/raw", handleRaw)
	mux.HandleFunc(gwPrefix+"/api/meta", handleMeta)
	mux.HandleFunc(gwPrefix+"/api/health", handleHealth)
	// settings（缩略图开关 / 自定义目录 / 缓存管理）
	mux.HandleFunc(gwPrefix+"/api/settings", handleSettings)
	mux.HandleFunc(gwPrefix+"/api/settings/purge", handleSettingsPurge)
	mux.HandleFunc(gwPrefix+"/api/settings/factory_reset", handleFactoryReset)
	mux.HandleFunc(gwPrefix+"/api/thumb/clear", handleClearThumbCache)
	mux.HandleFunc(gwPrefix+"/api/thumb/pause", handlePauseThumb)
	mux.HandleFunc(gwPrefix+"/api/thumb/resume", handleResumeThumb)
	mux.HandleFunc(gwPrefix+"/api/volumes", handleVolumes)
	mux.HandleFunc(gwPrefix+"/api/browse", handleBrowse)
	// static frontend (embedded)
	mux.Handle(gwPrefix+"/", &frontendHandler{})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	s := getSettings()
	writeJSON(w, r, map[string]any{
		"ok":              true,
		"app":             appName,
		"version":         version,
		"ffmpeg":          ffmpegPath != "",
		"ffprobe":         ffprobePath != "",
		"thumb_dir":       currentThumbRoot(),
		"thumb_on":        s.ThumbEnabled,
		"thumb_size":      s.ThumbSize,
		"settings":        settingsFile,
		"thumb_note":      currentThumbNote(),
		"vaapi_device":    detectVAAPIDevice(),
		"gpu_decode":      s.GPUDecode,
		"hw_ok":           atomic.LoadInt64(&hwOKCount),
		"hw_fallback":     atomic.LoadInt64(&hwFallbackCount),
		"hw_circuit_open": hwCircuitOpen(),
		// 缩略图解码并发与历史峰值：用于验证「后台让路」是否把峰值压在 thumb_concurrency 以内
		"thumb_concurrency": s.ThumbConcurrency,
		"thumb_active":      atomic.LoadInt64(&thumbGenActive),
		"thumb_peak":        atomic.LoadInt64(&thumbGenPeak),
	})
}

// logStartup 把配置落到日志里，出问题时一眼能看出"当时是按哪份配置跑的"
func logStartup() {
	s := getSettings()
	log.Printf("mediaview v%s  settings=%s  thumbEnabled=%v thumbDir=%q size=%d quality=%d",
		version, settingsFile, s.ThumbEnabled, currentThumbRoot(), s.ThumbSize, s.ThumbQuality)
	if n := currentThumbNote(); n != "" {
		log.Printf("mediaview: %s", n)
	}
}

func logThumbState(msg string) { log.Printf("mediaview: %s", msg) }

// recoveryMiddleware 捕获下游 handler 中的 panic，记录堆栈并返回 500，
// 避免单个请求的异常（空指针、数组越界等）导致整个进程崩溃。
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("mediaview: panic recovered in %s %s: %v\n%s",
					r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
