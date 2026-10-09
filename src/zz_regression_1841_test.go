package main

// 1.8.41：针对 1.8.39 / 1.8.40 两轮改动做的**回归自审**所发现问题的用例。
//
// 这一批问题都是"新改动引入"的：
//   ① 含画质的标识被当成 saveMeta 的 key → 内存元数据缓存永久失效、每次列目录都读盘；
//   ② 负缓存不分前后台 → 一次偶发失败让图在 TTL 内一直显示占位图；
//   ③ 内容标识 sidecar 被算进缓存文件数 → 「已缓存 N 个文件」凭空翻倍。
//
// 注：本机没有 Linux/WSL，而缩略图落盘路径用的是 Unix 语义（生产环境就是 Linux），
// 所以这里不真的生成缩略图文件，改为「内存往返 + 源码契约」两种平台无关的验证方式。
import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct {
	size int64
	mod  time.Time
}

func (f fakeFileInfo) Name() string       { return "x.jpg" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return f.mod }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

// ① saveMeta 写进去的 key，loadMetaCache 必须能查到（同一把 key）。
func TestMetaCacheKeyRoundtrip(t *testing.T) {
	p := "/vol1/1000/照片/x.jpg"
	info := fakeFileInfo{size: 1234, mod: time.Unix(1700000000, 0)}

	key := cacheKey(p, info)
	dropMetaMem()
	saveMeta(key, p, 64, &Meta{W: 100, H: 50}) // 磁盘写失败无所谓，内存已写入

	m, ok := loadMetaCache(p, 64, info)
	if !ok {
		t.Fatal("loadMetaCache 查不到 saveMeta 刚写入的元数据 —— 两处用了不同的 key")
	}
	if m.W != 100 || m.H != 50 {
		t.Errorf("元数据内容不对：%+v", m)
	}
}

// ①+③ 源码契约：两个 key 不能混用、cacheID 必须带画质、负缓存必须用 cacheID。
func TestThumbSourceKeysNotMixed(t *testing.T) {
	src, err := os.ReadFile("thumb.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	// generateThumb 必须同时收到「元数据 key」与「产物标识 cacheID」
	// （1.8.61 起签名末尾多了 immediate：大小图预览走预留槽，见 zzz_1861_*_test.go）
	if !strings.Contains(s, "key, cacheID string, foreground, immediate bool") {
		t.Error("generateThumb 的签名里 key/cacheID 不再是并列的两个参数 —— " +
			"元数据 key 与产物标识被混用，会导致内存元数据缓存失效（每个视频每次列目录都读盘）")
	}
	// 产物的成功出口必须**全部**写 cacheID。
	// 修过一次教训：其中两处曾写成 key（缩进不同，批量替换时漏掉），
	// 结果是所有小图（Go 解码）和视频的缩略图缓存永不命中、每次进目录都重新生成。
	//
	// 1.8.125：出口从 4 条收敛为 2 条 —— 删掉了「图片 VAAPI 硬解」与
	// 「大图走 ffmpeg 软件解码」两条分支（实测 vips 比那条 ffmpeg 快 34%，
	// 见 mediaview-压缩逻辑调研与清理清单.md）。现在只剩：
	//   ① vips / Go 原生（图片统一路径）
	//   ② 通用 ffmpeg（视频，以及 vips/Go 都解不了的图片格式）
	if n := strings.Count(s, "return finishThumb(out, cacheID)"); n != 3 {
		t.Errorf("finishThumb(out, cacheID) 出现 %d 次，应为 3 次（图片统一路径 / 图片 ffmpeg 引擎 / 通用 ffmpeg）", n)
	}
	if strings.Contains(s, "finishThumb(out, key)") {
		t.Error("有成功出口把不含画质的 key 写进了产物标识 —— 这些缩略图的缓存会永久失效")
	}
	// 产物标识要含画质，否则改画质不会重新生成
	if !strings.Contains(s, `cacheID := key + "|q" + strconv.Itoa(getSettings().ThumbQuality)`) {
		t.Error("cacheID 没有把画质算进去 —— 改画质后服务端会一直返回旧画质的缩略图")
	}
	// 负缓存必须用 cacheID，而且前台不受它影响
	if strings.Contains(s, "markThumbFailed(key)") {
		t.Error("负缓存写的是元数据 key（应为 cacheID）")
	}
	if !strings.Contains(s, "if !foreground && thumbFailedRecently(cacheID)") {
		t.Error("负缓存没有区分前后台 —— 一次偶发失败（如被 pause 抢占）会让图片在 TTL 内一直显示占位图")
	}
	// 取消/超时属于可恢复，不该进负缓存
	if !strings.Contains(s, "!isCancelErr(err)") {
		t.Error("负缓存没有排除 context 取消/超时这类可恢复错误")
	}

	cacheSrc, err := os.ReadFile("cache.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cacheSrc), "key := cacheKey(path, info)") {
		t.Error("loadMetaCache 的 key 算法变了 —— 请确认它仍与 saveMeta 用的一致")
	}
}

// ③ 缓存占用统计不能把 sidecar 算成缓存文件（否则数字翻倍）。
func TestCacheStatsSkipsKeySidecar(t *testing.T) {
	dir := t.TempDir()
	settingsMu.Lock()
	old := settings
	settings.ThumbEnabled = true
	settings.ThumbDir = dir
	settingsMu.Unlock()
	applyThumbRoot()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings = old
		settingsMu.Unlock()
		applyThumbRoot()
	})

	sizeDir := filepath.Join(currentThumbRoot(), "64")
	if err := os.MkdirAll(sizeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.jpg", "a.jpg.meta.json", "b.jpg"} {
		if err := os.WriteFile(filepath.Join(sizeDir, n), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 两个 sidecar：它们不该被计进「已缓存文件数」
	for _, n := range []string{"a.jpg" + thumbKeySuffix, "b.jpg" + thumbKeySuffix} {
		if err := os.WriteFile(filepath.Join(sizeDir, n), []byte("key"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	invalidateCacheStats()
	n, _ := cacheStats()
	// 1.8.148：.meta.json 也停止落盘了，并且统计口径同样跳过它 → 只有 2 张真缩略图。
	// （旧缓存里可能残留 .meta.json，所以这条过滤仍然必须保留。）
	if n != 2 {
		t.Errorf("cacheStats 统计到 %d 个文件，期望 2（2 张缩略图；不含 .key 与 .meta.json）—— "+
			"「已缓存 N 个文件」会凭空翻倍", n)
	}
}

// ① 内容标识的读写往返。
func TestThumbKeySidecarRoundtrip(t *testing.T) {
	// 1.8.148 起，内容标识的存储顺序是：
	//   xattr（首选，Linux）→ 旧 .key sidecar（只读，兼容升级前的缓存）→
	//   产物存在且非空（最后兜底）
	//
	// 本用例覆盖后两级（Windows 上 xattr 走 thumbxattr_other.go，恒不可用）。

	dir := t.TempDir()
	out := filepath.Join(dir, "t.jpg")
	if err := os.WriteFile(out, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① 旧 .key sidecar 仍然被读取 —— 保证升级后**已有缓存不会全部失效**
	if err := os.WriteFile(out+thumbKeySuffix, []byte("id-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !thumbCacheKeyMatches(out, "id-1") {
		t.Error("旧 .key sidecar 里的内容标识读不回来 —— 升级后已有缓存会全部失效")
	}
	if thumbCacheKeyMatches(out, "id-2") {
		t.Error("有 sidecar 时内容标识比对失效：标识不同却仍判定命中")
	}

	// ② 产物**不存在**时必须判为不命中（这是一个真 bug 的回归点：
	//    兜底分支最初写成无条件 return true，导致连不存在的文件都算命中）
	missing := filepath.Join(dir, "nope.jpg")
	if thumbCacheKeyMatches(missing, "id-1") {
		t.Error("产物文件不存在，却判定命中缓存 —— 兜底分支必须先 Stat")
	}

	// ③ 空产物同样不算命中
	empty := filepath.Join(dir, "empty.jpg")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if thumbCacheKeyMatches(empty, "id-1") {
		t.Error("产物是空文件，却判定命中缓存")
	}

	// ④ writeThumbKey 写入后必须能被读回（xattr 可用时严格相等；
	//    xattr 不可用时会落到"产物存在即命中"，同样为 true）
	out2 := filepath.Join(dir, "t2.jpg")
	if err := os.WriteFile(out2, []byte("jpeg2"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeThumbKey(out2, "id-9")
	if !thumbCacheKeyMatches(out2, "id-9") {
		t.Error("writeThumbKey 写入后读不回来")
	}
}

// ② 负缓存只该挡住后台；前台是用户正在等的那张图，必须真的再试一次。
func TestNegativeCacheOnlyBlocksBackground(t *testing.T) {
	svg := filepath.Join(t.TempDir(), "x.svg")
	if err := os.WriteFile(svg, []byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureThumbBG(context.Background(), svg, "image", 64); err == nil {
		t.Skip("这个环境居然能生成 svg 缩略图（装了 librsvg 的 ffmpeg），跳过")
	}
	if _, err := ensureThumbBG(context.Background(), svg, "image", 64); err != errThumbFailed {
		t.Errorf("后台第二次应命中负缓存 errThumbFailed，实际 %v", err)
	}
	if _, err := ensureThumb(svg, "image", 64); err == errThumbFailed {
		t.Error("前台被负缓存挡住了：一次偶发失败会让这张图在 TTL 内一直显示占位图")
	}
}
