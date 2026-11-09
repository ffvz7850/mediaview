package main

// 2.4.23：封面元数据从 .json sidecar 换到 jpg 的扩展属性（xattr）。
//
// 用户诉求原话：「现在封面会生成一张 .json 文件，能取消掉吗」——
// 一个封面两个文件（jpg + json）看着乱，dircov/ 下的文件数还翻倍。
//
// 但这份 json 不能直接删：它是「这个封面是从哪几张图拼的、走过哪些目录」
// 的唯一记录，判缓存有效性、按目录清封面、预生成幂等都靠它。
// 所以这版是**换载体**：写进 jpg 自己的 user.mediaview.dircov 属性，
// 目录里就只剩一张图；写不进去（网络盘 / exFAT / 非 Linux / 超 4KB）再回退 .json。
//
// ⚠ 本机开发是 Windows，平台实现走 no-op 分支 —— 也就是说**不注入 stub 的话，
// 这里测的全是"回退"那一半**。所以下面用 installStubCoverMeta 把 xattr 换成内存 map，
// 才真正覆盖到"写进 xattr 之后目录里没有 .json"这条主路径。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installStubCoverMeta 把 xattr 读写换成内存 map，并在测试结束时还原。
// 返回的 map 以"文件路径"为键，模拟"属性跟着文件走"。
func installStubCoverMeta(t *testing.T) map[string][]byte {
	t.Helper()
	mem := map[string][]byte{}
	prevSet, prevGet := coverMetaSet, coverMetaGet
	coverMetaSet = func(p string, raw []byte) error {
		if len(raw) > coverMetaXattrLimit {
			return errCoverMetaTooLarge
		}
		mem[p] = append([]byte(nil), raw...)
		return nil
	}
	coverMetaGet = func(p string) ([]byte, bool) {
		v, ok := mem[p]
		return v, ok
	}
	t.Cleanup(func() { coverMetaSet, coverMetaGet = prevSet, prevGet })
	return mem
}

// samplePick 造一个"内容有效"的封面元数据：目录 mtime 与 Items 里的文件都真实存在。
func samplePick(t *testing.T, media string) *coverPick {
	t.Helper()
	img := filepath.Join(media, "a.jpg")
	if err := os.WriteFile(img, []byte("jpg"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(media)
	if err != nil {
		t.Fatal(err)
	}
	return &coverPick{
		V: coverCacheVersion, Dir: media, PickAt: 1762345678901,
		Source: "direct", Depth: 0, Walked: 2,
		Items:  []coverItem{{Path: img, Mtime: 1, Kind: "image"}},
		Checks: []coverCheck{{Path: media, Mtime: st.ModTime().UnixMilli()}},
	}
}

func mkFakeJPG(t *testing.T, metaPath string) {
	t.Helper()
	jpg := coverJPGOf(metaPath)
	if err := os.MkdirAll(filepath.Dir(jpg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jpg, []byte("jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------- ① xattr 可用：目录里只剩 jpg ----------

func Test2423XattrNoSidecar(t *testing.T) {
	mem := installStubCoverMeta(t)
	media := t.TempDir()
	p := samplePick(t, media)

	base := filepath.Join(t.TempDir(), coverCacheDirName, "ab", "ab3f9c1d2e4b5a60_320")
	mkFakeJPG(t, base+".json")

	saveCoverPick(base+".json", p)

	if fileExists(base + ".json") {
		t.Error("★ xattr 可写时不该再落 .json —— 用户要的就是目录里只有封面图")
	}
	if _, ok := mem[coverJPGOf(base+".json")]; !ok {
		t.Error("★ 元数据没有写进 jpg 的扩展属性（那它就等于丢了）")
	}
	// 读回来必须逐字段一致：截断/串行化 bug 会在这里露出来
	got := loadCoverPick(base+".json", media)
	if got == nil {
		t.Fatal("★ 写进 xattr 之后读不回来了")
	}
	if got.PickAt != p.PickAt || got.Dir != p.Dir || len(got.Items) != 1 || len(got.Checks) != 1 {
		t.Errorf("读回的元数据与写入不一致：%+v", got)
	}
}

// ---------- ② 超限：宁可留 .json，也不能写丢 ----------

func Test2423MetaTooLargeFallsBack(t *testing.T) {
	mem := installStubCoverMeta(t)
	media := t.TempDir()
	p := samplePick(t, media)
	// 用超长路径把 meta 撑过 xattr 上限（真实场景：深目录 + 32 个检查目录）
	p.Checks = append(p.Checks, coverCheck{
		Path:  "/vol1/1000/" + repeatN("很长的目录名/", 300),
		Mtime: 1,
	})
	raw, _ := json.Marshal(p)
	if len(raw) <= coverMetaXattrLimit {
		t.Fatalf("构造的 meta 只有 %d 字节，没超过上限 %d，用例失去意义", len(raw), coverMetaXattrLimit)
	}

	base := filepath.Join(t.TempDir(), coverCacheDirName, "cd", "cdef012345678901_320")
	mkFakeJPG(t, base+".json")
	saveCoverPick(base+".json", p)

	if len(mem) != 0 {
		t.Error("★ 超限时不该硬写 xattr（会 ERANGE，等于把元数据弄丢）")
	}
	if !fileExists(base + ".json") {
		t.Error("★ 超限时必须回退写 .json —— 否则这份元数据就彻底没了")
	}
}

// ---------- ③ 文件系统不支持 xattr：回退 .json 且功能不降级 ----------

func Test2423XattrUnsupportedFallsBack(t *testing.T) {
	prevSet, prevGet := coverMetaSet, coverMetaGet
	coverMetaSet = func(string, []byte) error { return errCoverXattrUnsupported }
	coverMetaGet = func(string) ([]byte, bool) { return nil, false }
	t.Cleanup(func() { coverMetaSet, coverMetaGet = prevSet, prevGet })

	media := t.TempDir()
	p := samplePick(t, media)
	base := filepath.Join(t.TempDir(), coverCacheDirName, "ef", "ef01234567890123_320")
	mkFakeJPG(t, base+".json")

	saveCoverPick(base+".json", p)
	if !fileExists(base + ".json") {
		t.Fatal("★ 不支持 xattr 时必须落 .json（网络盘 / exFAT 上不然就没有元数据了）")
	}
	if got := loadCoverPick(base+".json", media); got == nil {
		t.Error("★ 回退路径下读不回来 —— 功能降级了")
	}
}

// ---------- ⑤ 清空缓存：dircov 必须被清，用户数据目录必须留着 ----------

func Test2423PurgeCacheIncludesDircov(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join("320", "vol1", "1000", "a.jpg"))
	mk(filepath.Join(coverCacheDirName, "ab", "abcdef0123456789_320.jpg"))
	mk(filepath.Join(coverCacheDirName, "ab", "abcdef0123456789_320.json"))
	mk(filepath.Join("我的资料", "别动.txt")) // 用户自己放的：绝不能删

	thumbRootMu.Lock()
	prev := thumbRoot
	thumbRoot = root
	thumbRootMu.Unlock()
	t.Cleanup(func() {
		thumbRootMu.Lock()
		thumbRoot = prev
		thumbRootMu.Unlock()
	})

	if _, err := purgeCache(); err != nil {
		t.Fatal(err)
	}

	if fileExists(filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320.jpg")) {
		t.Error("★★ 「清空缓存」没有清掉封面 —— 2.4.22 及之前就是这样：按钮点了、数字归零，封面原封不动")
	}
	if fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) {
		t.Error("★ 缩略图目录应当被清掉")
	}
	if !fileExists(filepath.Join(root, "我的资料", "别动.txt")) {
		t.Error("★★ 误删了非本应用创建的目录 —— 缓存根可能是用户指定的共享目录，这条是红线")
	}
}

// ---------- ⑥ 统计口径：封面元数据不算「已缓存 N 张」 ----------

func Test2423CacheStatsIgnoresCoverMeta(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join("320", "vol1", "a.jpg"))                // 真实缩略图
	mk(filepath.Join(coverCacheDirName, "ab", "x_320.jpg"))  // 封面图
	mk(filepath.Join(coverCacheDirName, "ab", "x_320.json")) // 封面元数据（回退载体）
	mk(filepath.Join("320", "vol1", "a.jpg.key"))            // 旧版 sidecar
	mk(filepath.Join("我的资料", "别的东西.bin"))                    // 用户放在缓存根下的目录：不是缓存

	thumbRootMu.Lock()
	prev := thumbRoot
	thumbRoot = root
	thumbRootMu.Unlock()
	thumbRootMu.Lock()
	thumbRootMu.Unlock()
	t.Cleanup(func() {
		thumbRootMu.Lock()
		thumbRoot = prev
		thumbRootMu.Unlock()
	})
	invalidateCacheStats()
	t.Cleanup(invalidateCacheStats)

	files, _ := cacheStats()
	if files != 2 {
		t.Errorf("★ 「已缓存」应只数本应用创建的产物（缩略图 1 + 封面图 1 = 2），实际 %d —— 元数据被算进去了，或把用户放在缓存根下的东西也算了", files)
	}
}

// ---------- ⑦ 元数据只在 xattr 里时，按目录清封面仍然能删 ----------

func Test2423PurgeDirCoverWithXattrOnly(t *testing.T) {
	mem := installStubCoverMeta(t)
	root := t.TempDir()
	media := t.TempDir()

	base := filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320")
	mkFakeJPG(t, base+".json")
	p := samplePick(t, media)
	saveCoverPick(base+".json", p)
	if fileExists(base + ".json") {
		t.Fatal("前置条件不成立：本用例要求元数据只在 xattr 里")
	}
	if len(mem) == 0 {
		t.Fatal("前置条件不成立：xattr 为空")
	}

	if n := purgeDirCover(root, media); n != 1 {
		t.Errorf("★ 元数据在 xattr 里时也应能按目录删掉封面（实际删了 %d 张）—— 只遍历 .json 会漏", n)
	}
	if fileExists(coverJPGOf(base + ".json")) {
		t.Error("封面图没删掉")
	}
}

// repeatN 拼重复字符串（造超长路径用）。
func repeatN(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// ---------- ⑨ 静态契约：元数据必须写在图片落盘之后 ----------
//
// 这条用单测测不出来（buildCover 要真实缩略图，本机 ffmpeg 探测不到），
// 但只要有人把 saveCoverPick 挪回 composeCover 之前，xattr 就永远写不进去、
// 退回成 .json —— 而这正是这一版要消灭的东西。所以直接钉源码顺序。
func Test2423SaveMetaHappensAfterJPG(t *testing.T) {
	src := readSourceOrSkip(t, "dircov.go")
	i := strings.Index(src, "func buildCover(")
	if i < 0 {
		t.Fatal("找不到 buildCover")
	}
	seg := src[i:]
	if j := strings.Index(seg, "\nfunc "); j > 0 {
		seg = seg[:j]
	}
	posWrite := strings.Index(seg, "os.Rename(tmp, jpgPath)")
	posSave := strings.LastIndex(seg, "saveCoverPick(metaPath, p)")
	if posWrite < 0 {
		t.Fatal("buildCover 里找不到写 jpg 的 rename")
	}
	if posSave < 0 {
		t.Fatal("buildCover 里找不到 saveCoverPick")
	}
	if posSave < posWrite {
		t.Error("★★ 元数据必须在 jpg 落盘之后保存 —— xattr 挂在图片上，顺序反了就只能回退 .json（等于没改）")
	}
}
