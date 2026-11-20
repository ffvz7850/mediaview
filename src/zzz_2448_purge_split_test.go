package main

// 2.4.48：清缓存拆成两类 —— 缩略图 / 封面缩略图，各自独立。
//
// 用户报的现象：设置里点「清空缓存」，封面和缩略图一起被清掉。
// 但这两类的重建代价完全不同（缩略图要逐张解码，封面只是拼一张图），
// 而且"换缩略图尺寸"这种场景根本不需要动封面。所以拆开。
//
// 这里钉住四件事：
//   ① 只清一类时，另一类必须原样留着；
//   ② 白名单之外（用户自己放在缓存根下的目录）任何情况下都不许碰；
//   ③ `?what=` 的解析与错误处理（未知值 400、GET 405）；
//   ④ 设置页确实有两个按钮，前端确实分别调了 thumb / cover。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// purgeFixture 造一棵缓存树，并把 thumbRoot 指向它。
// 布局：320/…（缩略图）、dircov/…（封面 + 元数据）、我的资料/…（用户自己的，绝不能删）
func purgeFixture(t *testing.T) string {
	t.Helper()
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
	mk(filepath.Join("800", "vol1", "1000", "a.jpg"))
	mk(filepath.Join(coverCacheDirName, "ab", "abcdef0123456789_320.jpg"))
	mk(filepath.Join(coverCacheDirName, "ab", "abcdef0123456789_320.json"))
	mk(filepath.Join("我的资料", "别动.txt"))

	thumbRootMu.Lock()
	prev := thumbRoot
	thumbRoot = root
	thumbRootMu.Unlock()
	t.Cleanup(func() {
		thumbRootMu.Lock()
		thumbRoot = prev
		thumbRootMu.Unlock()
	})
	invalidateCacheStats()
	t.Cleanup(invalidateCacheStats)
	return root
}

func Test2448PurgeThumbKeepsCovers(t *testing.T) {
	root := purgeFixture(t)

	if _, err := purgeCacheKind(purgeThumbs); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) ||
		fileExists(filepath.Join(root, "800", "vol1", "1000", "a.jpg")) {
		t.Error("★ 只清缩略图时，尺寸档目录应当被删掉")
	}
	if !fileExists(filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320.jpg")) {
		t.Error("★★ 清缩略图把封面也删了 —— 这正是用户报的问题，两类必须互不影响")
	}
	if !fileExists(filepath.Join(root, "我的资料", "别动.txt")) {
		t.Error("★★ 误删了非本应用创建的目录（缓存根可能是共享目录，这是红线）")
	}
}

func Test2448PurgeCoverKeepsThumbs(t *testing.T) {
	root := purgeFixture(t)

	if _, err := purgeCacheKind(purgeCovers); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320.jpg")) {
		t.Error("★ 只清封面时，dircov 目录应当被整体删掉")
	}
	if !fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) {
		t.Error("★★ 清封面把缩略图也删了 —— 缩略图重建要逐张解码，代价高得多，绝不能连带")
	}
	if !fileExists(filepath.Join(root, "我的资料", "别动.txt")) {
		t.Error("★★ 误删了非本应用创建的目录")
	}
}

func Test2448PurgeAllClearsBoth(t *testing.T) {
	root := purgeFixture(t)

	if _, err := purgeCacheKind(purgeAll); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) ||
		fileExists(filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320.jpg")) {
		t.Error("★ all 必须两类都清（「清除所有数据」走的就是这条）")
	}
	if !fileExists(filepath.Join(root, "我的资料", "别动.txt")) {
		t.Error("★★ 误删了非本应用创建的目录")
	}
}

func Test2448ParsePurgeKind(t *testing.T) {
	cases := map[string]purgeKind{
		"":           purgeAll, // 老调用只发 POST、不带参数 —— 必须仍然按"全清"处理
		"all":        purgeAll,
		"both":       purgeAll,
		"thumb":      purgeThumbs,
		"thumbs":     purgeThumbs,
		"thumbnail":  purgeThumbs,
		"thumbnails": purgeThumbs,
		" Thumb ":    purgeThumbs,
		"cover":      purgeCovers,
		"covers":     purgeCovers,
		"dircov":     purgeCovers,
	}
	for in, want := range cases {
		got, ok := parsePurgeKind(in)
		if !ok || got != want {
			t.Errorf("parsePurgeKind(%q) = %q,%v；期望 %q,true", in, got, ok, want)
		}
	}
	if _, ok := parsePurgeKind("everything"); ok {
		t.Error("未知的 what 必须被拒绝（否则等于给了个静默的全清入口）")
	}
}

func Test2448PurgeHandlerContract(t *testing.T) {
	root := purgeFixture(t)

	// GET 不许清（跨站 <img src> 就能触发）
	rec := httptest.NewRecorder()
	handleSettingsPurge(rec, httptest.NewRequest(http.MethodGet, "/api/settings/purge?what=thumb", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 应返回 405，实际 %d", rec.Code)
	}
	if !fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) {
		t.Fatal("★★ GET 不该产生任何删除")
	}

	// 未知 what → 400，且什么都不删
	rec = httptest.NewRecorder()
	handleSettingsPurge(rec, httptest.NewRequest(http.MethodPost, "/api/settings/purge?what=everything", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知 what 应返回 400，实际 %d", rec.Code)
	}
	if !fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) {
		t.Error("★ 未知 what 不该删任何东西")
	}

	// what=cover → 200，只删封面
	rec = httptest.NewRecorder()
	handleSettingsPurge(rec, httptest.NewRequest(http.MethodPost, "/api/settings/purge?what=cover", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("what=cover 应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%v %s", err, rec.Body.String())
	}
	if resp["what"] != "cover" {
		t.Errorf("响应应回显 what=cover，实际 %v", resp["what"])
	}
	if fileExists(filepath.Join(root, coverCacheDirName, "ab", "abcdef0123456789_320.jpg")) {
		t.Error("★ what=cover 没删掉封面")
	}
	if !fileExists(filepath.Join(root, "320", "vol1", "1000", "a.jpg")) {
		t.Error("★★ what=cover 把缩略图也删了")
	}
}

func Test2448SettingsViewSplitsCache(t *testing.T) {
	purgeFixture(t)

	v := buildSettingsView()
	if v.CacheThumbFiles != 2 {
		t.Errorf("缩略图应统计到 2 个（320/800 各一张），实际 %d", v.CacheThumbFiles)
	}
	if v.CacheCoverFiles != 1 {
		t.Errorf("封面应统计到 1 个（.json 元数据不算），实际 %d", v.CacheCoverFiles)
	}
	if v.CacheFiles != 3 {
		t.Errorf("合计应仍是 3（向后兼容 field），实际 %d", v.CacheFiles)
	}
}

func Test2448FrontendHasTwoPurgeButtons(t *testing.T) {
	html := mustReadWebFile(t, "index.html")
	for _, id := range []string{`id="btnPurgeThumb"`, `id="btnPurgeCover"`} {
		if !strings.Contains(html, id) {
			t.Errorf("★ 设置页缺少按钮 %s —— 用户要求缩略图与封面各一个按钮", id)
		}
	}
	if strings.Contains(html, `id="btnPurge"`) {
		t.Error("★ 旧的单一「清空缓存」按钮还在：两个新按钮之外再留一个全清的，等于没拆")
	}

	js := mustReadWebFile(t, "app.js")
	if !strings.Contains(js, "/settings/purge?what=' + what") {
		t.Error("★ 前端必须把 what 传给后端（否则两个按钮清的是同一堆东西）")
	}
	if !strings.Contains(js, "purgeCacheOf('thumb'") || !strings.Contains(js, "purgeCacheOf('cover'") {
		t.Error("★ 两个按钮必须分别清 thumb / cover")
	}
	// 封面要有自己的缓存计数器：否则清缩略图会把封面 URL 一起换掉（无谓地全量重下）。
	// 「coverURL 用的是 coverCacheBust 且清缩略图不改它」由 zzz_2414 的真实 JS 行为探针钉住，
	// 这里只验证两个计数器确实各自声明、各自持久化，不再用脆弱的源码子串猜测。
	for _, line := range []string{
		"var thumbCacheBust = 0;", "var coverCacheBust = 0;",
		"mediaview_thumb_bust", "mediaview_cover_bust",
	} {
		if !strings.Contains(js, line) {
			t.Errorf("★ 前端缺少 %q：两个缓存计数器必须各自声明且持久化", line)
		}
	}
}

// Test2448BustCountersFollowWhatWasCleared 钉住两个 URL 计数器的失效范围：
//
//	· 只清缩略图 ⇒ 封面没被删，封面记忆必须保留（bumpThumb 传 keepCover），否则封面白闪一下；
//	· "清除所有数据" ⇒ 封面也清了，两个计数器都必须换，否则封面沿用一年前的旧图。
//
// 这两条都是 2.4.48 拆分时最容易漏的对偶关系。
func Test2448BustCountersFollowWhatWasCleared(t *testing.T) {
	js := mustReadWebFile(t, "app.js")

	// 清缩略图按钮：bumpThumb 必须带 keepCover=true
	if !strings.Contains(js, "bumpThumbCacheBust(true)") {
		t.Error("★ 清缩略图按钮必须调 bumpThumbCacheBust(true)：封面没被删，别让它淡入闪一下")
	}

	// factory reset：从按钮声明到 Boot 段之间，两个 bump 都要出现
	i := strings.Index(js, "$('btnFactoryReset').onclick")
	if i < 0 {
		t.Fatal("找不到 factory reset 按钮处理")
	}
	block := js[i:]
	if j := strings.Index(block, "// ---------- Boot"); j >= 0 {
		block = block[:j]
	}
	for _, fn := range []string{"bumpThumbCacheBust()", "bumpCoverCacheBust()"} {
		if !strings.Contains(block, fn) {
			t.Errorf("★ factory reset 必须调 %s：后端连封面一起清了，URL 也要跟着失效", fn)
		}
	}
}
