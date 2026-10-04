package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// cacheKey derives a stable cache id from path + size + mtime.
func cacheKey(path string, info os.FileInfo) string {
	sum := sha1.Sum([]byte(path + "|" +
		info.ModTime().UTC().Format("20060102150405.000000000") +
		"|" + intToStr(info.Size())))
	return hex.EncodeToString(sum[:])
}

func intToStr(n int64) string {
	return strconv.FormatInt(n, 10)
}

// thumbPathFor 返回缩略图的磁盘路径，按原文件目录结构分文件夹存放。
// 结构：<thumbRoot>/{size}/{原文件相对路径}（去掉前导斜杠）
// 例如原文件 /vol3/1000/备份/旅游/IMG.jpg，size=320 → <thumbRoot>/320/vol3/1000/备份/旅游/IMG.jpg
func thumbPathFor(path string, size int) string {
	// 必须去掉前导斜杠，否则 filepath.Join 会把它当绝对路径丢弃前面的 root 和 size
	rel := strings.TrimPrefix(path, "/")
	return filepath.Join(currentThumbRoot(), strconv.Itoa(size), rel)
}

// thumbJPG / thumbMeta 旧的 hash 命名方式，保留用于兼容旧缓存清理。
func thumbJPG(key string) string  { return filepath.Join(currentThumbRoot(), key+".jpg") }
func thumbMeta(key string) string { return filepath.Join(currentThumbRoot(), key+".json") }

// metaPathFor 在缩略图被关闭时返回 ""，表示元数据只留在内存里。
// 这样"不生成缩略图"就真的不在磁盘上留任何东西。
func metaPathFor(path string, size int) string {
	if !getSettings().ThumbEnabled {
		return ""
	}
	return metaPathForFile(path, size)
}

// metaPathForFile 不带开关判断的纯路径计算（供内部调用）
func metaPathForFile(path string, size int) string {
	rel := strings.TrimPrefix(path, "/")
	return filepath.Join(currentThumbRoot(), strconv.Itoa(size), rel+".meta.json")
}

// ---- 元数据内存缓存 ----
//
// 元数据（宽高/时长）与缩略图是两件事：即使不生成缩略图，列表页也要显示分辨率。
// 因此元数据始终有一份内存缓存；只有开启缩略图时才额外落盘（下次启动免 probe）。

var (
	metaMemMu sync.RWMutex
	metaMem   = map[string]*Meta{}
)

func saveMeta(key string, path string, size int, m *Meta) {
	if m == nil {
		return
	}
	metaMemMu.Lock()
	metaMem[key] = m
	metaMemMu.Unlock()

	if p := metaPathFor(path, size); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			if data, err := json.Marshal(m); err == nil {
				_ = os.WriteFile(p, data, 0o644)
			}
		}
	}
}

func loadMetaMem(key string) (*Meta, bool) {
	metaMemMu.RLock()
	defer metaMemMu.RUnlock()
	m, ok := metaMem[key]
	return m, ok
}

// dropMetaMem 清空内存元数据缓存（清空缩略图缓存时一并调用）
func dropMetaMem() {
	metaMemMu.Lock()
	metaMem = map[string]*Meta{}
	metaMemMu.Unlock()
}

func loadMetaCache(path string, size int, info os.FileInfo) (*Meta, bool) {
	if info == nil {
		return nil, false
	}
	key := cacheKey(path, info)
	if m, ok := loadMetaMem(key); ok {
		return m, true
	}
	// 关闭缩略图时不读磁盘：目录可能已被换到别处，读它没有意义
	if !getSettings().ThumbEnabled {
		return nil, false
	}
	data, err := os.ReadFile(metaPathForFile(path, size))
	if err != nil {
		return nil, false
	}
	var m Meta
	if err := json.Unmarshal(bytes.TrimSpace(data), &m); err != nil {
		return nil, false
	}
	metaMemMu.Lock()
	metaMem[key] = &m
	metaMemMu.Unlock()
	return &m, true
}
