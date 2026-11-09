package main

// 2.4.24/2.4.25/2.4.26 的回归测试：
//
//	① 缝色退出缓存键：coverCacheBase 只由 dir + size 决定，不再含主题；
//	   2.4.26 起缝色固定为 coverSeamColor，composeFromImages 不再带 bg 参数；
//	② 封面有效性校验结论加 8 秒 TTL（省掉命中路径 36 次 stat）。
//
// 以及一件"删代码"的静态闸门：为升级写的迁移/兼容代码（migrateCoverMeta、
// isLegacyFlatCacheFile、Settings.SchemaVersion）在全新安装语义下必须真的不存在。

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- ① 缓存键与主题解耦 ----------

func Test2424CoverKeyIndependentOfTheme(t *testing.T) {
	dir := t.TempDir()
	thumbRootMu.Lock()
	thumbRoot = dir
	thumbRootMu.Unlock()
	t.Cleanup(func() {
		thumbRootMu.Lock()
		thumbRoot = ""
		thumbRootMu.Unlock()
	})

	a := coverCacheBase("/vol1/1000/照片", 320)
	b := coverCacheBase("/vol1/1000/照片", 320)
	if a != b {
		t.Fatalf("同一目录同一尺寸必须得同一缓存键：%q vs %q", a, b)
	}
	if strings.Contains(a, "bg") || strings.Contains(a, "theme") {
		t.Errorf("★ 缓存键里不能出现主题/缝色成分：%q", a)
	}

	// 不同 size → 不同文件（文件名带尺寸）
	c := coverCacheBase("/vol1/1000/照片", 640)
	if a == c {
		t.Error("不同尺寸应落到不同缓存文件")
	}
	// 不同 dir → 不同分片
	d := coverCacheBase("/vol1/1000/视频", 320)
	if a == d {
		t.Error("不同目录应落到不同缓存文件")
	}

	// 签名级闸门：缓存键必须不含 bg，composeFromImages 也不该再有 bg 参数
	// （2.4.26 起缝色固定 coverSeamColor，主题/请求参数与缝色彻底无关）
	src := readSourceOrSkip(t, "dircov.go")
	if !strings.Contains(src, "func coverCacheBase(dir string, size int)") {
		t.Error("★ coverCacheBase 签名必须是 (dir, size)，不能带回 bg")
	}
	if strings.Contains(src, "func composeFromImages(srcs []image.Image, side int, bg") {
		t.Error("★ composeFromImages 不应再有 bg 参数（缝色固定 coverSeamColor）")
	}
}

// ---------- ② 缝色 = 固定色（断言见 zzz_2426） ----------

// ---------- ④ 有效性校验的 TTL ----------

func Test2424ValidityTTLCachesResult(t *testing.T) {
	coverValidCache = sync.Map{}

	key := "/tmp/cover_ttl_test.jpg"

	// 先缓存一个「无效」的结论（模拟刚被目录变化判为失效）
	coverValidCache.Store(key, coverValidEntry{valid: false, at: time.Now()})
	// TTL 内：直接复用缓存的 false，即使真实校验此刻会返回 true（p 结构合法）
	p := &coverPick{V: coverCacheVersion, Dir: "/x", Items: []coverItem{}}
	if coverStillValidCached(p, "/x", key) {
		t.Error("TTL 内应复用缓存的 false 结论")
	}

	// 让缓存过期 → 重新计算（这次是合法 p，返回 true）
	coverValidCache.Store(key, coverValidEntry{valid: false, at: time.Now().Add(-2 * coverValidTTL)})
	if !coverStillValidCached(p, "/x", key) {
		t.Error("缓存过期后应重新计算，得到真实结果 true")
	}

	// 再验一次 TTL 内的正向缓存：真实结果是 false（版本不符），但缓存了 true 就该返回 true
	coverValidCache.Store(key, coverValidEntry{valid: true, at: time.Now()})
	badP := &coverPick{V: coverCacheVersion - 1, Dir: "/x", Items: []coverItem{}}
	if !coverStillValidCached(badP, "/x", key) {
		t.Error("TTL 内应复用缓存的 true 结论（哪怕此刻真实校验会判 false）")
	}
}

// ---------- ⑤ 删兼容代码的静态闸门 ----------

func Test2424NoUpgradeShims(t *testing.T) {
	dircov := readSourceOrSkip(t, "dircov.go")
	if strings.Contains(dircov, "func migrateCoverMeta") {
		t.Error("★ migrateCoverMeta 应已删除（全新安装不需要存量 .json 迁移）")
	}
	if strings.Contains(dircov, "惰性迁移") || strings.Contains(dircov, "懒迁移") {
		t.Error("★ loadCoverPick 里的惰性迁移应已删除")
	}

	cfg := readSourceOrSkip(t, "config.go")
	if strings.Contains(cfg, "SchemaVersion") || strings.Contains(cfg, "currentSettingsSchema") {
		t.Error("★ settings schema 迁移应已删除（全新安装不需要旧默认值迁移）")
	}
	if strings.Contains(cfg, "func isLegacyFlatCacheFile") {
		t.Error("★ 旧版平铺缓存兼容应已删除")
	}
}
