package main

// 文件夹封面（2×2 缩略图拼图）
// ============================================================
// 目的：网格里的文件夹不再只有 📁，而是「随机抽 4 张图拼成的封面」——
// 本层有图就从本层抽；不够就按随机顺序下探子目录补齐（对齐飞牛相册的观感）。
//
// 三个设计决定，都是被"成本"逼出来的，改之前先读一遍：
//
//  ① **服务端合成一张图**，而不是把 4 个路径丢给前端去拉 4 张缩略图。
//     理由：前端的并发纪律（GRID_THUMB_INFLIGHT=4、大图打开时挂起、换目录丢弃待办）
//     是按「一个格子一根 img」设计的；一个文件夹吃 4 个槽位会把其它格子饿死。
//     合成之后，文件夹格子在数据面上就是**一根普通 img**，上面那些机制全部白捡。
//
//  ② **缝（gap）烘进图里，颜色固定（coverSeamColor，白稍暗 #f0f0f0）**。
//     理由：让前端用 CSS 画十字缝的话，前端必须先知道"这次是几张图"（1/2/3/4 张
//     的排版不一样）——而它在拿到图之前并不知道，那就要多打一次 JSON 请求。
//     把缝烘进图里、颜色写死，一个请求搞定，而且**与主题无关**：
//     缝色既不参与缓存键，也不随深/浅主题切换（2.4.26 起，用户拍板的固定观感）。
//
//  ③ **随机一次就钉住**（用户选的是"永不重抽"）。
//     所以缓存有效性的判据是「目录内容有没有变」，不是时间：
//        · 抽取时走过的每个目录，mtime 必须没变；
//        · 选中的那几张文件必须还在。
//     任何一条不满足就重抽。为省掉这条校验里 36 次 stat 的大头，
//     结论缓存了一个很短的 TTL（coverValidTTL），目录刚变时封面最多晚这几秒刷新。
//
// 成本实测（bench_folder_cover.go，本机 SSD）：
//     ReadDir 1000 项 0.92ms / 带预算递归 400 项 6.9ms / 合成 320×320 14ms
//   ⇒ "找图"是毫秒级，瓶颈只在"4 张缩略图冷生成"（≈100~150ms/张），
//     所以挑图时优先挑**已经缓存过缩略图**的文件（lookupThumbCache 零成本命中）。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

const (
	// coverMaxItems 一个封面最多几张图（2×2）。
	coverMaxItems = 4
	// coverMaxDepth 下探的最大层数。0 = 只看本层，1 = 本层 + 一层子目录……
	//   4 层（2.4.27 起，用户指定）：覆盖"年 → 月 → 日 → 相册"这种组织方式；
	//   枚举成本随层数增长，但有条目/时间双预算兜底，深树走到预算就收手。
	coverMaxDepth = 4
	// coverWalkBudget 一次挑选最多枚举多少个目录条目（**不是** syscall 次数）。
	//   首页那种聚合目录（"电脑"下有几十万文件）没有这个上限就会把磁盘打满。
	coverWalkBudget = 800
	// coverTimeBudget 单次挑选的时间上限：到点就用已有结果，宁可封面凑不齐 4 张。
	coverTimeBudget = 120 * time.Millisecond
	// coverCacheProbe 挑图时最多探测多少个候选的"缩略图是否已缓存"。
	//   lookupThumbCache 每个候选要 2 次 stat，不能对几百个候选都做。
	coverCacheProbe = 24
	// coverGroupProbe 按目录分组补齐时，**每个子目录**最多探测几个候选。
	//   比 coverCacheProbe 小得多：一个封面最多 4 个目录、每个目录几张图就够挑，
	//   否则"目录多 × 每目录 24 次 stat"会让挑图本身变成开销。
	coverGroupProbe = 4
	// coverMaxChecks 缓存签名里最多记多少个目录的 mtime（防止深目录把 meta 撑大）。
	//   2.4.28 从 32 收到 16：meta 默认挂在 jpg 的 xattr 上，单值上限 4032B。
	//   本机实测（4 图 + 全中文深路径 /vol1/1000/我的照片/2024年/家庭旅行/日本东京/…）：
	//     8 条 1678B / 16 条 2652B / 24 条 3628B / 32 条 4604B ✗
	//   32 条会顶破上限 → 退回 .json sidecar（用户当初明确不想要的）。
	//   16 条既能覆盖"根 + 走过的 15 个目录"的变化检测，又留了 1.3KB 余量。
	coverMaxChecks = 16
	// coverCacheVersion 缓存格式版本。改了挑选/合成逻辑就 +1，旧缓存自动作废。
	//   v2（2.4.15）：① 挑选规则改成"多子文件夹分散取图"，旧封面是平铺随机抽的；
	//                 ② 合成加了"一块都没画上就报错"，早期可能落过纯色空封面。
	//                 两者都必须让旧缓存失效，否则用户升级后看到的还是老封面/空块。
	//   v3（2.4.24）：缝色从「前端传 bg」改成「自动取图集平均色」，缓存键不再含主题。
	//   v4（2.4.25）：缝色改回「前端传 bg」（平均色观感差），但 bg 仍不进缓存键。
	//   v5（2.4.26）：缝色改「固定色 coverSeamColor」（白稍暗），彻底与主题/请求参数无关。
	//   v6（2.4.27）：① meta 加 Truncated 标记（撞预算的下探不落盘"空"结论）；
	//                 ② 空结论也要过 Checks 校验（旧版直接判有效 = 大目录随机抽空
	//                    一次就被永久固化成"无封面"，加图也不刷新）；③ 下探深度 3→4。
	//                 旧缓存的"空"meta（v≤5）必须全部作废重抽，否则固化 bug 治不了。
	//   v7（2.4.28）：下探走过的**子目录**也写进 Checks（此前只记根目录 ⇒ 往子目录
	//                 里加/删照片，封面永远不刷新）。旧 meta 的 Checks 缺这些目录，
	//                 必须作废重挑一次，新结论才带得全。
	coverCacheVersion = 7
	// coverDefaultSize 前端没给 size 时的边长。
	coverDefaultSize = 320
	// coverMinSize / coverMaxSize 允许的边长（与 /api/thumb 的 64~640 对齐）。
	coverMinSize = 192
	coverMaxSize = 640
	// coverJPEGQuality 封面合成后的画质。与缩略图画质**解耦**：
	//   缩略图画质可调（50~95），若也跟进缓存键，用户调一次画质就要重合成所有封面。
	coverJPEGQuality = 86
)

// coverSkipDirs 这些目录名一律不下探、也不参与挑选。
//
//	· 隐藏目录：.xxx（含 .mediaview-thumbs）
//	· 群晖/威联通/Windows 的系统目录：里面全是缩略图或回收站，抽出来又糊又奇怪
var coverSkipDirs = map[string]bool{
	"@eaDir":                    true, // Synology 缩略图目录
	"#recycle":                  true, // Synology 回收站
	"$RECYCLE.BIN":              true,
	"System Volume Information": true,
	".mediaview-thumbs":         true, // 本应用自己的缩略图目录（若被放在媒体目录里）
}

// coverSem 封面合成的并发上限。
//
// 为什么单独一个信号量、且只有 2：缩略图那边有 thumbGenSem（默认核数−2），
// 首页一屏几十个文件夹若都去抢缩略图槽位，用户点开大图时就没槽了。
// 封面是"锦上添花"，永远排在缩略图之后。
var coverSem = make(chan struct{}, 2)

func acquireCoverSlot() func() {
	coverSem <- struct{}{}
	return func() { <-coverSem }
}

// coverItem 选中的一张图（写进缓存 meta，也用于失效校验）。
type coverItem struct {
	Path  string `json:"path"`
	Mtime int64  `json:"mtime"` // 毫秒，与列表接口同口径
	Kind  string `json:"kind"`  // image | video
}

// coverCheck 一个"抽取时走过"的目录及其 mtime。
// 用于判断"目录内容有没有变"（增删文件会更新父目录 mtime）。
type coverCheck struct {
	Path  string `json:"path"`
	Mtime int64  `json:"mtime"`
}

// coverPick 一次挑选的完整结果，也是缓存 meta 的结构。
type coverPick struct {
	V      int    `json:"v"`      // 缓存格式版本
	Dir    string `json:"dir"`    // 被挑选的目录
	PickAt int64  `json:"pickAt"` // 挑选时刻（毫秒），用于 ETag
	Source string `json:"source"` // direct（本层抽到）| deep（下探过）| none（一张都没有）
	Depth  int    `json:"depth"`  // 实际下探到第几层
	Walked int    `json:"walked"` // 枚举了多少条目（诊断 + 测试断言）
	// Truncated 这次下探**没有走完**（条目预算/时间预算耗尽时队列里还有目录没展开）。
	// 置位时挑选结果不完整 —— 若连一张图都没抽到，这个"空"绝不能落盘/判有效，
	// 否则大目录会随机抽空一次后被永久固化成"无封面"（2.4.26 真机踩到）。
	Truncated bool         `json:"truncated,omitempty"`
	Items     []coverItem  `json:"items"`
	Checks    []coverCheck `json:"checks"`
}

// ---------- 挑选 ----------

// coverRandSeed 只给测试用：非 0 时挑选结果完全可复现。
var coverRandSeed int64

// errCoverNoTiles 一块都没画上（缩略图全部不可用）。调用方应把它当成"暂时没有封面"，
// 不要落盘、不要缓存 —— 否则空色块会被永久固化。
var errCoverNoTiles = errors.New("cover: no tile rendered")

// 封面元数据的两种"写不进 xattr"的原因，都只用于内部判定，不会冒泡到 HTTP 层。
var (
	errCoverMetaTooLarge     = errors.New("cover meta: too large for xattr")
	errCoverXattrUnsupported = errors.New("cover meta: xattr unsupported")
)

// coverCacheDirName 封面缓存子目录名（与尺寸档目录平级）。
//
// 单独抽成常量是因为它现在有两个用途：拼路径，以及 purgeCache 的白名单判断
// （之前写死字面量，结果"清空缓存"把非数字目录一律跳过，封面永远清不掉）。
const coverCacheDirName = "dircov"

// coverMetaXattrLimit 单条 xattr 的实际可用上限。
//
// ext4/btrfs 的 user.* 单值上限是 4KB，还要减去属性名（user.mediaview.dircov = 21B），
// 所以留 64B 余量取 4032。超过就写不进 —— 这时退回 .json sidecar，
// 而不是硬写（硬写会返回 ERANGE，等于这次元数据彻底丢了，封面下次还得重挑）。
const coverMetaXattrLimit = 4032

// 元数据读写信封。
//
// 做成变量是为了可测：本机开发是 Windows，平台实现走 no-op 分支，
// 只能验证"回退写 .json"那一半；测试注入内存实现后才能覆盖
// "写进 xattr 后目录里确实没有 .json""超限时降级""老 .json 自动迁移"这三条正路。
var (
	coverMetaSet = setCoverMetaXattr
	coverMetaGet = getCoverMetaXattr
)

// coverJPGOf 由元数据路径推出封面图路径（两者只差扩展名）。
func coverJPGOf(metaPath string) string {
	return strings.TrimSuffix(metaPath, ".json") + ".jpg"
}

// readCoverPickAny 读取封面元数据（**不校验有效性**）：先读 xattr，再读 .json。
//
// 两个入口合成一个，是因为"读元数据"现在有三种用途：
// 判缓存有效性（loadCoverPick）、按目录清封面（purgeDirCover）。
// 两处的读写优先级必须完全一致，分开写迟早会漂。
func readCoverPickAny(jpgPath, metaPath string) (*coverPick, []byte) {
	var raw []byte
	if b, ok := coverMetaGet(jpgPath); ok {
		raw = b
	} else if b, err := os.ReadFile(metaPath); err == nil {
		raw = b
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var p coverPick
	if json.Unmarshal(raw, &p) != nil {
		return nil, nil
	}
	return &p, raw
}

// coverSkipRootsFor 算出「本次遍历要避开的内部目录」。
//
// 为什么不能直接用全局那几个根：`workDir` 是整个应用的工作目录（飞牛上是应用数据目录），
// 只要用户的媒体目录**自己就在它里面**，那么每一个子目录都"在 workDir 之下"，
// 整棵树会被跳过 —— 表现是封面永远出不来（本机冒烟实测踩到：workDir=应用目录，
// 用户在应用目录下放了测试图库，结果 dirs=0）。
//
// 正确语义：只有当遍历从**这些根的外面**走进来时，才需要避开它们；
// 扫描根本身就在根里，说明用户就是指着这里要看，这时这些根对本次遍历没有意义。
func coverSkipRootsFor(scanRoots ...string) []string {
	var out []string
	for _, root := range []string{currentThumbRoot(), workDir, appDest} {
		if root == "" {
			continue
		}
		inside := false
		for _, sr := range scanRoots {
			if sr == "" {
				continue
			}
			if samePath(sr, root) || underRoot(sr, root) {
				inside = true
				break
			}
		}
		if inside {
			continue // 任何一个扫描根就在里面 ⇒ 不能拿它做跳过依据
		}
		out = append(out, root)
	}
	return out
}

// shouldSkipCoverDir 判断某个子目录要不要跳过。
//
// full 是绝对路径：除了按名字跳过系统目录，还要跳过**缩略图根目录/运行期目录**——
// 用户完全可能把缩略图目录设成「图库/我的缩略图」，那样封面就会抽到缩略图的缩略图
// （越抽越糊），甚至自己喂自己。
//
// skipRoots 由 coverSkipRootsFor 按本次扫描根算好传进来（不要在这里重新算，
// 否则每个子目录都要重新判断一次"根是不是在根里"，既慢又容易把语义写错）。
func shouldSkipCoverDir(name, full string, skipRoots []string) bool {
	if name == "" || strings.HasPrefix(name, ".") || coverSkipDirs[name] {
		return true
	}
	for _, root := range skipRoots {
		if samePath(full, root) || underRoot(full, root) {
			return true
		}
	}
	return false
}

// pickCover 挑出最多 need 张图。
//
// 顺序规则（与用户确认过的语义一致）：
//  1. 本层有图片 → 本层的图**先算数**（随机取，最多 need 张）；
//  2. 不够 need → 按随机顺序 BFS 下探子目录，用找到的图片补齐；
//  3. 整棵树一张图片都没有 → 才退而用视频帧（"图片优先，无图用视频帧"）。
func pickCover(dir string, need int) coverPick {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	if coverRandSeed != 0 {
		rng = rand.New(rand.NewSource(coverRandSeed))
	}
	return pickCoverWithRand(dir, need, rng)
}

func pickCoverWithRand(dir string, need int, rng *rand.Rand) coverPick {
	deadline := time.Now().Add(coverTimeBudget)
	res := coverPick{V: coverCacheVersion, Dir: dir, Items: []coverItem{}, Source: "none"}
	budget := coverWalkBudget

	// ---- ① 本层 ----
	ents, err := os.ReadDir(dir)
	if err != nil {
		return res
	}
	res.noteCheck(dir)
	res.Walked += len(ents)
	budget -= len(ents)

	// 跳过依据按**本次扫描根**算一次（见 coverSkipRootsFor 的说明）
	skipRoots := coverSkipRootsFor(dir)
	var directImg, directVid, subs []string
	for _, e := range ents {
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if !shouldSkipCoverDir(e.Name(), full, skipRoots) {
				subs = append(subs, full)
			}
			continue
		}
		switch classify(filepath.Ext(e.Name())) {
		case "image":
			directImg = append(directImg, full)
		case "video":
			directVid = append(directVid, full)
		}
	}

	// ---- ② 本层图片优先（够 need 张时一次 ReadDir 就结束，这是最常见的路径）----
	rng.Shuffle(len(directImg), func(i, j int) { directImg[i], directImg[j] = directImg[j], directImg[i] })
	var chosen []string
	chosen = append(chosen, directImg[:minInt(len(directImg), need)]...)

	// ---- ③ 本层不足 → 子目录**按目录分散**补齐 ----
	//   用户要求：目录里套很多子文件夹时，「随机选四个文件夹、每个取一张」。
	//   所以这里不是把子目录里的图平铺后再随机抽（那样 4 张可能全来自同一个子目录），
	//   而是**每个子目录轮流取一张**（round-robin）：第一轮每目录各 1 张保证多样性，
	//   第二轮/第三轮再补第 2、3 张——目录少时也能凑满，不至于只出 1~2 张。
	usedDeep := false
	if len(chosen) < need && len(subs) > 0 {
		if budget <= 0 {
			// 根目录自己的 ReadDir 就把预算吃穿了（第一层级大目录正是如此）：
			// 下探根本没开始，这个结论也是不完整的。
			res.Truncated = true
		} else {
			groups, depth, truncated := collectCoverGroups(subs, budget, deadline, rng, &res.Walked, &res)
			res.Depth = depth
			res.Truncated = truncated
			// 图片优先：本层有图 或 任一子目录有图 ⇒ 只用图片；整棵子树一张图都没有才用视频帧
			hasImg := len(chosen) > 0
			if !hasImg {
				for _, g := range groups {
					if len(g.imgs) > 0 {
						hasImg = true
						break
					}
				}
			}
			// 每组的候选列表：按 hasImg 选图片/视频，先随机再「优先已缓存」
			cands := make([][]string, 0, len(groups))
			for _, g := range groups {
				list := g.vids
				if hasImg {
					list = g.imgs
				}
				if len(list) == 0 {
					continue
				}
				list = shuffled(list, rng)
				list = preferCachedThumbs(list, coverGroupProbe)
				cands = append(cands, list)
			}
			// round-robin：第一轮每目录各 1 张（分散），第二轮各第 2 张……直到凑齐 need
			for round := 0; len(chosen) < need; round++ {
				progressed := false
				for _, list := range cands {
					if len(chosen) >= need {
						break
					}
					if round < len(list) {
						chosen = append(chosen, list[round])
						progressed = true
						usedDeep = true
					}
				}
				if !progressed {
					break
				}
			}
		}
	}

	// ---- ④ 整棵树一张图片都没有 → 退用本层视频帧 ----
	if len(chosen) == 0 && len(directVid) > 0 {
		directVid = shuffled(directVid, rng)
		chosen = append(chosen, directVid[:minInt(len(directVid), need)]...)
	}

	// ---- ⑤ 落成 coverItem（这时才 stat：只 stat 真正要用的那几张）----
	for _, p := range chosen {
		if len(res.Items) >= need {
			break
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue // 挑完之后被删了：跳过，后面还有候选
		}
		kind := classify(filepath.Ext(p))
		if kind == "" {
			continue
		}
		res.Items = append(res.Items, coverItem{Path: p, Mtime: st.ModTime().UnixMilli(), Kind: kind})
	}
	if len(res.Items) > 0 {
		res.Source = "direct"
		if usedDeep {
			res.Source = "deep"
		}
	}
	return res
}

// coverGroup 一个「有媒体文件」的目录及其图片/视频。
//
// 按目录分组是为了实现「多个子文件夹时每个目录各取一张」（用户要求）——
// 平铺成一个大列表再随机抽，4 张可能全来自同一个子文件夹，封面就看不出聚合感。
type coverGroup struct {
	dir  string
	imgs []string
	vids []string
}

// collectCoverGroups 从 subs 出发按随机顺序 BFS，**按目录分组**收集图片/视频。
// 返回的 depth 是"实际展开了几层"（0 = 一个子目录都没展开，1 = 展开了第一层……）。
// 返回的 truncated 表示预算耗尽时队列里**还有目录没展开**——此时收集结果不完整，
// 调用方必须据此拒绝把"空"结论当真（见 coverPick.Truncated）。
//
// rec 非 nil 时，**每个真正读过盘的目录都会记进 rec.Checks**（2.4.28 修）：
// 之前只记了根目录，于是"往子目录里加/删照片"（父目录 mtime 不变）永远不会让封面
// 失效 —— 表现是封面陈旧到清缓存为止。文件头的设计说明一直是"走过的每个目录都要查"，
// 这条 2.4.14 起就没落实（真机只表现为"封面怎么不更新"，很难归因）。
func collectCoverGroups(subs []string, budget int, deadline time.Time, rng *rand.Rand, walked *int, rec *coverPick) (groups []coverGroup, depth int, truncated bool) {
	// 扫描根 = 这些子目录的共同父级（调用方就是它的 ReadDir 产生了 subs）
	scanRoot := ""
	if len(subs) > 0 {
		scanRoot = filepath.Dir(subs[0])
	}
	skipRoots := coverSkipRootsFor(scanRoot)
	type node struct {
		path  string
		level int
	}
	queue := make([]node, 0, len(subs))
	for _, s := range subs {
		queue = append(queue, node{s, 1})
	}
	// 随机顺序：否则"电脑"这种目录的封面永远是第一个子目录里的图。
	rng.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })

	for len(queue) > 0 {
		if budget <= 0 || time.Now().After(deadline) {
			// 队列非空却被预算打断 ⇒ 这次下探没走完，结果不完整。
			truncated = true
			break
		}
		cur := queue[0]
		queue = queue[1:]
		if cur.level > coverMaxDepth {
			continue
		}
		ents, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}
		budget -= len(ents)
		*walked += len(ents)
		if cur.level > depth {
			depth = cur.level
		}
		// 读过的目录都要进校验（见函数头说明）：否则子目录内容变了封面也不会重抽。
		// noteCheck 自带 coverMaxChecks 上限，深树不会把 meta 撑大。
		if rec != nil {
			rec.noteCheck(cur.path)
		}
		g := coverGroup{dir: cur.path}
		var next []node
		for _, e := range ents {
			full := filepath.Join(cur.path, e.Name())
			if e.IsDir() {
				if cur.level < coverMaxDepth && !shouldSkipCoverDir(e.Name(), full, skipRoots) {
					next = append(next, node{full, cur.level + 1})
				}
				continue
			}
			switch classify(filepath.Ext(e.Name())) {
			case "image":
				g.imgs = append(g.imgs, full)
			case "video":
				g.vids = append(g.vids, full)
			}
		}
		if len(g.imgs) > 0 || len(g.vids) > 0 {
			groups = append(groups, g)
		}
		rng.Shuffle(len(next), func(i, j int) { next[i], next[j] = next[j], next[i] })
		queue = append(queue, next...)
		// 第一轮每个有图目录只贡献 1 张：need 个目录就够封面；留一倍余量给「优先已缓存」挑。
		// 这条 break 不算 truncated —— 已经拿到够用的图，收手是"够了"不是"没走完"。
		if len(groups) >= coverMaxItems*2 {
			break
		}
	}
	return groups, depth, truncated
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// preferCachedThumbs 把"缩略图已在缓存里"的候选挪到前面（只探测前 probe 个）。
func preferCachedThumbs(pool []string, probe int) []string {
	if len(pool) == 0 {
		return pool
	}
	if probe > len(pool) {
		probe = len(pool)
	}
	size := thumbRequestSize()
	cached := make([]string, 0, probe)
	rest := make([]string, 0, len(pool))
	for i, p := range pool {
		if i < probe && lookupThumbCache(p, size) != "" {
			cached = append(cached, p)
		} else {
			rest = append(rest, p)
		}
	}
	return append(cached, rest...)
}

// noteCheck 记下"走过的目录"的 mtime（用于"内容没变就复用封面"）。
func (p *coverPick) noteCheck(dir string) {
	if len(p.Checks) >= coverMaxChecks {
		return
	}
	st, err := os.Stat(dir)
	if err != nil {
		return
	}
	p.Checks = append(p.Checks, coverCheck{Path: dir, Mtime: st.ModTime().UnixMilli()})
}

func shuffled(in []string, rng *rand.Rand) []string {
	out := append([]string{}, in...)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// ---------- 缓存 ----------

// coverCacheBase 返回某目录某尺寸的缓存文件名前缀（不含扩展名）。
//
// 键里只含目录路径：缝色虽然随主题（前端传 bg），但**不参与缓存键**（2.4.25 起），
// 切换深/浅主题不重合成、也不多存一整套封面（已生成的封面沿用生成时那版缝色）。
func coverCacheBase(dir string, size int) string {
	h := sha1.Sum([]byte(dir))
	key := hex.EncodeToString(h[:])[:16]
	root := currentThumbRoot()
	if root == "" {
		return "" // 没有缩略图目录 = 不落盘（仍然能当场合成）
	}
	return filepath.Join(root, coverCacheDirName, key[:2], key+"_"+strconv.Itoa(size))
}

// coverValidTTL 封面「有效性校验」结论的缓存时长。
//
// 这条校验要做 36 次 stat（32 个被检查目录 + 4 张选中图），是命中路径的大头
// （本机实测 ~1.8ms，真机机械盘更贵）。把结论缓存一个很短的 TTL：
// 目录刚被修改时封面最多晚这几秒刷新，换来反复浏览目录时校验几乎零开销。
// refresh=1 的强制重抽走的是「根本不调 loadCoverPick」那条路，天然绕过这里。
const coverValidTTL = 8 * time.Second

// coverValidCache 缓存 coverStillValid 的结论（key = jpgPath）。
// 每目录一条、按 TTL 惰性失效重算，不会无限增长。
var coverValidCache sync.Map

type coverValidEntry struct {
	valid bool
	at    time.Time
}

// coverStillValid 判断缓存 meta 是否还能用：走过的目录 mtime 没变、选中的文件还在。
//
// "空目录"结论同样要走 Checks 校验（2.4.27 起，不再提前判有效）：
// 旧版对空结论直接返回 true，大目录撞预算被随机抽空一次后，这个"空"会被
// 永久固化 —— 往目录里加了新照片封面也不出来，只有清缓存能救。
// 真空目录的 mtime 稳定，走 Checks 照样判有效，不会重复白扫。
func coverStillValid(p *coverPick, dir string) bool {
	if p == nil || p.V != coverCacheVersion || p.Dir != dir {
		return false
	}
	for _, c := range p.Checks {
		st, err := os.Stat(c.Path)
		if err != nil || !st.IsDir() || st.ModTime().UnixMilli() != c.Mtime {
			return false
		}
	}
	for _, it := range p.Items {
		st, err := os.Stat(it.Path)
		if err != nil || st.IsDir() {
			return false
		}
		// 2.4.50（二次审查 P2-11）：**必须比较 mtime**。
		//
		// 原来这里只校验"文件还在、不是目录"：原地覆盖选中的那张图（用修图软件另存、
		// 用同名新图替换）时，文件名与目录项数都不变 ⇒ 目录 mtime 也不变 ⇒
		// 上面那段 Checks 全部通过 ⇒ 封面永久陈旧，只能手动"重建封面"。
		// 字段 Items[].Mtime 从 2.4.2x 起就已经在写了，只是从来没人比较 —— 纯漏实现。
		//
		// 兼容：老 meta 可能缺这个字段（Mtime==0），此时**跳过**比较，
		// 否则升级后所有存量封面会被判失效、全量重抽一次（封面重抽要读 4 张缩略图，
		// 十万目录级别是一次明显的 IO 峰值）。
		if it.Mtime != 0 && st.ModTime().UnixMilli() != it.Mtime {
			return false
		}
	}
	return true
}

// coverStillValidCached 是 coverStillValid 的带 TTL 包装（见 coverValidTTL 说明）。
func coverStillValidCached(p *coverPick, dir, key string) bool {
	if v, ok := coverValidCache.Load(key); ok {
		if e, ok := v.(coverValidEntry); ok && time.Since(e.at) < coverValidTTL {
			return e.valid
		}
	}
	valid := coverStillValid(p, dir)
	coverValidCache.Store(key, coverValidEntry{valid, time.Now()})
	return valid
}

func loadCoverPick(metaPath, dir string) *coverPick {
	if metaPath == "" {
		return nil
	}
	jpgPath := coverJPGOf(metaPath)
	p, _ := readCoverPickAny(jpgPath, metaPath)
	if p == nil {
		return nil
	}
	if !coverStillValidCached(p, dir, jpgPath) {
		return nil
	}
	return p
}

func saveCoverPick(metaPath string, p *coverPick) {
	if metaPath == "" {
		return
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	jpgPath := coverJPGOf(metaPath)

	// 2.4.51（封面重复生成根因修复）：写入端一有封面产物落盘，就作废旧的有效性结论。
	//
	// coverStillValidCached 会把"无效"结论按 jpgPath 在 TTL(8s) 内缓存；而旧版
	// saveCoverPick 写完新 meta 却不清它 —— 于是"一键生成 → 立刻进目录"这条链
	// 会命中 8 秒内那条 stale=false，把刚生成的封面判死、重新合成一份。等 8 秒
	// TTL 到期又恢复正常，正是用户描述的"仅刚生成后短时间重做"时序现象。
	//
	// 放在函数出口统一删：xattr 成功分支与 .json 回退分支都会覆盖；即使写入
	// 失败，删掉也只会让下次重新校验（此刻磁盘上本就是旧 meta / 无 meta，
	// 重算结论更准确），无害。key 与 loadCoverPick→coverStillValidCached 完全一致
	// （都取 coverJPGOf(metaPath)），不会出现"清错 key"。
	coverValidCache.Delete(jpgPath)

	// 首选：写进封面图自己的扩展属性 —— 目录里就只剩一张 jpg
	// （用户诉求原话："封面上会生成一张 .json，能取消掉吗"）。
	// 前提是 jpg 已落盘，所以 buildCover 特意把这次保存挪到了写图之后。
	if len(raw) <= coverMetaXattrLimit && fileExists(jpgPath) {
		if err := coverMetaSet(jpgPath, raw); err == nil {
			_ = os.Remove(metaPath) // 上一版留下的 sidecar，顺手清掉
			return
		}
	}
	// 回退（三条路都在这里）：
	//   ① 文件系统不支持 xattr（网络盘 / exFAT / 非 Linux）；
	//   ② 元数据超过 xattr 单值上限（深目录 + 32 个检查目录）；
	//   ③ 空目录 —— 没有 jpg 可挂。
	// 回退时功能不降级，只是目录里多一个 .json。
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		return
	}
	// 原子落盘：先写临时文件再 rename（与缩略图产物同一套做法）。
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, metaPath); err != nil {
		_ = os.Remove(tmp)
	}
}

// ---------- 合成 ----------

// coverComposed 是 singleflight 的返回值：挑选结果 + 合成产物。
type coverComposed struct {
	pick *coverPick
	data []byte
}

// coverSeamColor 封面拼缝的固定底色：白，稍微暗一点（#f0f0f0）。
//
// 2.4.26 用户拍板：缝色不再跟主题（2.4.25 曾由前端传 --bg-cell），固定成这个值后
// 缝色与主题、请求参数彻底无关 —— 缓存键只含目录路径 + 尺寸，任何时刻命中同一张封面。
func coverSeamColor() color.RGBA { return color.RGBA{240, 240, 240, 255} }

// composeCover 把最多 4 张图按"实际张数"排版拼成 side×side 的 JPEG。
//
//	n=1 铺满 | n=2 左右分栏 | n=3 左大 + 右上下 | n>=4 2×2
//
// 缝色固定为 coverSeamColor（见其说明）。
func composeCover(items []coverItem, side int) ([]byte, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("no items")
	}
	// 封面合成独占 2 个槽位（见 coverSem 说明）：绝不与缩略图抢主槽位。
	defer acquireCoverSlot()()
	// 缩略图统一用网格档（默认 320）：这样"用户浏览过的目录"封面直接命中已有缓存，
	// 不用另生成一套 180 档。多出来的像素在合成时用 CatmullRom 缩掉。
	tileSrc := thumbRequestSize()
	srcs := make([]image.Image, 0, len(items))
	for _, it := range items {
		thumbPath, err := ensureThumb(it.Path, it.Kind, tileSrc)
		if err != nil {
			srcs = append(srcs, nil) // 单张失败留底色，不毁整个封面
			continue
		}
		f, err := os.Open(thumbPath)
		if err != nil {
			srcs = append(srcs, nil)
			continue
		}
		src, _, derr := image.Decode(f)
		_ = f.Close()
		if derr != nil {
			srcs = append(srcs, nil)
			continue
		}
		srcs = append(srcs, src)
	}
	return composeFromImages(srcs, side)
}

// composeFromImages 把最多 4 张**已解码**的图按"实际张数"排版拼成 side×side 的 JPEG。
//
// 纯函数、不碰 IO，专为可测拆出来（composeCover 负责 ensureThumb + decode 后调它）。
// srcs[i] 允许为 nil（该张失败/解码失败），对应那块留底色（缝色）。
//
// 缝色固定为 coverSeamColor（2.4.26 起），在图块之间留缝并铺满画布底。
func composeFromImages(srcs []image.Image, side int) ([]byte, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("no items")
	}
	if side < coverMinSize {
		side = coverMinSize
	}
	if side > coverMaxSize {
		side = coverMaxSize
	}
	canvas := image.NewRGBA(image.Rect(0, 0, side, side))
	gap := side / 90 // 360 → 4px，180 → 2px
	if gap < 2 {
		gap = 2
	}
	if gap > 8 {
		gap = 8
	}
	seam := coverSeamColor()
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			canvas.Set(x, y, seam)
		}
	}

	half := side / 2
	g := gap
	var rects []image.Rectangle
	switch len(srcs) {
	case 1:
		rects = []image.Rectangle{image.Rect(0, 0, side, side)}
	case 2:
		rects = []image.Rectangle{
			image.Rect(0, 0, half-g/2, side),
			image.Rect(half+g/2+g%2, 0, side, side),
		}
	case 3:
		rects = []image.Rectangle{
			image.Rect(0, 0, half-g/2, side),
			image.Rect(half+g/2+g%2, 0, side, half-g/2),
			image.Rect(half+g/2+g%2, half+g/2+g%2, side, side),
		}
	default:
		rightX := half + g/2 + g%2
		bottomY := half + g/2 + g%2
		rects = []image.Rectangle{
			image.Rect(0, 0, half-g/2, half-g/2),
			image.Rect(rightX, 0, side, half-g/2),
			image.Rect(0, bottomY, half-g/2, side),
			image.Rect(rightX, bottomY, side, side),
		}
	}

	drawn := 0
	for i, src := range srcs {
		if i >= len(rects) {
			break
		}
		if src == nil {
			continue // 该张失败：留底色（其余块照常画，不至于毁掉整张封面）
		}
		drawCoverFit(canvas, src, rects[i])
		drawn++
	}
	// 一块都没画上 ⇒ 这张图只是一块底色。绝不能当封面发出去、更不能缓存：
	// 用户看到的是个空色块（比 📁 更糟），而且一旦落盘就会一直这样。
	// 真实原因通常是缩略图管线暂时不可用（ffmpeg 缺失 / 缩略图目录不可写），
	// 这时应该回退到 📁，并在下次访问时重试。
	if drawn == 0 {
		return nil, errCoverNoTiles
	}

	var out byteBuffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: coverJPEGQuality}); err != nil {
		return nil, err
	}
	return out.b, nil
}

// drawCoverFit 把 src 以 object-fit:cover 的方式画进 dst 的 r 区域（居中裁切后缩放）。
func drawCoverFit(dst *image.RGBA, src image.Image, r image.Rectangle) {
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	// 从源图中心裁一块与目标区域同比例的区域
	cropW, cropH := sw, sh
	if sw*r.Dy() > sh*r.Dx() {
		cropW = sh * r.Dx() / r.Dy()
	} else {
		cropH = sw * r.Dy() / r.Dx()
	}
	if cropW <= 0 {
		cropW = 1
	}
	if cropH <= 0 {
		cropH = 1
	}
	cr := image.Rect(0, 0, cropW, cropH).
		Add(sb.Min).
		Add(image.Pt((sw-cropW)/2, (sh-cropH)/2))
	draw.CatmullRom.Scale(dst, r, src, cr, draw.Src, nil)
}

type byteBuffer struct{ b []byte }

func (w *byteBuffer) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

// ---------- HTTP ----------

// handleDirCover 提供文件夹封面。
//
//	GET /api/dircov?path=<目录>&size=<192..640>&refresh=0|1
//
// 返回 image/jpeg（side×side）。目录里一张图都没有时返回 404 + JSON，
// 前端据此保留 📁 图标（并记住本次会话不再重试）。
func handleDirCover(w http.ResponseWriter, r *http.Request) {
	if !getSettings().ThumbEnabled || !getSettings().CoverEnabled {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-MediaView-Thumb", "disabled")
		http.Error(w, "cover disabled", http.StatusForbidden)
		return
	}
	touchThumbActivity() // 用户正在看网格：后台预生成让路（与 /api/thumb 同款）

	q := r.URL.Query()
	// 聚合根 "/" 不是真实目录，没有封面可言（它的子项各自有封面）
	raw := strings.TrimSpace(q.Get("path"))
	if raw == "" || raw == "/" {
		writeCoverError(w, "bad_path", "path is required")
		return
	}
	dir, sc := resolveMediaPath(raw)
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		writeCoverError(w, "not_dir", "path is not a directory")
		return
	}

	size := coverDefaultSize
	if s, err := strconv.Atoi(q.Get("size")); err == nil && s >= coverMinSize && s <= coverMaxSize {
		size = s
	}
	refresh := q.Get("refresh") == "1"

	base := coverCacheBase(dir, size)
	jpgPath, metaPath := "", ""
	if base != "" {
		jpgPath, metaPath = base+".jpg", base+".json"
	}
	// 强制重抽后要把「有效性结论」的 TTL 缓存也作废（2.4.28 修）：
	// 否则这一轮刚写好的新 meta 会在下一次普通请求里被 8 秒前的旧结论判死
	// （旧结论很可能是 false），于是又白抽一遍图 —— refresh 就白点了。
	if refresh && jpgPath != "" {
		coverValidCache.Delete(jpgPath)
	}

	// ---- 缓存命中：meta 有效 + 产物在 → 直接发 ----
	var pick *coverPick
	if !refresh {
		pick = loadCoverPick(metaPath, dir)
	}
	if pick != nil && len(pick.Items) > 0 && jpgPath != "" {
		if fi, err := os.Stat(jpgPath); err == nil && fi.Size() > 0 {
			serveCoverFile(w, r, jpgPath, pick, dir, size)
			return
		}
	}

	// ---- 未命中：singleflight 里挑图 + 合成（同一目录并发请求只算一次）----
	sfKey := "cover:" + dir + "|" + strconv.Itoa(size)
	if refresh {
		// 强制重抽：让本次的 key 唯一，避免与正在跑的旧请求合并
		sfKey += fmt.Sprintf("|r%d", time.Now().UnixNano())
	}
	v, err, _ := sfGroup.Do(sfKey, func() (any, error) {
		p, data, berr := buildCover(dir, size, jpgPath, metaPath, refresh)
		if berr != nil {
			return nil, berr
		}
		if len(p.Items) == 0 {
			return p, nil // 空目录：不发图片，让前端画 📁
		}
		return coverComposed{pick: p, data: data}, nil
	})
	if err != nil {
		// 缩略图全都不可用 ⇒ 这次只是「暂时没有封面」，不是服务端故障：
		// 用和空目录相同的 404 JSON，前端看到就画 📁，下次访问自然重试。
		// 用 500 会让这变成"错误"，既污染日志，也会让前端把它当成可重试失败反复打。
		if errors.Is(err, errCoverNoTiles) {
			writeCoverError(w, "no_tiles", "cover tiles unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "cover generation failed", http.StatusInternalServerError)
		return
	}

	switch out := v.(type) {
	case *coverPick:
		// 空目录
		writeCoverError(w, "empty", "no images in this folder")
	case coverComposed:
		// 不落盘时（没有缩略图目录）直接发内存里的产物
		if jpgPath == "" || !fileExists(jpgPath) {
			writeCoverImage(w, r, out.pick, dir, size, out.data)
			return
		}
		serveCoverFile(w, r, jpgPath, out.pick, dir, size)
	default:
		writeCoverError(w, "empty", "no images in this folder")
	}
}

// buildCover 挑图 + 合成 + 落盘。handler 在 singleflight 里调它；
// 「一键生成」也直接调它（同一套逻辑，避免两处实现漂移）。
//
// 返回 (挑选结果, 内存产物, 错误)。内存产物只在「没有缩略图目录、无处落盘」时有值。
// refresh=true 时强制重抽（忽略已有 meta）——「清缓存后重建」与「换一批」用得上。
// 缝色固定为 coverSeamColor，与请求参数无关。
func buildCover(dir string, size int, jpgPath, metaPath string, refresh bool) (*coverPick, []byte, error) {
	var p *coverPick
	// needSave：这次是不是**新挑**的。原实现挑完立刻 saveCoverPick，
	// 但元数据现在挂在 jpg 的 xattr 上 —— 那时图还没写，必然只能回退成 .json。
	// 所以改成"挑完先拿在手里，等图落盘再存"。
	needSave := false
	if !refresh {
		p = loadCoverPick(metaPath, dir)
	}
	if p == nil {
		fresh := pickCover(dir, coverMaxItems)
		fresh.PickAt = time.Now().UnixMilli()
		p = &fresh
		needSave = true
	}
	if len(p.Items) == 0 {
		// 空目录：不发图片，让前端画 📁。
		// 但"空"这个结论只有在下探**走完了**时才可信，才落盘（否则每次进空目录
		// 都得重新遍历一遍）；Truncated（预算/超时打断）的空只是"这次没抽到"，
		// 落盘就会把大目录随机抽空一次的结果永久固化 —— 宁可下次重抽。
		if needSave && !p.Truncated {
			saveCoverPick(metaPath, p)
		}
		return p, nil, nil
	}
	// 已有产物（比如 jpg 被单独删了）就直接复用挑选结果重合成
	data, cerr := composeCover(p.Items, size)
	if cerr != nil {
		// 合成失败：把可能的旧产物删掉。否则它会被继续当成"已生成"端出去/跳过，
		// 而这次的失败原因（缩略图暂时不可用）根本没机会重试。
		if jpgPath != "" && errors.Is(cerr, errCoverNoTiles) {
			_ = os.Remove(jpgPath)
		}
		return nil, nil, cerr
	}
	if jpgPath != "" {
		if err := os.MkdirAll(filepath.Dir(jpgPath), 0o755); err == nil {
			tmp := jpgPath + ".tmp"
			if err := os.WriteFile(tmp, data, 0o644); err == nil {
				if err := os.Rename(tmp, jpgPath); err != nil {
					_ = os.Remove(tmp)
				}
			}
		}
	}
	// ★ 必须写在图片落盘之后：xattr 挂在 jpg 上，顺序反了就只能回退成 .json。
	if needSave {
		saveCoverPick(metaPath, p)
	}
	return p, data, nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}

// coverETag 让浏览器能 304，而不是每次都重下一张图。
//
//	含 pickAt：重抽之后必然换 etag；含 size：换尺寸也是另一张图。
func coverETag(p *coverPick, dir string, size int) string {
	return fmt.Sprintf(`"cv%d-%s-%d-%d"`, coverCacheVersion,
		shortHash(dir), size, p.PickAt)
}

func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])[:12]
}

func serveCoverFile(w http.ResponseWriter, r *http.Request, jpgPath string, p *coverPick, dir string, size int) {
	fi, err := os.Stat(jpgPath)
	if err != nil {
		writeCoverError(w, "empty", "cover file missing")
		return
	}
	etag := coverETag(p, dir, size)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=600")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-MediaView-Cover", p.Source)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f, err := os.Open(jpgPath)
	if err != nil {
		writeCoverError(w, "empty", "cover file unreadable")
		return
	}
	defer f.Close()
	http.ServeContent(w, r, "cover.jpg", fi.ModTime(), f)
}

func writeCoverImage(w http.ResponseWriter, r *http.Request, p *coverPick, dir string, size int, data []byte) {
	etag := coverETag(p, dir, size)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=600")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-MediaView-Cover", p.Source)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// writeCoverError 统一的"没有封面"响应。
// 用 JSON 而不是 1×1 占位图：前端要能区分"没图"（画 📁、本次会话别再问）
// 与"生成失败"（可以重试），占位图做不到这个区分。
func writeCoverError(w http.ResponseWriter, reason, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     false,
		"reason": reason,
		"error":  msg,
	})
}

// purgeDirCover 删除某个媒体目录对应的封面缓存（jpg + 元数据一起删），返回删除张数。
//
// 「清缓存（指定目录）」时调用；「清全部」由 purgeCache 直接删掉整个 dircov 目录。
// 为什么不按 hash 精确算 key：封面键由 sha1(dir) 精确可算，但元数据现在默认存在
// jpg 的 xattr 里，直接算 key 后还要读 xattr 确认 Dir —— 不如遍历读 Dir 匹配来得直白。
// 封面量少（每目录一张），遍历成本可忽略。
//
// 2.4.23 起元数据默认存在 jpg 的 xattr 里、目录里看不到 .json，
// 所以这里改成**遍历 jpg 与 .json 两种**、按主名去重：只认 .json 会让
// "元数据已在 xattr 里"的封面永远删不掉（这是改完不测就会漏的一条）。
func purgeDirCover(root, dir string) int {
	base := filepath.Join(root, coverCacheDirName)
	shards, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	n := 0
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		sd := filepath.Join(base, shard.Name())
		files, err := os.ReadDir(sd)
		if err != nil {
			continue
		}
		seen := make(map[string]bool, len(files))
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			ext := filepath.Ext(f.Name())
			if ext != ".jpg" && ext != ".json" {
				continue
			}
			stem := strings.TrimSuffix(f.Name(), ext)
			if seen[stem] {
				continue
			}
			seen[stem] = true
			jpg := filepath.Join(sd, stem+".jpg")
			meta := filepath.Join(sd, stem+".json")
			p, _ := readCoverPickAny(jpg, meta)
			if p == nil || p.Dir != dir {
				continue
			}
			_ = os.Remove(jpg)
			_ = os.Remove(meta)
			n++
		}
	}
	return n
}

// handleRebuildDirCover 重建**单个目录**的封面（立即强制重抽 + 重合成）。
//
//	POST /api/dircov/rebuild?path=<目录>&size=<192..640>&bg=<rrggbb>
//
// 用途：用户看到某个文件夹封面是"坏图"（缩略图源坏了 / 内容变了却还没刷新）时，
// 点工具栏「重建」按钮就地重做这一个封面。**只碰这一个目录**，不递归、不碰下层的
// 文件夹/图片，也不清缩略图缓存 —— 与 /api/thumb/clear 的递归清理是两回事。
func handleRebuildDirCover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !getSettings().ThumbEnabled || !getSettings().CoverEnabled {
		http.Error(w, "cover disabled", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("path"))
	if raw == "" || raw == "/" {
		writeCoverError(w, "bad_path", "path is required")
		return
	}
	dir, sc := resolveMediaPath(raw)
	if sc != 0 {
		http.Error(w, http.StatusText(sc), sc)
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		writeCoverError(w, "not_dir", "path is not a directory")
		return
	}
	size := coverDefaultSize
	if s, err := strconv.Atoi(q.Get("size")); err == nil && s >= coverMinSize && s <= coverMaxSize {
		size = s
	}

	base := coverCacheBase(dir, size)
	jpgPath, metaPath := "", ""
	if base != "" {
		jpgPath, metaPath = base+".jpg", base+".json"
	}
	// 清掉有效性结论的 TTL 缓存，别让旧结论把刚重建的结果又"判死"。
	if jpgPath != "" {
		coverValidCache.Delete(jpgPath)
	}
	// refresh=true：无视旧 meta 强制重抽；同一套 buildCover，合成失败也能删掉旧产物。
	p, _, berr := buildCover(dir, size, jpgPath, metaPath, true)
	if berr != nil {
		if errors.Is(berr, errCoverNoTiles) {
			writeCoverError(w, "no_tiles", "cover tiles unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "cover generation failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{"ok": true, "source": p.Source, "items": len(p.Items)})
}

// ---------- 一键生成（批量预生成文件夹封面）----------
//
// 为什么需要它：按需生成是"进目录才现算"，首次浏览一个没缓存的大目录树时，
// 一屏几十个文件夹要逐个挑图+合成，用户会看到封面一张张慢慢出现。
// 「一键生成」把这件事**提前批量做完并落盘**，之后浏览直接命中缓存（也顺便消除"闪"）。
//
// 与按需生成的产物**完全同一套缓存**（同 buildCover、同 dircov/ 目录、同 meta），
// 所以它是幂等的：已生成的目录直接跳过。
//
// 参数必须和前端 coverURL 一致（size），否则缓存键不同、白生成一遍：
//   · size = 前端 cfg.thumbSize
//   （缝色自动取图集平均色，不再参与缓存键，故无需对齐主题。）

// coverPregenMaxDirs 单次「一键生成」最多处理多少个目录。
//
// ⚠️ 这个数字**不是**产品意义上的"只支持这么多文件夹"（2.4.45 之前它被当成硬上限，
// 而且前端没传 max，于是不管目录树多大，一律只做前 3000 个，界面上还显示"已完成"）。
// 它现在的角色是**安全阀**：防止病态目录树（几十万目录）把目录清单撑爆内存。
//
// 命中时状态里 truncated=true，前端会明确提示"目录可能没走完，再点一次继续"——
// 重复点安全：已生成的目录直接跳过（幂等）。
const coverPregenMaxDirs = 200000

type coverPregenState struct {
	mu        sync.Mutex
	running   bool
	gen       int // 运行代号：区分"当前任务"与"还在退出路上的旧任务"
	cancel    chan struct{}
	total     int
	done      int
	generated int
	skipped   int
	failed    int
	truncated bool // 目录清单撞到安全阀，实际还有目录没排进来
	current   string
	started   int64
	finished  int64
}

var coverPregen = &coverPregenState{}

type coverPregenView struct {
	Running   bool   `json:"running"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Generated int    `json:"generated"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
	Truncated bool   `json:"truncated,omitempty"`
	Current   string `json:"current"`
	Started   int64  `json:"started"`
	Finished  int64  `json:"finished"`
}

func (s *coverPregenState) snapshot() coverPregenView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return coverPregenView{
		Running:   s.running,
		Total:     s.total,
		Done:      s.done,
		Generated: s.generated,
		Skipped:   s.skipped,
		Failed:    s.failed,
		Truncated: s.truncated,
		Current:   s.current,
		Started:   s.started,
		Finished:  s.finished,
	}
}

func (s *coverPregenState) start(startDirs []string, size, max int) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("已经在一键生成中")
	}
	s.running = true
	// 运行代号：见 update() 的说明。被取消的旧任务可能还在退出路上，
	// 必须让它认出"我已经不是当前任务了"，否则它的收尾会把新任务的状态清掉。
	s.gen++
	gen := s.gen
	s.cancel = make(chan struct{})
	s.total, s.done, s.generated, s.skipped, s.failed = 0, 0, 0, 0, 0
	s.truncated = false
	s.current = ""
	s.started = time.Now().UnixMilli()
	s.finished = 0
	cancel := s.cancel
	s.mu.Unlock()

	go runCoverPregen(startDirs, size, max, cancel, gen)
	return nil
}

// update 只在「当前运行仍是本次」时才执行 fn。
//
// 为什么必须有它：取消是**协作式**的，被取消的 goroutine 要走到下一个检查点才退出，
// 这段时间里它仍会执行到循环尾的计数与 defer 收尾。若不加代号守卫，
// 「取消后立刻重新开始」就会出现上一轮把新一轮的 running/finished 置成已完成
// —— 实测表现是进度条瞬间归零、用户以为任务没启动（真机冒烟抓到的）。
func (s *coverPregenState) update(gen int, fn func()) {
	s.mu.Lock()
	if s.gen == gen {
		fn()
	}
	s.mu.Unlock()
}

func (s *coverPregenState) cancelRun() {
	s.mu.Lock()
	if s.cancel != nil {
		close(s.cancel)
		s.cancel = nil
	}
	if s.running {
		// 立刻释放运行位：用户点了取消就该能马上重新开始，不必等旧 goroutine 走到检查点。
		// gen++ 让旧任务从这一刻起无权再改状态（否则它退出时会把新任务标记成已完成）。
		s.running = false
		s.finished = time.Now().UnixMilli()
		s.gen++
	}
	s.mu.Unlock()
}

// handleCoverPregen 一键生成封面。
//
//	POST /api/dircov/pregen?path=&size=&max=   启动
//	GET  /api/dircov/pregen                        查进度
//	POST /api/dircov/pregen/cancel                 取消
func handleCoverPregen(w http.ResponseWriter, r *http.Request) {
	if !getSettings().ThumbEnabled || !getSettings().CoverEnabled {
		http.Error(w, "cover disabled", http.StatusForbidden)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		coverPregen.cancelRun()
		writeJSON(w, r, map[string]any{"ok": true})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, coverPregen.snapshot())
	case http.MethodPost:
		q := r.URL.Query()
		raw := strings.TrimSpace(q.Get("path"))
		startDirs, sc := pregenStartDirs(raw)
		if sc != 0 {
			http.Error(w, http.StatusText(sc), sc)
			return
		}
		size := coverDefaultSize
		if s, err := strconv.Atoi(q.Get("size")); err == nil && s >= coverMinSize && s <= coverMaxSize {
			size = s
		}
		max := coverPregenMaxDirs
		// 允许前端下调，但**不允许超过安全阀**：两处数字一旦各写一份，
		// 迟早出现"前端传 5 万、后端只认 2 万"这种静默截断（2.4.45 之前的 3000 就是这么来的）。
		if m, err := strconv.Atoi(q.Get("max")); err == nil && m > 0 && m <= coverPregenMaxDirs {
			max = m
		}
		if err := coverPregen.start(startDirs, size, max); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, r, map[string]any{"ok": true, "startDirs": startDirs, "size": size})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// coverPregenAlreadyDone 判断某目录的封面「已经生成好了、可以跳过」。
//
// 这是「一键生成可以反复点」的唯一依据，所以两个条件**缺一不可**：
//   - 产物存在：没有 jpg 就是没生成（可能上次合成失败）；
//   - 选图元数据有效：只有产物没有 meta（或 meta 因目录变化失效）时，
//     无法知道这批封面是从哪几张图拼的，也不能确定它和当前 size 一致
//     ⇒ 必须重做，否则会一直端出旧封面。
func coverPregenAlreadyDone(dir, jpgPath, metaPath string) bool {
	if jpgPath == "" || !fileExists(jpgPath) {
		return false
	}
	p := loadCoverPick(metaPath, dir)
	return p != nil && len(p.Items) > 0
}

// pregenCount 把一次 buildCover 的结果归成「生成 / 跳过 / 失败」三档（三者互斥）。
//
// 抽成纯函数是为了可测：这里曾经把"撞预算没抽到图"算成 generated，
// 进度条与磁盘对不上，光看代码很容易再写回去。
//
//	· 有错          ⇒ failed（缩略图不可用 / 目录不可读）
//	· 无错但没 Items ⇒ skipped（目录里这轮确实没有可用媒体，下次访问会再试）
//	· 其它          ⇒ generated
func pregenCount(p *coverPick, err error) (gen, skip, fail int) {
	if err != nil {
		return 0, 0, 1
	}
	if p == nil || len(p.Items) == 0 {
		return 0, 1, 0
	}
	return 1, 0, 0
}

func runCoverPregen(startDirs []string, size, max int, cancel chan struct{}, gen int) {
	defer func() {
		// 只有**当前任务**才有权收尾：被取消的旧任务退出时不得改写新一轮的状态
		coverPregen.update(gen, func() {
			coverPregen.running = false
			coverPregen.finished = time.Now().UnixMilli()
			coverPregen.current = ""
		})
	}()

	// ① 先收集"有媒体的目录"（先知道 total，进度条才有百分比）
	dirs, truncated := collectDirsWithMedia(startDirs, max, cancel)
	coverPregen.update(gen, func() {
		coverPregen.total = len(dirs)
		coverPregen.truncated = truncated
	})

	// ② 逐个生成（已缓存的直接跳过 → 幂等，重复点不会重做）
	for _, d := range dirs {
		select {
		case <-cancel:
			return
		default:
		}
		base := coverCacheBase(d, size)
		jpgPath, metaPath := "", ""
		if base != "" {
			jpgPath, metaPath = base+".jpg", base+".json"
		}
		if coverPregenAlreadyDone(d, jpgPath, metaPath) {
			coverPregen.update(gen, func() {
				coverPregen.done++
				coverPregen.skipped++
			})
			continue
		}
		coverPregen.update(gen, func() { coverPregen.current = d })
		p, _, err := buildCover(d, size, jpgPath, metaPath, false)
		// 归类必须看**有没有真的产出**（2.4.28 修）：buildCover 对"撞预算一张没抽到"
		// 是返回 nil 错（只是这次没抽到，不落盘、下次重试），旧代码一律算 generated++
		// ⇒ 进度条显示"已生成 N"，磁盘上一个封面都没有，用户以为生成完了。
		genN, skipN, failN := pregenCount(p, err)
		coverPregen.update(gen, func() {
			coverPregen.done++
			coverPregen.generated += genN
			coverPregen.skipped += skipN
			coverPregen.failed += failN
		})
	}
}

// pregenStartDirs 推导「一键生成」的起点集合。
//
//   - 指定了具体目录：就是它（且必须通过可见范围校验）；
//   - 首页 / 空 path：**业务上的根** —— 开启「只显示已添加的文件夹」时是那几个文件夹，
//     否则是各存储空间的用户目录（/vol{n}/1000，与首页聚合层完全一致）。
//
// 绝不能退回字面量 "/"：那会把 /usr、/proc 一起走一遍。这条在 NAS 上是实打实的
// 长时间磁盘风暴，在开发机上则会把整个 C 盘扫掉。
func pregenStartDirs(raw string) ([]string, int) {
	if raw != "" && raw != "/" {
		dir, sc := resolveMediaPath(raw)
		if sc != 0 {
			return nil, sc
		}
		return []string{dir}, 0
	}
	if s := getSettings(); s.OnlyRoots {
		out := make([]string, 0, len(s.Roots))
		for _, r := range s.Roots {
			if r != "" && dirExists(r) {
				out = append(out, r)
			}
		}
		return out, 0
	}
	var out []string
	for _, vol := range listVolDirs() {
		home, err := defaultUserDir(vol)
		if err != nil || !dirExists(home) {
			continue
		}
		out = append(out, home)
	}
	return out, 0
}

// collectDirsWithMedia 广度优先收集"含媒体文件的目录"（最多 max 个）。
// 只收目录、不收文件；跳过系统/隐藏/缩略图目录（与挑图同一套跳过规则）。
//
// 第二个返回值 truncated：**撞到 max 时队列里还有目录没展开**，也就是"这棵树下还有
// 含媒体的目录没排进来"。这个信号必须一路传到前端 —— 2.4.45 之前 max 固定 3000、
// 截断又是静默的，界面显示"已完成：新生成 N 个"，实际只做完了前 3000 个目录，
// 用户完全看不出来（这就是"一键生成最多只有 3000 个"的来源）。
//
// 2.4.50（二次审查 P2-13）：max 从"只约束有媒体的目录数"扩展为**同时约束已展开的
// 目录数**。原来只在 `hasMedia` 时检查 `len(out)>=max`，于是**海量空目录**
// （几十万层子目录但没有一张图片）时队列与 seen 会一路增长直到走完整棵树：
// 注释里宣称"防止病态目录树撑爆内存"的安全阀形同虚设，长时间遍历 + seen 暴涨
// 是真的可以 OOM 的。现在展开数一到 max 就带着 truncated=true 返回，
// 前端会提示"再点一次继续"（见 coverPregenDoneText）。
//
// startDirs 是**多个**起点：首页（聚合根）一键生成时对应"每个存储空间用户目录"，
// 开启「只显示已添加的文件夹」时对应那几个文件夹。
// 早期版本在聚合根传的是字面量 "/"，那会把整个文件系统（/usr、/proc…）走一遍 ——
// 在 NAS 上既慢又没意义，这条路径必须由调用方给出**业务上的根**。
func collectDirsWithMedia(startDirs []string, max int, cancel chan struct{}) ([]string, bool) {
	out := make([]string, 0, 64)
	skipRoots := coverSkipRootsFor(startDirs...)
	queue := make([]string, 0, len(startDirs))
	seen := make(map[string]bool, len(startDirs))
	for _, d := range startDirs {
		if d == "" {
			continue
		}
		key := pathKey(d)
		if seen[key] {
			continue
		}
		seen[key] = true
		queue = append(queue, d)
	}
	// expanded = 已经 ReadDir 过的目录数（含起点）。它是 max 的第二重约束，
	// 保证"空目录树"也被安全阀挡住（见函数注释）。
	expanded := 0
	for len(queue) > 0 {
		select {
		case <-cancel:
			return out, false
		default:
		}
		// 开一个新目录之前就判：超限 ⇒ 队列里剩下的都没展开，必须报 truncated。
		// 用 `>`（不是 `>=`）是为了不误伤"恰好收满最后一个"的场景 ——
		// 那时 len(out)==max 且队列为空，属于完整走完，不该报截断。
		if max > 0 && expanded > max {
			return out, true
		}
		cur := queue[0]
		queue = queue[1:]
		expanded++
		ents, err := os.ReadDir(cur)
		if err != nil {
			continue
		}
		hasMedia := false
		for i, e := range ents {
			// 超大目录（几万条目）里 ReadDir 已返回、但下面的循环仍要跑一会儿：
			// 每 64 条检查一次取消，让"取消一键生成"能及时生效。
			if i%64 == 0 {
				select {
				case <-cancel:
					return out, false
				default:
				}
			}
			if e.IsDir() {
				full := filepath.Join(cur, e.Name())
				if !shouldSkipCoverDir(e.Name(), full, skipRoots) {
					key := pathKey(full)
					if !seen[key] {
						seen[key] = true
						queue = append(queue, full)
					}
				}
				continue
			}
			if classify(filepath.Ext(e.Name())) != "" {
				hasMedia = true
			}
		}
		if hasMedia {
			out = append(out, cur)
			if len(out) >= max {
				// 队列里还剩节点 ⇒ 一定有目录没被展开（可能没有媒体，但对调用方而言
				// 只能保守报"没走完"；宁可提示多，也不能让用户以为已经全做完）。
				return out, len(queue) > 0
			}
		}
	}
	return out, false
}
