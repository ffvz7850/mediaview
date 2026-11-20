package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 2.4.51 —— 封面重复生成根因修复的行为级回归测试。
//
// 背景（用户现象，历史一直如此）：文件夹封面已全部预生成，但"一键生成后立刻
// 进入路径"仍会重新合成一份；等十几秒再进就正常。
// 根因：coverStillValidCached 把"无效"结论按 jpgPath 缓存 8 秒（coverValidTTL），
// 而写入端 saveCoverPick 写完新 meta 后不清这条缓存 —— 紧随的浏览命中 stale=false
// 把刚生成的封面判死重做。TTL 到期自愈，故表现为时序窗口。
//
// 这组测试把断言从"TTL 函数语义"（zzz_2424，那是正确的底层行为，保留）下沉到
// "写入端作废旧结论"这一真正的修复点，并做端到端验证。

// Test2451SaveCoverPickInvalidatesStaleValidity 是主因的直接回归：
// 塞一条 stale=false → saveCoverPick 写新 meta → 断言旧结论被清 + 立刻读取命中。
func Test2451SaveCoverPickInvalidatesStaleValidity(t *testing.T) {
	coverValidCache = sync.Map{}
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "cover.json")
	jpgPath := coverJPGOf(metaPath)

	// 合法的 pick：V/Dir 匹配、无 Checks/Items ⇒ coverStillValid 返回 true。
	p := &coverPick{V: coverCacheVersion, Dir: dir}

	// 模拟"早先浏览时缓存了一条无效结论"，且仍在 8 秒 TTL 内。
	coverValidCache.Store(jpgPath, coverValidEntry{valid: false, at: time.Now()})
	if v, ok := coverValidCache.Load(jpgPath); !ok || v.(coverValidEntry).valid {
		t.Fatal("前置条件失败：stale=false 未写入 TTL 缓存")
	}

	// 写入端：一键生成完成后写新 meta。
	saveCoverPick(metaPath, p)

	// 核心断言：写入端必须作废这条 stale 结论，否则紧随的浏览会误判失效。
	if v, ok := coverValidCache.Load(jpgPath); ok {
		t.Fatalf("saveCoverPick 后 stale 结论必须被清除，实际仍在：%+v", v)
	}

	// 端到端：紧接着读取，应直接命中新 meta（coverStillValidCached 重算为 true），
	// 不再落入 singleflight buildCover 重新合成。
	if got := loadCoverPick(metaPath, dir); got == nil {
		t.Fatal("saveCoverPick 后立刻 loadCoverPick 应命中新 meta，不应判失效重做")
	}
}

// Test2451StaleFalseWouldBreakBrowseWithoutInvalidation 是反向语义确认：
// 证明"stale=false + 有效 meta"在**不清缓存**时确实会误判失效——即修复针对的
// 是真 bug，不是假想。这样若有人把 saveCoverPick 的 Delete 去掉，上一条测试
// 会变红；本条则锁死"这个 bug 过去确实存在"。
func Test2451StaleFalseWouldBreakBrowseWithoutInvalidation(t *testing.T) {
	coverValidCache = sync.Map{}
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "cover.json")
	jpgPath := coverJPGOf(metaPath)

	p := &coverPick{V: coverCacheVersion, Dir: dir}
	saveCoverPick(metaPath, p) // 先写好有效 meta（这一步会清缓存）

	// 人为再塞回一条 stale=false，模拟旧版"写 meta 没清缓存"的状态。
	coverValidCache.Store(jpgPath, coverValidEntry{valid: false, at: time.Now()})

	if got := loadCoverPick(metaPath, dir); got != nil {
		t.Fatal("stale=false 在 TTL 内应让 loadCoverPick 判失效（证明 bug 机制真实存在）")
	}
}

// Test2451DeleteCoversBothWritePaths 是静态闸门：确认 Delete 在 xattr 成功分支
// 的 return 之前（函数级统一删，两条写路径都覆盖），否则非 Linux 本机测不到
// xattr 成功分支漏清的回归。key 也必须取 coverJPGOf(metaPath)，与读取端同口径。
func Test2451DeleteCoversBothWritePaths(t *testing.T) {
	src := readSourceOrSkip(t, "dircov.go")
	start := strings.Index(src, "func saveCoverPick(")
	if start < 0 {
		t.Fatal("找不到 saveCoverPick")
	}
	// 截取到下一个顶层 func 之前
	seg := src[start:]
	if end := strings.Index(seg, "\nfunc "); end > 0 {
		seg = seg[:end]
	}
	deleteAt := strings.Index(seg, "coverValidCache.Delete(jpgPath)")
	if deleteAt < 0 {
		t.Fatal("saveCoverPick 缺少 coverValidCache.Delete(jpgPath)")
	}
	if !strings.Contains(seg[:deleteAt], "jpgPath := coverJPGOf(metaPath)") {
		t.Error("Delete 的 key 必须来自 coverJPGOf(metaPath)，与读取端同口径")
	}
	// xattr 成功分支（coverMetaSet）必须在 Delete 之后，确保该分支也清了缓存
	xattrAt := strings.Index(seg, "coverMetaSet(jpgPath")
	if xattrAt >= 0 && xattrAt < deleteAt {
		t.Error("coverMetaSet 在 Delete 之前：xattr 成功分支会漏清 TTL 缓存")
	}
}
