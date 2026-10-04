package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed all:web
var webFS embed.FS

// versionPlaceholder 出现在 web/index.html 与 web/bridge.js 里，响应时替换成真实版本号。
//
// 为什么必须由服务端注入：前端资源文件名是固定的（app.js / style.css / bridge.js），
// 只能靠 URL 上的 ?v=<版本> 区分新旧。这个版本号手工维护迟早会漏 —— 1.8.17 就漏了
// （HTML 里写的还是 ?v=1.8.16），浏览器因此命中 1.8.16 的缓存 JS：用户看得见新加的
// "源文件直出"开关（HTML 是 no-cache 所以是新的），但旧 JS 的保存请求里根本没有
// rawOriginal 字段，一保存就被后端全量覆盖回默认值，表现为"开关保存不住"。
// （该开关已在 1.8.22 移除，但这条教训对所有设置项都成立。）
//
// 改成服务端注入后，版本号只有 main.go 的 version 常量一处，不可能再不同步。
const versionPlaceholder = "__MV_VERSION__"

type frontendHandler struct{}

func (h *frontendHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, gwPrefix)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		rel = "index.html"
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.Error(w, "fs error", 500)
		return
	}
	// SPA fallback: unknown non-asset routes -> index.html
	if _, err := fs.Stat(sub, rel); err != nil {
		if strings.Contains(rel, ".") {
			http.NotFound(w, r)
			return
		}
		rel = "index.html"
	}

	f, err := sub.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat error", 500)
		return
	}

	// 文本资源先做版本号替换再发出去。
	if isTextAsset(rel) {
		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read error", 500)
			return
		}
		data = bytes.ReplaceAll(data, []byte(versionPlaceholder), []byte(version))

		// no-cache = 每次使用前都要回源验证（命中即 304，开销极小）。
		// 这里刻意不用 immutable：资源文件名固定，一旦版本串不同步，
		// immutable 会让用户在一年内都跑旧代码 —— 正是这次故障的成因。
		// ETag 取内容哈希：文件一变 ETag 就变，不依赖版本号是否记得同步。
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", contentETag(data))
		// modtime 传零值：embed 里的文件本来就没有有意义的修改时间，
		// 而内容哈希已经足以做条件请求判断。
		http.ServeContent(w, r, info.Name(), time.Time{}, bytes.NewReader(data))
		return
	}

	// 二进制资源（图片等）：Immutable + 一年有效期。
	// 改版时 HTML 里的 ?v= 会跟着版本变，URL 一变浏览器必然重新下载，
	// 所以同一个版本内可以放心强缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// fallback: stream copy
		io.Copy(w, f)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
}

// isTextAsset 判断哪些资源需要做版本占位符替换。
func isTextAsset(rel string) bool {
	return strings.HasSuffix(rel, ".html") ||
		strings.HasSuffix(rel, ".js") ||
		strings.HasSuffix(rel, ".css")
}

// contentETag 用内容哈希做 ETag。
func contentETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}
