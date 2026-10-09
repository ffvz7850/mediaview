package main

import (
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// media extension sets
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
	".bmp": true, ".tif": true, ".tiff": true, ".heic": true, ".heif": true,
	".avif": true, ".jfif": true, ".svg": true, ".jpe": true,
}
var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".avi": true, ".webm": true,
	".m4v": true, ".flv": true, ".wmv": true, ".ts": true, ".mts": true,
	".m2ts": true, ".mpg": true, ".mpeg": true, ".3gp": true, ".3g2": true,
	".rmvb": true, ".vob": true, ".f4v": true, ".rm": true,
}

type FileItem struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	// Btime 创建时间（毫秒）。只在本次请求按创建时间排序时才去 statx 取
	// —— 每张图多一次系统调用，不排序时白花。
	Btime    int64   `json:"btime,omitempty"`
	Kind     string  `json:"kind"` // image | video
	Ext      string  `json:"ext"`
	W        int     `json:"w,omitempty"`
	H        int     `json:"h,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Btime int64  `json:"btime,omitempty"`
}

// sortKey 投影出排序需要的字段，让目录与文件共用同一套比较规则
// （飞牛文件管理器里目录也是按当前列排序的，只是恒在文件前面）。
func (f FileItem) sortKey() sortKey {
	return sortKey{Name: f.Name, Size: f.Size, Mtime: f.Mtime, Btime: f.Btime, Ext: f.Ext}
}

func (d DirEntry) sortKey() sortKey {
	// 目录没有扩展名：按「类型」排序时所有目录同类，落到文件名兜底
	return sortKey{Name: d.Name, Size: d.Size, Mtime: d.Mtime, Btime: d.Btime}
}

type ListResponse struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent"`
	Dirs   []DirEntry `json:"dirs"`
	Files  []FileItem `json:"files"`
	Total  int        `json:"total"`
	// Sort / SortDesc 回传本次**实际生效**的排序（字段名 + 是否降序）。
	// 前端传来的值可能带别名或无法识别，回传生效值后，页面能据此同步排序控件、
	// 排查时也不用靠猜。
	Sort     string `json:"sort"`
	SortDesc bool   `json:"sortDesc"`
}

// safePath normalizes user-supplied path and rejects traversal/empty.
func safePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", false
	}
	// after Clean, any ".." segments are resolved; reject if it escapes (it can't from abs)
	return p, true
}

func classify(ext string) string {
	ext = strings.ToLower(ext)
	if imageExts[ext] {
		return "image"
	}
	if videoExts[ext] {
		return "video"
	}
	return ""
}

// imageDim reads only the header to obtain dimensions (fast, no full decode).
func imageDim(p string) (int, int) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func listDir(p, sortBy string) (*ListResponse, error) {
	// 排序规格在遍历前就解析好：只有按创建时间排序时才需要为每个条目多取一次
	// statx（每张图一次系统调用，几百张的目录是实打实的开销），别的排序一律不取。
	sp := parseSort(sortBy)
	needBtime := sp.field == sortFieldBtime

	// 用 Readdir 而不是 os.ReadDir：后者保证**按文件名排序**，而飞牛文件管理器在
	// 主键相等的字段下（「类型」全相等、同一秒的「修改时间」）显示的是**目录物理顺序**
	// —— 实测 `ls -f` 与它完全一致（DSC00809, DSC00746, DSC00560, DSC00855, DSC00702）。
	// 两边必须用同一个原始顺序，相等项的先后才能对上。
	df, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	entries, err := df.Readdir(-1)
	_ = df.Close()
	if err != nil {
		return nil, err
	}
	resp := &ListResponse{Path: p, Dirs: []DirEntry{}, Files: []FileItem{}}
	parent := filepath.Dir(p)
	if parent != p {
		resp.Parent = parent
	}

	for _, e := range entries {
		full := filepath.Join(p, e.Name())
		if e.IsDir() {
			// skip hidden/system dirs
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			// 目录也带上大小/时间：文件管理器里目录是按当前列参与排序的，
			// 缺了这些字段目录段就只能固定按名字排，与文件管理器不一致。
			d := DirEntry{Name: e.Name(), Path: full}
			if e != nil {
				d.Size = e.Size()
				// 与文件（下方）一致：修改时间截断到秒
				d.Mtime = e.ModTime().Unix() * 1000
				if needBtime {
					d.Btime = fileBirthTime(full)
				}
			}
			resp.Dirs = append(resp.Dirs, d)
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		kind := classify(ext)
		if kind == "" {
			continue
		}
		info := e // Readdir 返回的就是 FileInfo，不必再 Info()
		item := FileItem{
			Name: e.Name(),
			Path: full,
			Size: info.Size(),
			// 修改时间截断到秒：飞牛文件管理器的「修改时间」列精确到秒，同一秒内的文件
			// 在它那边相等，先后由目录物理顺序决定（与创建时间同一套语义，见 1.8.82）。
			// 保留毫秒会把同秒文件按亚秒排开 —— 实测表现为「文件管理器第 1 张在大图里
			// 成了第 10 张、第 2 张才是第 1 张」。
			Mtime: info.ModTime().Unix() * 1000,
			Kind:  kind,
			Ext:   ext,
		}
		if needBtime {
			item.Btime = fileBirthTime(full)
		}
		// 图片尺寸不在列表阶段读取（image.DecodeConfig 对大文件夹同步阻塞，严重拖慢打开速度）
		// 前端网格不需要尺寸，查看器打开时浏览器自动适配
		if kind == "video" {
			// video dims from cached meta if available
			if m, ok := loadMetaCache(full, thumbRequestSize(), info); ok {
				item.W, item.H, item.Duration = m.W, m.H, m.Duration
			}
		}
		resp.Files = append(resp.Files, item)
	}

	// 目录段与文件段分别排序，规则完全相同（字段 + 方向）。
	// 飞牛文件管理器也是「目录恒在文件前、目录内部按当前列排」。
	// 用 SliceStable：主字段相等时 less 返回 false，从而保持**目录物理顺序**
	// （上面 Readdir 拿到的不排序顺序）。文件管理器在字段相等时也是返回 0、
	// 靠稳定排序保持它数据源的原始顺序 —— 实测那就是目录物理顺序（ls -f 一致）。
	sort.SliceStable(resp.Dirs, func(i, j int) bool {
		return resp.Dirs[i].sortKey().less(resp.Dirs[j].sortKey(), sp)
	})
	sort.SliceStable(resp.Files, func(i, j int) bool {
		return resp.Files[i].sortKey().less(resp.Files[j].sortKey(), sp)
	})
	resp.Total = len(resp.Files)
	// 回传本次真正生效的排序：前端把「文件管理器排序 → 本应用排序」映射过去，
	// 若某个字段名没被识别（退回默认），用户能在 /api/list 的响应里直接看到，
	// 而不用去猜为什么顺序不对。
	resp.Sort = sp.field
	resp.SortDesc = sp.desc
	return resp, nil
}

// scanRecursive walks the tree and collects all media items (for timeline view).
func scanRecursive(root string, max int) ([]FileItem, error) {
	out := make([]FileItem, 0, 256)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if d.IsDir() {
			// 注意比的是 path 而不是 d.Name()：d.Name() 是 basename，root 是绝对路径，
			// 二者永不相等（原写法让这层"不跳过根目录"的保护完全失效），
			// 结果是以隐藏目录为扫描根时整棵树被 SkipDir，接口恒返回空。
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		kind := classify(ext)
		if kind == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		item := FileItem{
			Name: d.Name(), Path: path, Size: info.Size(),
			Mtime: info.ModTime().UnixMilli(), Kind: kind, Ext: ext,
		}
		if kind == "image" && ext != ".heic" && ext != ".heif" && ext != ".avif" && ext != ".svg" {
			item.W, item.H = imageDimCached(path)
		} else if kind == "video" {
			if m, ok := loadMetaCache(path, thumbRequestSize(), info); ok {
				item.W, item.H, item.Duration = m.W, m.H, m.Duration
			}
		}
		out = append(out, item)
		if max > 0 && len(out) >= max {
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return out, err
	}
	// newest first
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	return out, nil
}

// errStop signals an early (non-error) termination of a walk.
var errStop = &simpleErr{"stop"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
