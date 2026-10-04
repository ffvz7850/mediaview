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
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Size     int64   `json:"size"`
	Mtime    int64   `json:"mtime"`
	Kind     string  `json:"kind"` // image | video
	Ext      string  `json:"ext"`
	W        int     `json:"w,omitempty"`
	H        int     `json:"h,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ListResponse struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent"`
	Dirs   []DirEntry `json:"dirs"`
	Files  []FileItem `json:"files"`
	Total  int        `json:"total"`
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
	entries, err := os.ReadDir(p)
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
			if !strings.HasPrefix(e.Name(), ".") {
				resp.Dirs = append(resp.Dirs, DirEntry{Name: e.Name(), Path: full})
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		kind := classify(ext)
		if kind == "" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		item := FileItem{
			Name:  e.Name(),
			Path:  full,
			Size:  info.Size(),
			Mtime: info.ModTime().UnixMilli(),
			Kind:  kind,
			Ext:   ext,
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

	// sort dirs by name
	sort.Slice(resp.Dirs, func(i, j int) bool {
		return strings.ToLower(resp.Dirs[i].Name) < strings.ToLower(resp.Dirs[j].Name)
	})
	// sort files
	switch sortBy {
	case "name":
		sort.Slice(resp.Files, func(i, j int) bool {
			return strings.ToLower(resp.Files[i].Name) < strings.ToLower(resp.Files[j].Name)
		})
	case "size":
		sort.Slice(resp.Files, func(i, j int) bool { return resp.Files[i].Size > resp.Files[j].Size })
	default: // date: newest first
		sort.Slice(resp.Files, func(i, j int) bool { return resp.Files[i].Mtime > resp.Files[j].Mtime })
	}
	resp.Total = len(resp.Files)
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
			item.W, item.H = imageDim(path)
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
