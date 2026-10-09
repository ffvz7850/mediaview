package main

import (
	"crypto/sha1"
	"encoding/hex"
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

	// 1.8.148：**不再落盘 .meta.json**，元数据只留在内存。
	//
	// 原因：图片那份 meta 基本是纯冗余（列表阶段本来就不读图片尺寸，
	// 只有 handleMeta 命中时能省一次 imageDimCached —— 而那是进程内只读文件头，约 1ms）；
	// 视频那份能省一次 ffprobe（30~80ms），但只在**冷启动后第一次**要，之后 metaMem 命中。
	// 而它让缓存目录的文件数变成实际缩略图的三倍（实测 9,664 → 28,992）。
	//
	// 删掉后：缓存目录是纯镜像结构（一张图一个文件，文件名与源文件同名）。
	_ = path
	_ = size
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
	// 1.8.148：不再读 .meta.json（已不落盘），只查内存缓存。
	// 冷启动后第一次遇到某个视频会走一次 ffprobe，之后 metaMem 命中 —— 可接受。
	key := cacheKey(path, info)
	if m, ok := loadMetaMem(key); ok {
		return m, true
	}
	return nil, false
}
