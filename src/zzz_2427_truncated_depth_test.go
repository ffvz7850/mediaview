package main

// 2.4.27 的回归测试：修「第一层级文件夹封面不生成」的固化 bug。
//
//	根因：需要下探子目录才能抽到图的目录（第一层级大目录），下探撞条目/时间预算
//	半途而废 → 空结果被落盘 + coverStillValid 对空结论直接判有效 → 永久固化。
//
//	修复三件套：
//	  ① coverPick 加 Truncated（撞预算置位）；
//	  ② Truncated 的空结果不落盘（buildCover）、空结论也走 Checks 校验（coverStillValid）；
//	  ③ 下探深度 3→4。
//
// 另有一条"全新安装"语义的静态闸门：不为升级覆盖写兼容逻辑。

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// segmentBetween 取 src 中 start 标记之后到 end 标记之前的一段（找不到 start 返回空）。
func segmentBetween(src, start, end string) string {
	i := strings.Index(src, start)
	if i < 0 {
		return ""
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// makeHugeDir 造一个"第一层级"目录：root 下有 n 个杂项文件（非媒体）+ 一个
// 只装一张图的子目录。root 的 ReadDir 一次就把 coverWalkBudget 吃穿，
// 正是线上固化 bug 的复现场景。
func makeHugeDir(t *testing.T, n int) string {
	t.Helper()
	root := t.TempDir()
	sub := filepath.Join(root, "s")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(sub, "img.jpg"))
	for i := 0; i < n; i++ {
		touch(t, filepath.Join(root, fmt.Sprintf("f%04d.dat", i)))
	}
	return root
}

// ---------- ① collectCoverGroups 的截断标记 ----------

func Test2427CollectTruncatedFlag(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	// 预算充足：小树走完 → 不截断
	root := t.TempDir()
	var subs []string
	for i := 0; i < 3; i++ {
		d := filepath.Join(root, fmt.Sprintf("d%d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		touch(t, filepath.Join(d, "x.jpg"))
		subs = append(subs, d)
	}
	var walked int
	deadline := time.Now().Add(time.Second)
	_, _, trunc := collectCoverGroups(subs, 1000, deadline, rng, &walked, nil)
	if trunc {
		t.Error("小树 + 预算充足不应截断")
	}

	// 预算 2：3 个子目录根本展开不完 → 必须截断
	_, _, trunc = collectCoverGroups(subs, 2, deadline, rng, &walked, nil)
	if !trunc {
		t.Error("★ 预算耗尽且队列非空时必须置 Truncated（否则空结论会被固化）")
	}

	// 预算 0：一个都没展开 → 也是截断（下探没走完）
	_, _, trunc = collectCoverGroups(subs, 0, deadline, rng, &walked, nil)
	if !trunc {
		t.Error("预算为 0 且还有子目录待展开应置 Truncated")
	}
}

// ---------- ①b 根目录 ReadDir 吃穿预算 → pick 也标截断 ----------

func Test2427PickRootBudgetExhausted(t *testing.T) {
	setCoverSeed(t, 42)

	root := makeHugeDir(t, 900)
	res := pickCover(root, coverMaxItems)
	if !res.Truncated {
		t.Error("★ 根目录条目吃穿预算、下探没开始时必须置 Truncated")
	}
	if len(res.Items) != 0 {
		t.Errorf("没下探就不该有 Items，got %d", len(res.Items))
	}
}

// ---------- ②a Truncated 的空结果不落盘 ----------

func Test2427TruncatedEmptyNotSaved(t *testing.T) {
	setCoverSeed(t, 42)

	// 撞预算的"空"：不落盘（下次访问自动重抽，逐步收敛出封面）
	root := makeHugeDir(t, 900)
	meta := filepath.Join(t.TempDir(), "h_320.json")
	if _, _, err := buildCover(root, 320, coverJPGOf(meta), meta, false); err != nil {
		t.Fatalf("buildCover: %v", err)
	}
	if _, err := os.Stat(meta); err == nil {
		t.Error("★ Truncated 的空结论绝不能落盘（落盘=固化 bug 回归）")
	}

	// 对照：真·空目录（0 条目，没撞预算）的"空"照旧落盘 —— 真空目录不重复白扫
	empty := t.TempDir()
	meta2 := filepath.Join(t.TempDir(), "e_320.json")
	if _, _, err := buildCover(empty, 320, coverJPGOf(meta2), meta2, false); err != nil {
		t.Fatalf("buildCover(empty): %v", err)
	}
	if _, err := os.Stat(meta2); err != nil {
		t.Error("真空目录的空结论应照旧落盘（否则每次进空目录都重新遍历）")
	}
}

// ---------- ②b 空结论也走 Checks 校验（修"加图不刷新"） ----------

func Test2427EmptyPickValidatedByChecks(t *testing.T) {
	dir := t.TempDir()
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &coverPick{
		V:      coverCacheVersion,
		Dir:    dir,
		PickAt: time.Now().UnixMilli(),
		Items:  []coverItem{},
		Checks: []coverCheck{{Path: dir, Mtime: st.ModTime().UnixMilli()}},
	}
	if !coverStillValid(p, dir) {
		t.Fatal("空目录 mtime 未变时，空结论应有效")
	}
	// 目录 mtime 变了（用户往里放了新照片）→ 空结论必须失效重抽
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
	if coverStillValid(p, dir) {
		t.Error("★ 目录 mtime 变了，空结论还判有效 = 固化 bug 回归")
	}
}

// ---------- ③ 下探深度 4 ----------

func Test2427DeepPickLevel4(t *testing.T) {
	setCoverSeed(t, 7)

	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(deep, "img.jpg"))

	res := pickCover(root, coverMaxItems)
	if len(res.Items) != 1 {
		t.Fatalf("第 4 层的图应能被抽到，got %d 张（Items=%v）", len(res.Items), res.Items)
	}
	if res.Depth != 4 {
		t.Errorf("Depth 应为 4，got %d", res.Depth)
	}
	if res.Source != "deep" {
		t.Errorf("Source 应为 deep，got %q", res.Source)
	}
}

// ---------- 静态闸门 ----------

func Test2427StaticGates(t *testing.T) {
	if coverCacheVersion < 6 {
		t.Errorf("coverCacheVersion 应 >= 6（v5 及更早的空 meta 必须全部作废；2.4.28 起为 7），got %d", coverCacheVersion)
	}
	if coverMaxDepth != 4 {
		t.Errorf("coverMaxDepth 应为 4（用户指定），got %d", coverMaxDepth)
	}
	dircov := readSourceOrSkip(t, "dircov.go")
	if !strings.Contains(dircov, "Truncated bool") {
		t.Error("★ coverPick 必须有 Truncated 字段")
	}
	// 旧的"空结论直接判有效"提前返回必须删掉
	if strings.Contains(dircov, "也是有效结论") {
		t.Error("★ coverStillValid 不得再对空结论提前返回 true（固化 bug 根源）")
	}
	// buildCover 的空分支必须检查 Truncated
	seg := segmentBetween(dircov, "if len(p.Items) == 0 {", "// 已有产物")
	if seg == "" {
		t.Fatal("buildCover 里找不到空 Items 分支")
	}
	if !strings.Contains(seg, "!p.Truncated") {
		t.Error("★ buildCover 空分支必须 !p.Truncated 才落盘")
	}
	// collectCoverGroups 必须返回截断标记（三返回值）
	if !strings.Contains(dircov, "func collectCoverGroups(subs []string, budget int, deadline time.Time, rng *rand.Rand, walked *int, rec *coverPick) (groups []coverGroup, depth int, truncated bool)") {
		t.Error("★ collectCoverGroups 签名必须含 truncated 返回值")
	}
}
