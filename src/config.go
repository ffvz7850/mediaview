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
	// ThumbEngine 缩略图压缩引擎：
	//   "auto"（默认）：**按 JPEG 类型自动分流** —— 渐进式图走 ffmpeg、
	//                   普通 baseline 图走 vips（见 thumbUseFFmpegForImage 的说明）。
	//   "vips"        ：只用 libvips。
	//   "ffmpeg"      ：图片一律走 ffmpeg（能吃 -lowres 提速）。
	// 实测：普通图 vips 快 34%；渐进式 102MP 图 ffmpeg 快 26%（1920 档）。
	ThumbEngine string `json:"thumbEngine"`
	// ThumbLowres 图片缩略图是否用 ffmpeg 的 -lowres 快速解码（默认 true）。
	// -lowres N 让解码器只取 DCT 低频系数，直接产出 1/2^N 尺寸的图像 ——
	// 这正是 vips 的 shrink-on-load 在**渐进式 JPEG** 上做不到的事。
	// 本机实测（6000x4000 与 8736x11648 真实样本）：
	//   普通图 →320 档：387ms → 173ms（快 55%）
	//   渐进式 102MP →320 档：1452ms → 1035ms（快 29%）
	//   渐进式 102MP →1920 档：1582ms → 1169ms（快 26%）
	// 档位按目标尺寸动态计算（lowresFor），取小了没收益、取大了会把图放大糊掉。
	ThumbLowres bool `json:"thumbLowres"`

	// ---- 1.8.144 新增：大图预览的画质 / 清晰度 / lowres 档位都能在设置里调 ----

	// ViewerQuality 大图预览的 JPEG 画质：
	//   0   = 自动（88，兼顾清晰与体积）
	//   -1  = 跟随「缩略图质量」，**不再钳制**（用户设多少就用多少）
	//   84/88/95 = 固定画质（实测：84→-q:v 4、88→3、95→2；90/92 与 88 等价、98 与 95 等价，故不列）
	//
	// 背景：1.8.137 起大图质量曾是"跟随缩略图质量但钳到 [82,86]"（恒为 -q:v 4），
	// 于是设置里的质量对大图预览形同虚设 —— 用户调得越高，被砍得越狠。
	ViewerQuality int `json:"viewerQuality"`

	// ViewerMaxdim 大图预览的清晰度上限（像素长边）：
	//   0 = 自动（按视口 × 1.4 计算，clamp 到 [2048,4096]）；2048 = 默认
	//   2048/3072/4096/5120/6144 = 固定档位
	ViewerMaxdim int `json:"viewerMaxdim"`

	// ViewerLowres 大图预览的低分辨率解码档位（ffmpeg -lowres）：
	//   0 = 自动（lowresFor 公式，保证「长边/2^N ≥ 目标×0.9」）
	//   -1 = 关闭（完整解码）
	//   1/2/3 = 固定档位（档位越高越快，但可能轻微缩水）
	ViewerLowres int `json:"viewerLowres"`

	// ThumbLowresLevel 缩略图的低分辨率解码档位（与 ThumbLowres 那个总开关叠加）：
	//   0 = 自动（公式）；-1 = 关闭；1/2/3 = 固定档位
	ThumbLowresLevel int `json:"thumbLowresLevel"`
	// TakeoverSystemThumb 接管系统缩略图：开启后停止并禁用飞牛 auto_thumbnailer 服务，
	// 系统不再生成缩略图，统一由本应用生成。关闭时恢复系统服务。
	TakeoverSystemThumb bool `json:"takeoverSystemThumb"`
	// CPUCores 限制使用的 CPU 核心数（1~NumCPU，0=不限制）。通过 cgroups v2 cpuset 限制。
	CPUCores int `json:"cpuCores"`
	// ThumbConcurrency 缩略图生成并发数（1~maxThumbConcurrency，默认按核数收敛，见 thumbConcurrencyDefault）。
	ThumbConcurrency int `json:"thumbConcurrency"`
	// PreloadConcurrency 后台预生成 worker 数（0~8，默认0=关闭）。
	// 0 = 不启动 worker、也不主动投递：进入目录后完全由前台按需生成。
	PreloadConcurrency int `json:"preloadConcurrency"`
	// SchemaVersion 配置结构版本，仅供一次性迁移使用（见 migrate）。
	// 前端 body 不含该字段，由服务端在保存时补上当前值。
	SchemaVersion int `json:"schemaVersion"`
	// GPUDecode 是否启用 GPU 视频硬件解码（VAAPI，默认 true）。
	// 视频抽帧（H.264/HEVC）走核显硬解，大幅降低 CPU 占用。A8-7680 已实测支持 H.264 + HEVC Main 8-bit。
	GPUDecode bool `json:"gpuDecode"`
	// ViewerPreload 大图浏览时切换后预加载后面几张图（0~5，默认2）。打开时只加载当前图，切换后才预加载。
	ViewerPreload int `json:"viewerPreload"`
	// ViewerAnimation 大图浏览翻页动画效果：slide(滑动)/fade(淡入淡出)/zoom(缩放)/none(无动画)，默认 slide
	ViewerAnimation string `json:"viewerAnimation"`
	// ViewerMode 大图浏览方式：window(独立桌面窗口)/overlay(窗口内遮罩)，默认 window
	ViewerMode string `json:"viewerMode"`
	//
	// 为什么必须是 *bool 而不是 bool：loadSettings 是「从零值 struct 开始
	// json.Unmarshal」的，用 bool 的话「配置文件里没有这个字段」（老版本升级上来的
	// 用户，占绝大多数）和「用户明确关掉了」会变成同一个值 false —— 升级即被静默
	// 改成关闭。用指针区分「没写过」(nil) 与「写成了 false」，nil 按默认值 true 处理。
}

// thumbConcurrencyDefault 缩略图并发的默认值：按 CPU 核数收敛，**永远给前台留 2 个核**。
//
// 为什么不是固定值：缩略图是批量的（打开一个目录可能几十张），大图转码是单张但用户
// 正在等。固定并发在 4 核机器上会把核占满（默认原来是 3，用户设置常见 4），
// 于是点大图时它抢不到 CPU，只能干等 —— 实测 4 核机器上大图与缩略图同时解码时
// 大图耗时数倍于空闲时。按核数留出余量后，缩略图侧最多用 nproc-2 个核。
//
// 用户仍可在设置里手动调整（1~maxThumbConcurrency），此项只影响默认值。
func thumbConcurrencyDefault() int {
	// 实测（4 核 NAS、6000x4000 原图）缩略图生成期间：
	//   CPU 71% 空闲、14% wa、后端 size=big 仅 11ms —— **并发不是争用源**。
	// 而并发偏低会直接放大「打开大图要等整页缩略图」：
	//   飞牛一次发出整页（~50 张）list 请求，它的 size=big（大图预览）排在队尾，
	//   并发 1 时 50 x 210ms = 10.5s，big 请求要等这么久才轮到 —— 等到时
	//   暂停机制才生效，而两页早已生成完。并发 4 时是 2.6s，快 4 倍。
	// 所以默认给 4，并把上限交给用户（1~maxThumbConcurrency）。
	n := 4
	if n > runtime.NumCPU() {
		n = runtime.NumCPU()
	}
	if n > maxThumbConcurrency {
		n = maxThumbConcurrency
	}
	return n
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
		ThumbEngine:         "auto",
		ThumbLowres:         true,
		ViewerQuality:       0,    // 自动（88）
		ViewerMaxdim:        2048, // 1.8.146 起默认 2048（用户要求）
		ViewerLowres:        0,    // 自动（按公式）
		ThumbLowresLevel:    0,    // 自动（按公式）
		TakeoverSystemThumb: false,
		CPUCores:            0, // 0 = 不限制
		ThumbConcurrency:    thumbConcurrencyDefault(),
		PreloadConcurrency:  2, // 默认开 2 个后台 worker：把"等待"提前到空闲时段（0 = 关闭）
		SchemaVersion:       currentSettingsSchema,
		GPUDecode:           true,
		ViewerPreload:       2,
		ViewerAnimation:     "slide",
		ViewerMode:          "window",
		// 默认跟随文件管理器排序：这才是不用配置就正确的那一项。
		// 关掉它 = 回到「本应用自己一套顺序」，只有用户明确不想要跟随时才该用。
	}
}

// boolPtr 取 bool 的地址。Settings 里几个「默认开」的开关用指针区分
// 「配置里没这一项」与「用户关掉了」，需要一个取址辅助函数。
func boolPtr(v bool) *bool { return &v }

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
	// P0-1 治本：从默认值出发再反序列化，缺失字段保留默认值 ——
	// 这样以后**每一个新加的 bool 字段**都不会再因为"零值 = false"而静默失效
	// （1.8.136 的 ThumbLowres 就是这么丢的：normalize 里没有它，bool 缺失即 false，
	//   还会被 migrate/save 落盘固化）。
	//
	// ★ SchemaVersion 必须在 Unmarshal **之前**清零：
	//   defaultSettings() 会把它设成 currentSettingsSchema，而"磁盘上从来没写过
	//   schemaVersion 的老配置"Unmarshal 后不会覆盖它 → migrated 判为 false →
	//   migrate 整段跳过、迁移失效。清零后：JSON 里有就覆盖、没有就保持 0 → 迁移照常跑。
	s := defaultSettings()
	s.SchemaVersion = 0
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
	// 先留原始值再 normalize：normalize 会做交叉钳制（Preload ≤ Thumb−1），
	// 迁移判据「是否仍等于旧默认值」必须看磁盘上的原值，见 migrate 注释。
	rawThumb, rawPreload := s.ThumbConcurrency, s.PreloadConcurrency
	s.normalize()
	migrated := s.SchemaVersion < currentSettingsSchema
	s.migrate(rawThumb, rawPreload)
	settingsMu.Lock()
	settings = s
	settingsMu.Unlock()
	if migrated {
		// 迁移结果必须落盘：否则 SchemaVersion 永远停在 0，每次启动都重判一遍，
		// 用户手动把值设回 6/3 会被反复改掉。
		saveSettingsFile()
	}
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

// currentSettingsSchema 当前配置结构版本。需要「把旧默认值升到新默认值」时 +1。
//
//	v1：1.8.66 —— ThumbConcurrency 6→3、PreloadConcurrency 3→0，
//	    但判据是「两个值同时等于旧默认组合」，太窄。
//	v2：1.8.67 —— 两个旧默认值各自独立判断。v1 的窄判据会漏掉
//	    「改过并发数、没碰过预生成」的配置（旧版预生成默认 3，
//	    只要用户调过并发数就不再等于 6，于是预生成一直是 3 —— 表现就是「关不掉」）。
const currentSettingsSchema = 4

// migrate 把旧默认值升到新默认值。
//
// rawThumb / rawPreload 必须是**磁盘上的原始值**，要在 normalize() 之前取：
// normalize 的交叉钳制（Preload ≤ Thumb−1）会先把 3 改成 2，
// 那样下面「是否仍等于旧默认值」的判断就落空了。
//
//	ThumbConcurrency    旧默认 6 → 3
//	PreloadConcurrency  旧默认 3 → 0（0 = 关闭）
//
// 两个值各自独立判断：只要仍等于旧默认值就升级。用户主动改过的非默认值
// （例如把预生成调到 5）不会被覆盖。迁移后写入 SchemaVersion，所以只发生一次 ——
// 之后用户手动把值设回 6 或 3 不会被反复改掉。
func (s *Settings) migrate(rawThumb, _ int) { // 第二个参数（rawPreload）已无规则使用
	if s.SchemaVersion >= currentSettingsSchema {
		return
	}
	if rawThumb == 6 {
		// 6 是旧版本的固定默认值 → 迁移到「按核数收敛」的新默认，
		// 而不是硬编码 3（那样存量用户永远拿不到 thumbConcurrencyDefault 的结果）。
		s.ThumbConcurrency = thumbConcurrencyDefault()
	}
	// 【已删除】schema 3 曾有一条「rawPreload == 3 || == 0 → 新默认」的迁移，
	// 于 1.8.139 按用户要求移除：0 是合法的「关闭」值，而这条规则没有版本守卫，
	// 每次 schema 升级都会重跑一次，等于用户永远关不掉后台预生成。
	// 现在磁盘上是几就是几（旧默认 3 会被 normalize 钳到 ThumbConcurrency-1）。
	// 1.8.139：原先这里有一条「rawPreload == 3 || == 0 → 新默认」的迁移，
	// 已按需求删除。原因：0 是合法的「关闭」值，而这条规则没有版本守卫，
	// 每次 schema 升级都会重跑一次，等于用户永远关不掉后台预生成。
	// 删除后行为：磁盘上是几就是几（旧默认 3 会被 normalize 钳到 ThumbConcurrency-1）。
	// schema 4：ThumbLowres 是 1.8.136 新加的 bool 字段。
	// 存量配置里没有它 → 反序列化成零值 false → -lowres 静默失效，
	// 而且会被 migrate/save 落盘固化（用户此后打开设置保存还会再写一次 false）。
	// 该字段在 1.8.136 之前不可能被用户显式设置过，直接给新默认。
	//
	// 这一条专门救"已经升到 1.8.136、配置里已被写成 thumbLowres:false"的情况
	// （他们 schemaVersion 已是 3，光靠治本路径不够 —— Unmarshal 会用磁盘上的 false 覆盖默认值）。
	if s.SchemaVersion < 4 {
		s.ThumbLowres = defaultSettings().ThumbLowres
	}
	s.SchemaVersion = currentSettingsSchema
}

// normalize 把越界值收敛到合法范围
func (s *Settings) normalize() {
	// ThumbEngine 非法值一律回落到 auto（空值也走这里 —— 老配置没有这个字段）
	// 1.8.144：新增档位字段的白名单收敛
	switch s.ViewerQuality {
	// 只保留实测有意义的档：84→-q:v 4、88/92→3、95/98→2（后两者各自等价，去掉重复项）
	case -1, 0, 84, 88, 95:
	default:
		s.ViewerQuality = 0
	}
	switch s.ViewerMaxdim {
	case 0, 2048, 3072, 4096, 5120, 6144:
	default:
		s.ViewerMaxdim = 0
	}
	if s.ViewerLowres < -1 || s.ViewerLowres > 3 {
		s.ViewerLowres = 0
	}
	if s.ThumbLowresLevel < -1 || s.ThumbLowresLevel > 3 {
		s.ThumbLowresLevel = 0
	}

	switch s.ThumbEngine {
	case "vips", "ffmpeg", "auto":
	default:
		s.ThumbEngine = "auto"
	}
	if s.ThumbSize < 64 || s.ThumbSize > 640 {
		s.ThumbSize = 320
	}
	if s.ThumbQuality < 50 || s.ThumbQuality > 95 {
		s.ThumbQuality = 80
	}
	if s.CPUCores < 0 {
		s.CPUCores = 0
	}
	if s.ThumbConcurrency < 1 || s.ThumbConcurrency > maxThumbConcurrency {
		// 用 thumbConcurrencyDefault()（按核数收敛）而不是硬编码 3：
		// 否则「越界回落」与「新装默认」给出不同答案，存量用户永远拿不到按核数的值。
		s.ThumbConcurrency = thumbConcurrencyDefault()
	}
	// 0 = 关闭后台预生成，是合法值（不启动 worker、也不主动投递）。
	if s.PreloadConcurrency < 0 || s.PreloadConcurrency > 8 {
		s.PreloadConcurrency = 0
	}
	// 交叉钳制：后台实际并发最多 ThumbConcurrency−1（留 1 个槽给前台），
	// 超过部分是死配置（worker 抢不到槽，每 200ms 纯轮询），直接钳到有效值。
	// PreloadConcurrency=0（关闭）不参与钳制：0 > maxBg 恒不成立，保持 0。
	// 绝不能把 0 当越界值补成 1 —— 那会让用户设的「关闭」自己变回开启。
	maxBg := s.ThumbConcurrency - 1
	if maxBg < 1 {
		maxBg = 1
	}
	if s.PreloadConcurrency > maxBg {
		s.PreloadConcurrency = maxBg
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
	// 没写过这一项（老配置升级上来）补成默认值「开」。
	// 补成具体值而不是留 nil：/api/settings 的响应里就会是一个真实 bool，
	// 用户在前端看到的开关状态与磁盘上的内容一致，排查时不会被 null 绕。
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
	// 缩略图根目录必须是**真实目录**：若路径中任何一段是链接，后面的
	// MkdirAll / Chmod / RemoveAll 全部会跟着链到别处 —— 用户只要把自己 ThumbDir 下的
	// .mediaview-thumbs 换成指向 /etc 的链接，再点一次「清空缓存」，
	// root 进程就会把 /etc 权限改成 0644（丢掉 +x），把系统弄坏。
	//
	// 只 Lstat 最后一段是不够的，两个盲区：
	//   ① 中间段是链接时，Lstat(整条路径) 会跟随中间段，最后一段看起来仍是普通目录；
	//   ② Windows 目录联接（junction）连 Lstat 都不标记为符号链接（ModeSymlink=false）。
	// 所以统一走 resolveRealPath 逐段解析：
	//   - 解析不出来（成环 / Readlink 失败）→ 回退，绝不放行；
	//   - 解析结果仍落在允许范围内（例如链接指向另一个存储空间）→ 采纳解析后的路径；
	//   - 解析结果跑到允许范围之外 → 回退到默认目录，并把原因告诉用户。
	if real, ok := resolveRealPath(root); !ok {
		note = "缩略图缓存目录无法解析（存在循环链接），已回退到默认目录"
		root = filepath.Join(workDir, thumbSubdir)
	} else if !samePath(real, root) {
		// 注意这里必须用 pathAllowedStrict（独立根），不能用 pathAllowed：
		// 后者把 ThumbDir 自己也当根 → 校验缩略图目录时成了自我引用。
		if pathAllowedStrict(real) {
			root = real
		} else {
			note = "缩略图缓存目录是链接且指向允许范围之外，已回退到默认目录"
			root = filepath.Join(workDir, thumbSubdir)
		}
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
		// 1.8.148：附属文件都跳过。.key 已不再产生（旧缓存可能还有），
		// .meta.json 也已停止落盘 —— 但旧缓存里存在，若不算进去，
		// 界面上「已缓存 N 张」会虚高近一倍（实测 19356 vs 真实 9664）。
		if strings.HasSuffix(d.Name(), thumbKeySuffix) || strings.HasSuffix(d.Name(), ".meta.json") {
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
	roots := independentRoots()
	if t := currentThumbRoot(); t != "" {
		roots = append(roots, t)
	}
	// 配置里声明过但暂时不可用的目录也要放行，否则用户无法在界面里纠正它
	if s := getSettings(); s.ThumbDir != "" {
		if d, ok := cleanThumbDir(s.ThumbDir, false); ok {
			roots = append(roots, d)
		}
	}
	return roots
}

// independentRoots 返回**与用户配置无关**的白名单根：存储空间、系统注入的可访问路径、
// 以及本应用自己的数据目录。
//
// 为什么要单独拆出来：allowedRoots() 会把 settings.ThumbDir / currentThumbRoot()
// 也算成根，而这正是「用户可配置、可被替换成链接」的值。拿它去校验缩略图根目录
// 就是自我引用 —— 把 ThumbDir 设成指向 /etc 的链接，解析后的结果恰好落在
// 「刚刚被写进根列表的那个位置」之下，校验永远通过。凡是「被校验的值有可能来自
// 用户配置」的场景，都必须用独立根。
func independentRoots() []string {
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
	for _, p := range []string{workDir, appDest, settingsDir()} {
		if p != "" {
			roots = append(roots, p)
		}
	}
	return roots
}

// pathAllowedStrict 用独立根做范围校验（不含用户配置派生出来的根）。
// 根目录自身同样解析一次链接（某些 NAS 上 /vol1 就是挂载点/链接）。
func pathAllowedStrict(abs string) bool {
	for _, r := range independentRoots() {
		if r == "" {
			continue
		}
		if underRoot(abs, r) {
			return true
		}
		if real, ok := resolveRealPath(r); ok && !samePath(real, r) && underRoot(abs, real) {
			return true
		}
	}
	return false
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
	// 前端 body 不含 schemaVersion，保存时补上当前版本；
	// 否则保存一次就把它清回 0，下次启动又会触发一次迁移。
	in.SchemaVersion = currentSettingsSchema

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
