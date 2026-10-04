package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 运行期配置
//
// 存放位置优先级：
//   1. $MEDIAVIEW_ETC/mediaview.settings.json   （打包脚本注入 TRIM_PKGETC）
//   2. <workdir>/mediaview.settings.json        （兜底，开发/调试用）
//
// 之所以不放在 TRIM_PKGVAR：用户可能把缩略图目录指到别的存储空间，
// 而升级/卸载流程对 PKGVAR 有清理动作；放 PKGETC（/etc）更稳。
// ---------------------------------------------------------------------------

type Settings struct {
	// ThumbEnabled 是否生成缩略图（默认 true，保持原有行为）
	ThumbEnabled bool `json:"thumbEnabled"`
	// ThumbDir 自定义缩略图目录；空字符串 = 使用默认的 <运行期目录>/thumbs
	ThumbDir string `json:"thumbDir"`
	// ThumbSize 生成的缩略图边长（像素），64~640
	ThumbSize int `json:"thumbSize"`
	// ThumbQuality JPEG 质量，50~95
	ThumbQuality int `json:"thumbQuality"`
	// TakeoverSystemThumb 接管系统缩略图：开启后停止并禁用飞牛 auto_thumbnailer 服务，
	// 系统不再生成缩略图，统一由本应用生成。关闭时恢复系统服务。
	TakeoverSystemThumb bool `json:"takeoverSystemThumb"`
	// CPUCores 限制使用的 CPU 核心数（1~NumCPU，0=不限制）。通过 cgroups v2 cpuset 限制。
	CPUCores int `json:"cpuCores"`
	// ThumbConcurrency 缩略图生成并发数（1~8，默认6）。HTTP 请求和后台共用，防止 OOM。
	ThumbConcurrency int `json:"thumbConcurrency"`
	// PreloadConcurrency 后台预生成 worker 数（1~8，默认3）。进入目录后后台预生成缩略图。
	PreloadConcurrency int `json:"preloadConcurrency"`
	// GPUDecode 是否启用 GPU 视频硬件解码（VAAPI，默认 true）。
	// 视频抽帧（H.264/HEVC）走核显硬解，大幅降低 CPU 占用。A8-7680 已实测支持 H.264 + HEVC Main 8-bit。
	GPUDecode bool `json:"gpuDecode"`
	// GPUImageDecode 是否启用 GPU 图片硬件解码（VAAPI JPEG，默认 false，实验性）。
	// AMD Carrizo/GCN 3.0 的 JPEG 硬解在 Mesa radeonsi 下可能产出颜色错误的图（绿图），
	// 仅在 vainfo 明确暴露 VAProfileJPEGBaseline 且实测正常时开启。需同时 GPUDecode=true。
	GPUImageDecode bool `json:"gpuImageDecode"`
	// ViewerPreload 大图浏览时切换后预加载后面几张图（0~5，默认2）。打开时只加载当前图，切换后才预加载。
	ViewerPreload int `json:"viewerPreload"`
	// ViewerAnimation 大图浏览翻页动画效果：slide(滑动)/fade(淡入淡出)/zoom(缩放)/none(无动画)，默认 slide
	ViewerAnimation string `json:"viewerAnimation"`
	// ViewerMode 大图浏览方式：window(独立桌面窗口)/overlay(窗口内遮罩)，默认 window
	ViewerMode string `json:"viewerMode"`
}

var (
	settingsMu   sync.RWMutex
	settings     = defaultSettings()
	settingsFile string // 实际使用的配置文件路径

	// thumbRoot 当前生效的缩略图根目录（已清洗为绝对路径）
	thumbRootMu sync.RWMutex
	thumbRoot   string
	// thumbRootNote 目录回退原因，供设置页提示用户
	thumbRootNote string

	// cacheStats* 缩略图缓存统计的 TTL 缓存（见 cacheStats 注释）
	cacheStatsMu   sync.Mutex
	cacheStatsAt   time.Time
	cacheStatsN    int
	cacheStatsSize int64
)

const cacheStatsTTL = 60 * time.Second

// thumbSubdir 是缩略图在「用户所选目录」之下的专属子目录。
// 有了它，"清空缓存"最多只能删到我们自己创建的东西，不可能碰到用户数据。
const thumbSubdir = ".mediaview-thumbs"

func defaultSettings() Settings {
	return Settings{
		ThumbEnabled:        true,
		ThumbDir:            "",
		ThumbSize:           320,
		ThumbQuality:        80,
		TakeoverSystemThumb: false,
		CPUCores:            0, // 0 = 不限制
		ThumbConcurrency:    6,
		PreloadConcurrency:  3,
		GPUDecode:           true,
		GPUImageDecode:      false, // 图片 JPEG 硬解默认关闭（Carrizo 下可能出绿图）
		ViewerPreload:       2,
		ViewerAnimation:     "slide",
		ViewerMode:          "window",
	}
}

// ---- 读写 ----

func loadSettings() {
	settingsFile = resolveSettingsPath()
	data, err := os.ReadFile(settingsFile)
	if err != nil {
		// 首次运行：写一份默认配置，方便用户/其他 AI 直接看到可改的字段
		settingsMu.Lock()
		settings = defaultSettings()
		settingsMu.Unlock()
		saveSettingsFile()
		return
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		// 配置损坏：退回默认值，但保留原文件（改名备份）以便排查
		if len(data) > 0 {
			_ = os.Rename(settingsFile, settingsFile+".bad")
		}
		settingsMu.Lock()
		settings = defaultSettings()
		settingsMu.Unlock()
		saveSettingsFile()
		return
	}
	s.normalize()
	settingsMu.Lock()
	settings = s
	settingsMu.Unlock()
}

func resolveSettingsPath() string {
	if etc := strings.TrimSpace(os.Getenv("MEDIAVIEW_ETC")); etc != "" {
		if st, err := os.Stat(etc); err == nil && st.IsDir() {
			return filepath.Join(etc, "mediaview.settings.json")
		}
	}
	if strings.TrimSpace(os.Getenv("TRIM_PKGETC")) != "" {
		p := filepath.Join(os.Getenv("TRIM_PKGETC"), "mediaview.settings.json")
		return p
	}
	if dir := settingsDir(); dir != "" {
		return filepath.Join(dir, "mediaview.settings.json")
	}
	return filepath.Join(workDir, "mediaview.settings.json")
}

// settingsDir 返回配置目录（不存在则尝试创建）
func settingsDir() string {
	if etc := strings.TrimSpace(os.Getenv("MEDIAVIEW_ETC")); etc != "" {
		return etc
	}
	if etc := strings.TrimSpace(os.Getenv("TRIM_PKGETC")); etc != "" {
		_ = os.MkdirAll(etc, 0o755)
		return etc
	}
	return workDir
}

func saveSettingsFile() {
	if settingsFile == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(settingsFile), 0o755)
	settingsMu.RLock()
	data, err := json.MarshalIndent(settings, "", "  ")
	settingsMu.RUnlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(settingsFile, data, 0o644)
}

// normalize 把越界值收敛到合法范围
func (s *Settings) normalize() {
	if s.ThumbSize < 64 || s.ThumbSize > 640 {
		s.ThumbSize = 320
	}
	if s.ThumbQuality < 50 || s.ThumbQuality > 95 {
		s.ThumbQuality = 80
	}
	if s.CPUCores < 0 {
		s.CPUCores = 0
	}
	if s.ThumbConcurrency < 1 || s.ThumbConcurrency > 8 {
		s.ThumbConcurrency = 6
	}
	if s.PreloadConcurrency < 1 || s.PreloadConcurrency > 8 {
		s.PreloadConcurrency = 3
	}
	if s.ViewerPreload < 0 || s.ViewerPreload > 5 {
		s.ViewerPreload = 2
	}
	// ViewerAnimation 白名单校验：非法值回落到 slide，避免前端 classList.add 抛
	// InvalidCharacterError 导致查看器空白（设置文件损坏或手滑改坏时触发）。
	switch s.ViewerAnimation {
	case "slide", "fade", "zoom", "none":
	default:
		s.ViewerAnimation = "slide"
	}
	// ViewerMode 白名单校验：非法值回落到 window
	switch s.ViewerMode {
	case "window", "overlay":
	default:
		s.ViewerMode = "window"
	}
	s.ThumbDir = strings.TrimSpace(s.ThumbDir)
}

func getSettings() Settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return settings
}

// ---- 缩略图根目录解析 ----

// expandUser 展开 "~" 与 "~/" 前缀
func expandUser(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// cleanThumbDir 把用户输入的目录清洗成一个绝对路径。第二个返回值为 false 表示无法使用。
func cleanThumbDir(dir string, mustExist bool) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	dir = expandUser(dir)
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		// 相对路径按运行期目录解析
		dir = filepath.Join(workDir, dir)
	}
	if mustExist {
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return dir, false
		}
	}
	return dir, true
}

// resolveThumbRoot 依据当前配置计算缩略图根目录。
// 自定义目录不可创建/不可写时自动回退到默认目录，并把原因记录下来。
func resolveThumbRoot() (string, string) {
	fallback := filepath.Join(workDir, thumbSubdir)
	s := getSettings()
	if s.ThumbDir == "" {
		return fallback, ""
	}
	dir, ok := cleanThumbDir(s.ThumbDir, false)
	if !ok {
		return fallback, "自定义目录无效，已回退到默认目录"
	}
	if !dirUsable(dir) {
		return fallback, "自定义目录不可写（" + dir + "），已回退到默认目录"
	}
	// 只使用 <用户所选目录>/<thumbSubdir>。所有读写都发生在这一层，
	// 用户数据与缩略图缓存物理隔离，"清空缓存"因此不可能删到用户数据。
	return filepath.Join(dir, thumbSubdir), ""
}

func dirUsable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".mediaview-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	_ = f.Close()
	// defer 确保即使进程在 Close 之后、函数返回之前被中断，测试文件也会被清理
	defer os.Remove(probe)
	return true
}

// applyThumbRoot 重新计算并切换缩略图根目录（配置变更后调用）
func applyThumbRoot() {
	root, note := resolveThumbRoot()
	// 缓存根目录必须是**真实目录**：若它（或路径中任何一段）是符号链接，
	// 后面的 MkdirAll / Chmod / RemoveAll 全部会跟着链到别处 ——
	// 用户只要把自己 ThumbDir 下的 .mediaview-thumbs 换成指向 /etc 的链接，
	// 再点一次「清空缓存」，root 进程就会把 /etc 权限改成 0644（丢掉 +x），把系统弄坏。
	if st, err := os.Lstat(root); err == nil && st.Mode()&os.ModeSymlink != 0 {
		note = "缩略图缓存目录是符号链接，已回退到默认目录"
		root = filepath.Join(workDir, thumbSubdir)
	}
	_ = os.MkdirAll(root, 0o755)
	thumbRootMu.Lock()
	thumbRoot = root
	thumbRootNote = note
	thumbRootMu.Unlock()
	if thumbDir != root {
		thumbDir = root
	}
}

// currentThumbRoot 返回当前生效的缩略图根目录（惰性初始化）
func currentThumbRoot() string {
	thumbRootMu.RLock()
	r := thumbRoot
	thumbRootMu.RUnlock()
	if r != "" {
		return r
	}
	applyThumbRoot()
	thumbRootMu.RLock()
	defer thumbRootMu.RUnlock()
	return thumbRoot
}

func currentThumbNote() string {
	thumbRootMu.RLock()
	defer thumbRootMu.RUnlock()
	return thumbRootNote
}

// ---- 目录容量统计（缩略图缓存占用） ----

// cacheStats 扫描整个缩略图缓存，统计文件数与占用。
//
// 这个扫描很重（缓存几万个文件就是几万次 stat），而它被 buildSettingsView 调用，
// 后者在**每次页面加载**时都会跑（boot 里就调 loadSettings，包括每一个独立看图窗口）。
// 所以结果缓存一会儿：用户看到「已缓存 N 个文件」晚 60 秒毫无影响，
// 但服务端省掉了每次开窗/开设置面板的全量遍历。
func cacheStats() (files int, bytes int64) {
	cacheStatsMu.Lock()
	if !cacheStatsAt.IsZero() && time.Since(cacheStatsAt) < cacheStatsTTL {
		n, b := cacheStatsN, cacheStatsSize
		cacheStatsMu.Unlock()
		return n, b
	}
	cacheStatsMu.Unlock()

	root := currentThumbRoot()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// 跳过缩略图的内容标识 sidecar（每个缩略图旁边一个），
		// 否则「已缓存 N 个文件」会凭空翻倍
		if strings.HasSuffix(d.Name(), thumbKeySuffix) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files++
			bytes += info.Size()
		}
		return nil
	})

	cacheStatsMu.Lock()
	cacheStatsN, cacheStatsSize, cacheStatsAt = files, bytes, time.Now()
	cacheStatsMu.Unlock()
	return files, bytes
}

// invalidateCacheStats 让下一次统计重新扫描（清缓存、改缩略图目录后调用）。
func invalidateCacheStats() {
	cacheStatsMu.Lock()
	cacheStatsAt = time.Time{}
	cacheStatsMu.Unlock()
}

// purgeCache 清空缩略图缓存（递归删除 size 子目录及旧版平铺文件）
func purgeCache() (int, error) {
	root := currentThumbRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(root, name)
		if e.IsDir() {
			// 只删我们自己创建的 size 子目录（目录名必须是纯数字尺寸）。
			// 任何非数字目录一律不碰 —— 防止误删用户数据。
			if _, err := strconv.Atoi(name); err != nil {
				continue
			}
			if err := os.RemoveAll(full); err == nil {
				n++
			}
		} else {
			// 旧版兼容：平铺的 hash 命名文件
			if strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".json") ||
				strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".gen.jpg") {
				if err := os.Remove(full); err == nil {
					n++
				}
			}
		}
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// 系统缩略图服务接管
//
// 飞牛 fnOS 自带 auto_thumbnailer.service，会在后台扫描媒体文件并生成
// 系统缩略图。与本应用的缩略图生成重复，进入未缓存目录时双方同时跑
// ffmpeg/解码，CPU/IO 争抢导致卡顿。开启「接管」后停止并禁用该服务，
// 统一由本应用生成缩略图。
// ---------------------------------------------------------------------------

const systemThumbService = "auto_thumbnailer.service"

// systemThumbnailerAvailable 检测系统是否存在 auto_thumbnailer 服务
func systemThumbnailerAvailable() bool {
	out, err := exec.Command("systemctl", "list-unit-files", systemThumbService).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), systemThumbService)
}

// systemThumbnailerActive 检测服务当前是否在运行
func systemThumbnailerActive() bool {
	err := exec.Command("systemctl", "is-active", "--quiet", systemThumbService).Run()
	return err == nil
}

// sysThumb* 缓存这两个 systemctl 查询的结果。
// 每次查询都要 fork 进程 + 扫描 unit 文件（低配 NAS 上几十到几百毫秒），
// 而 /api/settings 在**每个页面加载**（含每个看图窗口）时都会被调用。
// 30 秒内直接复用，用户完全无感。
var (
	sysThumbMu     sync.Mutex
	sysThumbAt     time.Time
	sysThumbAvail  bool
	sysThumbActive bool
)

const sysThumbTTL = 30 * time.Second

// systemThumbnailerState 返回（是否存在, 是否在运行）的缓存结果。
func systemThumbnailerState() (avail, active bool) {
	sysThumbMu.Lock()
	if !sysThumbAt.IsZero() && time.Since(sysThumbAt) < sysThumbTTL {
		a, b := sysThumbAvail, sysThumbActive
		sysThumbMu.Unlock()
		return a, b
	}
	sysThumbMu.Unlock()

	avail = systemThumbnailerAvailable()
	active = avail && systemThumbnailerActive()

	sysThumbMu.Lock()
	sysThumbAvail, sysThumbActive, sysThumbAt = avail, active, time.Now()
	sysThumbMu.Unlock()
	return avail, active
}

// invalidateSysThumbState 在接管/恢复系统缩略图服务后调用，让状态立刻反映变化。
func invalidateSysThumbState() {
	sysThumbMu.Lock()
	sysThumbAt = time.Time{}
	sysThumbMu.Unlock()
}

// disableSystemThumbnailer 停止并禁用系统缩略图服务
func disableSystemThumbnailer() error {
	if err := exec.Command("systemctl", "stop", systemThumbService).Run(); err != nil {
		return err
	}
	_ = exec.Command("systemctl", "disable", systemThumbService).Run()
	return nil
}

// enableSystemThumbnailer 启用并启动系统缩略图服务
func enableSystemThumbnailer() error {
	_ = exec.Command("systemctl", "enable", systemThumbService).Run()
	return exec.Command("systemctl", "start", systemThumbService).Run()
}

// applySystemThumbTakeover 根据配置同步系统缩略图服务状态
func applySystemThumbTakeover() {
	s := getSettings()
	if !systemThumbnailerAvailable() {
		return
	}
	if s.TakeoverSystemThumb {
		_ = disableSystemThumbnailer()
	} else {
		_ = enableSystemThumbnailer()
	}
	invalidateSysThumbState() // 状态刚变过，别让缓存返回旧值
}

// ---------------------------------------------------------------------------
// CPU 使用率限制（cgroups v2）
//
// 通过 cgroups v2 限制整个 mediaview 进程组（含 ffmpeg 子进程）的 CPU 使用率，
// 避免缩略图批量生成时把 CPU 占满导致系统卡顿。飞牛 fnOS 基于 Debian，使用 cgroups v2。
// ---------------------------------------------------------------------------

const cpuCgroupDir = "/sys/fs/cgroup/mediaview"

// cgroupsV2Available 检测系统是否使用 cgroups v2
func cgroupsV2Available() bool {
	// cgroups v2 的特征文件
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return true
	}
	return false
}

// applyCPUCores 根据配置限制使用的 CPU 核心数（cgroups v2 cpuset）。
// cores 为 0 或 >= 总核心数时移除限制。
func applyCPUCores() {
	s := getSettings()
	if !cgroupsV2Available() {
		return
	}

	pid := os.Getpid()
	total := runtime.NumCPU()

	if s.CPUCores <= 0 || s.CPUCores >= total {
		// 不限制：把进程移回根 cgroup，删除 mediaview cgroup
		_ = os.WriteFile("/sys/fs/cgroup/cgroup.procs", []byte(strconv.Itoa(pid)), 0o644)
		_ = os.Remove(cpuCgroupDir)
		return
	}

	// 创建 mediaview cgroup 目录
	if err := os.MkdirAll(cpuCgroupDir, 0o755); err != nil {
		return
	}

	// 设置允许使用的核心：0 ~ (cores-1)
	cpuList := "0-" + strconv.Itoa(s.CPUCores-1)
	if err := os.WriteFile(filepath.Join(cpuCgroupDir, "cpuset.cpus"), []byte(cpuList), 0o644); err != nil {
		return
	}

	// 把当前进程移入 cgroup（子进程会自动继承）
	if err := os.WriteFile(filepath.Join(cpuCgroupDir, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return
	}
}

// ---------------------------------------------------------------------------
// 安全路径白名单
//
// 缩略图目录可以被指到 /vol{n} 之外（用户挂载点），因此不能仅靠 privilege
// 里的 folder-permission 约束。这里维护一份"允许访问的根目录"清单：
//   所有 /vol{n}（动态扫描，不硬编码上限）、$TRIM_DATA_ACCESSIBLE_PATHS、
//   配置目录/运行期目录、当前缩略图目录、以及配置里显式声明的缩略图目录。
// ---------------------------------------------------------------------------

// volDirsCache 缓存扫描到的 /vol{n} 目录列表，带 30 秒 TTL。
// 不用 sync.Once：首次扫描失败（/ 不可读、容器隔离）会永久为空，且热插拔新盘
// （飞牛支持不重启接入新盘，挂载为 /vol11）不会出现在缓存里 → 白名单缺失 → 全部 403。
var (
	volDirsMu    sync.Mutex
	volDirsCache []string
	volDirsAt    time.Time
)

// listVolDirs 动态扫描根目录下所有 /vol{n} 目录，不硬编码 1-10 上限。
// 结果缓存 30 秒：热插拔新盘后最多 30 秒自愈；扫描失败时不覆盖已有结果，
// 避免"一次失败永久失败"导致应用整体不可用。
func listVolDirs() []string {
	volDirsMu.Lock()
	defer volDirsMu.Unlock()

	if time.Since(volDirsAt) < 30*time.Second && volDirsCache != nil {
		return volDirsCache
	}

	entries, err := os.ReadDir("/")
	if err != nil {
		log.Printf("mediaview: listVolDirs: read / failed: %v (white list may be incomplete)", err)
		// 扫描失败时保留已有结果；首次失败则置空切片（非 nil），避免每次都重扫
		if volDirsCache == nil {
			volDirsCache = []string{}
		}
		volDirsAt = time.Now()
		return volDirsCache
	}

	var vols []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "vol") {
			if num, err := strconv.Atoi(strings.TrimPrefix(name, "vol")); err == nil && num > 0 {
				vols = append(vols, "/"+name)
			}
		}
	}
	// 扫描成功但结果为空时也写缓存（空切片非 nil），避免每次调用都重扫 /
	if vols == nil {
		vols = []string{}
	}
	volDirsCache = vols
	volDirsAt = time.Now()
	return volDirsCache
}

// isVolDir 判断清洗后的路径是否是 /vol{n}（n 为任意正整数）
func isVolDir(clean string) bool {
	if !strings.HasPrefix(clean, "/vol") {
		return false
	}
	num, err := strconv.Atoi(strings.TrimPrefix(clean, "/vol"))
	return err == nil && num > 0
}

// isVolUserDir 判断清洗后的路径是否是 /vol{n}/1000（存储空间下的用户主目录）
func isVolUserDir(clean string) bool {
	return samePath(filepath.Base(clean), "1000") && isVolDir(filepath.Dir(clean))
}

func allowedRoots() []string {
	roots := []string{}
	for _, vol := range listVolDirs() {
		roots = append(roots, vol)
	}
	if v := os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS"); v != "" {
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				roots = append(roots, p)
			}
		}
	}
	for _, p := range []string{workDir, appDest, settingsDir(), currentThumbRoot()} {
		if p != "" {
			roots = append(roots, p)
		}
	}
	// 配置里声明过但暂时不可用的目录也要放行，否则用户无法在界面里纠正它
	if s := getSettings(); s.ThumbDir != "" {
		if d, ok := cleanThumbDir(s.ThumbDir, false); ok {
			roots = append(roots, d)
		}
	}
	return roots
}

func underRoot(abs, root string) bool {
	root = filepath.Clean(root)
	if samePath(abs, root) {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(abs, root+sep)
}

// samePath 比较两个路径是否指向同一处。
//
// 必须用 Windows 专用的 EqualFold：如果拿 filepath.Clean 的结果去比对
// 没做过 Clean 的原串（如 "/vol1" vs `\vol1`），分隔符不一致会导致永远判 false，
// 白名单形同虚设 —— 这是实测踩到的坑。
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// pathAllowed 判断媒体路径是否落在白名单内（只做字面比较，范围校验的入口见 resolveMediaPath）
func pathAllowed(abs string) bool {
	for _, r := range allowedRoots() {
		if r == "" {
			continue
		}
		if underRoot(abs, r) {
			return true
		}
		// 根目录本身可能是挂载点/链接（某些 NAS 上 /vol1 就是），
		// 只解析请求侧会把所有合法请求也拒掉，所以根目录也要解析一次。
		if real, ok := resolveRealPath(r); ok && !samePath(real, r) && underRoot(abs, real) {
			return true
		}
	}
	return false
}

// maxLinkHops 限制链接解析跳数，避免 a→b→a 这种环把请求卡死。
const maxLinkHops = 32

// isLinkLike 判断某一段路径是否是「指向别处的链接」。
//   - Unix 符号链接 / Windows 符号链接：Lstat 会带 ModeSymlink
//   - Windows 目录联接（junction）：Lstat 不会标记为符号链接，但 os.Readlink 能读出目标
func isLinkLike(p string) bool {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	if _, err := os.Readlink(p); err == nil {
		return true
	}
	return false
}

// firstLinkComponent 从路径根部逐段查找第一个链接，返回 (链接路径, 其后的剩余部分)。
func firstLinkComponent(abs string) (link, rest string, found bool) {
	vol := filepath.VolumeName(abs)
	cur := vol + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(abs, cur), string(filepath.Separator))
	for i, p := range parts {
		if p == "" {
			continue
		}
		next := filepath.Join(cur, p)
		if isLinkLike(next) {
			return next, filepath.Join(parts[i+1:]...), true
		}
		cur = next
	}
	return "", "", false
}

// resolveRealPath 解析路径中所有链接，返回最终真实路径；解析不出来一律返回 ok=false。
// 不用 filepath.EvalSymlinks：它在遇到「Go 不认作符号链接的重解析点」（如 Windows 目录联接）
// 时直接报错，而"出错就跳过校验"正好等于把校验让过去。自己逐段解析，解析失败一律不放行。
func resolveRealPath(abs string) (string, bool) {
	hops := 0
	for {
		link, rest, found := firstLinkComponent(abs)
		if !found {
			return abs, true
		}
		hops++
		if hops > maxLinkHops {
			return "", false
		}
		target, err := os.Readlink(link)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		abs = filepath.Clean(filepath.Join(target, rest))
	}
}

// resolveMediaPath 是所有媒体接口的统一入口：绝对路径规范化 + 白名单校验。
//
// safePath 只保证"是个绝对路径"，不做任何范围限制；而本进程以 run-as=root 运行，
// 不校验范围等于把整个文件系统暴露给每一个能打开应用的用户（桌面入口 allUsers=true）。
// 因此凡是接受 path 参数去读文件的 handler，都必须走这里。
// 返回 (路径, HTTP 状态码)；状态码为 0 表示通过。
func resolveMediaPath(p string) (string, int) {
	abs, ok := safePath(p)
	if !ok {
		return "", http.StatusBadRequest
	}
	if !pathAllowed(abs) {
		return "", http.StatusForbidden
	}
	// 再把「链接解析后的真实路径」也校验一次 —— 否则在被允许的目录里放一个
	// 符号链接 / 目录联接即可越界。解析不出来（跳数超限/读链接失败）一律拒绝。
	// 返回解析后的路径，让下游 os.Open、缩略图缓存路径都基于同一形态。
	real, ok := resolveRealPath(abs)
	if !ok {
		return "", http.StatusForbidden
	}
	if !samePath(real, abs) && !pathAllowed(real) {
		return "", http.StatusForbidden
	}
	return real, 0
}

// isForbiddenThumbDir 拒绝把"根目录级别"的位置当作缩略图目录。
// 这类位置一旦被选为缩略图目录，清空缓存就等于对存储空间/用户目录做递归删除。
func isForbiddenThumbDir(dir string) bool {
	// 注意：不能用 dir == "/" 这种字符串比较。filepath.Clean 是平台相关的
	// （Windows 上 Clean("/") == `\`），字符串比较会导致守卫静默失效 —— 与本文件
	// samePath 注释里踩过的坑同源。统一走 samePath。
	clean := filepath.Clean(dir)
	if samePath(clean, "/") {
		return true
	}
	// 动态匹配所有 /vol{n} 和 /vol{n}/1000，不硬编码 1-10 上限
	if isVolDir(clean) || isVolUserDir(clean) {
		return true
	}
	for _, p := range []string{workDir, appDest, settingsDir()} {
		if p != "" && samePath(clean, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

type settingsView struct {
	Settings
	DefaultThumbDir   string `json:"defaultThumbDir"`
	ActiveThumbDir    string `json:"activeThumbDir"`
	Note              string `json:"note,omitempty"`
	CacheFiles        int    `json:"cacheFiles"`
	CacheBytes        int64  `json:"cacheBytes"`
	SettingsFile      string `json:"settingsFile"`
	FFmpeg            bool   `json:"ffmpeg"`
	SystemThumbAvail  bool   `json:"systemThumbAvail"`
	SystemThumbActive bool   `json:"systemThumbActive"`
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, buildSettingsView())
	case http.MethodPost, http.MethodPut:
		handleSettingsSave(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func buildSettingsView() settingsView {
	s := getSettings()
	files, bytes := cacheStats()
	sysAvail, sysActive := systemThumbnailerState()
	return settingsView{
		Settings:          s,
		DefaultThumbDir:   filepath.Join(workDir, thumbSubdir),
		ActiveThumbDir:    currentThumbRoot(),
		Note:              currentThumbNote(),
		CacheFiles:        files,
		CacheBytes:        bytes,
		SettingsFile:      settingsFile,
		FFmpeg:            ffmpegPath != "",
		SystemThumbAvail:  sysAvail,
		SystemThumbActive: sysActive,
	}
}

func handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	in.normalize()

	// 先在副本上完成全部校验，任何一项不通过就整体拒绝。
	// （绝不能"边校验边写入"：那样非法请求会把配置改坏一半 —— 实测踩过。）
	prev := getSettings()
	note := ""
	if in.ThumbDir != "" {
		dir, ok := cleanThumbDir(in.ThumbDir, false)
		if !ok {
			http.Error(w, "缩略图目录不合法", http.StatusBadRequest)
			return
		}
		// 目录必须先存在于允许范围内，且不能是某个"根目录"本身
		if !dirExists(dir) {
			http.Error(w, "目录不存在："+dir, http.StatusBadRequest)
			return
		}
		// 白名单校验：必须走 resolveMediaPath 而不是只调 pathAllowed，
		// pathAllowed 只做字面前缀比较，而 currentThumbRoot() 之后会被 os.MkdirAll / purgeCache 使用，
		// 两者都跟随链接。因此在白名单目录里放一个链接，就能把缩略图目录指到白名单外。
		realDir, sc := resolveMediaPath(dir)
		if sc != 0 {
			http.Error(w, "该目录不在允许访问范围内（仅支持存储空间等已授权路径）", http.StatusBadRequest)
			return
		}
		// 拒绝根目录级位置：字面路径与解析后的真实路径都要判。
		if isForbiddenThumbDir(dir) || isForbiddenThumbDir(realDir) {
			http.Error(w, "不能把存储空间根目录或用户主目录设为缩略图目录，请选择其下的一层子目录", http.StatusBadRequest)
			return
		}
		if !dirUsable(dir) {
			http.Error(w, "目录不可用：无法创建或没有写入权限", http.StatusBadRequest)
			return
		}
		in.ThumbDir = dir
	} else if prev.ThumbDir != "" {
		note = "已切回默认目录"
	}
	if prev.ThumbDir != in.ThumbDir {
		note = "缩略图目录已切换，缓存将重新生成"
	}

	settingsMu.Lock()
	settings = in
	settingsMu.Unlock()
	saveSettingsFile()
	applyThumbRoot()
	invalidateCacheStats() // 缩略图目录可能换了，占用数字要重新统计
	if n := currentThumbNote(); n != "" {
		note = n
	}
	// 开关切换后同步 worker 的启停
	restartThumbWorkers()
	// 系统缩略图服务接管
	applySystemThumbTakeover()
	// CPU 核心数限制
	applyCPUCores()

	writeJSON(w, r, map[string]any{
		"ok":       true,
		"settings": buildSettingsView(),
		"note":     note,
	})
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// handleSettingsPurge 清空缩略图缓存（同时清掉内存元数据缓存）
func handleSettingsPurge(w http.ResponseWriter, r *http.Request) {
	// 破坏性操作只接受 POST：否则 <img src=".../api/settings/purge"> 这类
	// 跨站请求就能把别人的缓存清空。
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, err := purgeCache()
	dropMetaMem()
	invalidateCacheStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{"ok": true, "removed": n})
}

// handleFactoryReset 清除所有数据：缩略图缓存 + 设置文件，恢复出厂默认
// 用于用户卸载应用前手动清除数据，避免残留缩略图占用存储空间。
func handleFactoryReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 1. 清除缩略图缓存
	n, _ := purgeCache()
	dropMetaMem()
	invalidateCacheStats()
	// 2. 恢复系统缩略图服务（如果之前接管了）
	if systemThumbnailerAvailable() {
		_ = enableSystemThumbnailer()
		invalidateSysThumbState() // 状态刚变过，别让 30s 缓存返回旧值
	}
	// 3. 重置设置为默认值
	settingsMu.Lock()
	settings = defaultSettings()
	settingsMu.Unlock()
	// 4. 删除设置文件（下次启动时重新生成默认配置）
	if settingsFile != "" {
		_ = os.Remove(settingsFile)
	}
	// 5. 重启 worker（用新的默认配置）
	restartThumbWorkers()
	applyThumbRoot()
	// 6. 恢复 CPU 核心数限制：默认设置 CPUCores=0（不限制），
	// 必须调用 applyCPUCores() 移除之前设置的 cgroup cpuset 限制，
	// 否则设置显示不限制但实际 cgroup 仍限制着 CPU 核心。
	applyCPUCores()
	writeJSON(w, r, map[string]any{"ok": true, "removed": n, "reset": true})
}

// handleVolumes 返回可用的存储空间列表（/vol{n}）。
//
// 前端首页原来硬编码 /vol1..10：① 盘位超过 10 的机器看不到后面的存储空间；
// ② 首屏要发 10 个 /api/list（另外还白搭 10 个毫无用途的 /api/health）。
// 这里一次把真实盘位给出去，前端一个请求就能渲染首页。
func handleVolumes(w http.ResponseWriter, r *http.Request) {
	touchThumbActivity()
	type volEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	out := []volEntry{}
	for _, p := range listVolDirs() {
		if !dirExists(p) {
			continue
		}
		name := filepath.Base(p) // vol3
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "vol")); err == nil {
			name = "存储空间 " + strconv.Itoa(n)
		}
		out = append(out, volEntry{Name: name, Path: p})
	}
	writeJSON(w, r, map[string]any{"volumes": out})
}

// handleBrowse 目录浏览（供设置页挑选缩略图目录）
//
// 起点为「聚合层」：各存储空间的用户目录 /vol{n}/1000 下的内容，用户不必一层层点进来。
func handleBrowse(w http.ResponseWriter, r *http.Request) {
	// 注意：r.URL.Query() 已经做过一次百分号解码，这里不能再解一次。
	// 再解一次会把目录名里真正的 "+" 变成空格（实测 "a%2Bb" -> "a+b" -> "a b"），
	// 导致含 "+" 的目录（IMG+RAW、C++…）在目录选择器里点不进去。
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	type resp struct {
		Path   string  `json:"path"`
		Parent string  `json:"parent"`
		Self   string  `json:"self"`
		Dirs   []entry `json:"dirs"`
	}

	if raw == "" || raw == "/" {
		// 聚合层：把白名单根目录列出来当入口，用户不用一层层点进来。
		// 排序：存储空间 /vol{n}/1000（真人数据）在前，运行期与系统目录在后。
		out := resp{Path: "/", Self: currentThumbRoot(), Dirs: []entry{}}
		seen := map[string]bool{}
		var sys []entry
		candidates := make([]string, 0, 32)
		for _, vol := range listVolDirs() {
			candidates = append(candidates, vol)
		}
		if v := os.Getenv("TRIM_DATA_ACCESSIBLE_PATHS"); v != "" {
			candidates = append(candidates, filepath.SplitList(v)...)
		}
		if s := getSettings(); s.ThumbDir != "" {
			if d, ok := cleanThumbDir(s.ThumbDir, false); ok {
				candidates = append(candidates, d)
			}
		}
		candidates = append(candidates, workDir, settingsDir(), currentThumbRoot())

		for _, root := range candidates {
			root = strings.TrimSpace(root)
			if root == "" {
				continue
			}
			key := strings.ToLower(filepath.Clean(root))
			if seen[key] {
				continue
			}
			if !dirExists(root) {
				continue
			}
			seen[key] = true
			// 各存储空间的用户目录（/vol{n}/1000）优先：那才是用户的照片所在
			if sub, err := defaultUserDir(root); err == nil {
				e := entry{Name: filepath.Base(root) + "/1000", Path: sub}
				if !seen["@"+strings.ToLower(sub)] {
					seen["@"+strings.ToLower(sub)] = true
					out.Dirs = append(out.Dirs, e)
				}
				continue
			}
			sys = append(sys, entry{Name: root, Path: root})
		}
		out.Dirs = append(out.Dirs, sys...)
		writeJSON(w, r, out)
		return
	}

	abs := filepath.Clean(expandUser(raw))
	if !filepath.IsAbs(abs) {
		http.Error(w, "需要绝对路径", http.StatusBadRequest)
		return
	}
	// 链接防护：pathAllowed 只做字面前缀比较，而 os.ReadDir 会跟随符号链接。
	// 只借 resolveMediaPath 的校验，列表与返回仍用字面路径 abs。
	if _, sc := resolveMediaPath(abs); sc != 0 {
		http.Error(w, "路径不在允许范围内", http.StatusForbidden)
		return
	}
	if !dirExists(abs) {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}

	out := resp{Path: abs, Self: currentThumbRoot(), Dirs: []entry{}}
	parent := filepath.Dir(abs)
	if !samePath(parent, abs) && pathAllowed(parent) {
		out.Parent = parent
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out.Dirs = append(out.Dirs, entry{Name: e.Name(), Path: filepath.Join(abs, e.Name())})
	}
	writeJSON(w, r, out)
}

// defaultUserDir 返回 /vol{n}/1000（不存在则报错）
func defaultUserDir(root string) (string, error) {
	p := filepath.Join(root, "1000")
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return "", os.ErrNotExist
	}
	return p, nil
}
