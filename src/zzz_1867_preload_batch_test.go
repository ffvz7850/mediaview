package main

// 1.8.67：后台预生成投递收成**唯一入口** preloadBatch，并把配置迁移的覆盖面补全。
//
// 起因（用户原话）：「handleList 还投了 12 个后台预生成任务，想关的是这个」。
// 那一段循环在 1.8.66 里其实已经被 enqueueThumb 的早退拦住了，但它拦在**被调用的函数内部**：
// 调用点仍然是字面上的「循环 12 次 enqueueThumb」，读代码的人无法确认到底关没关；
// 而且只要以后有人新加一个调用点，或者把早退挪走，就会悄无声息地复现。
// 本版把开关判断提到唯一的批处理入口 preloadBatch，并让 /api/health 报告
// preload_enqueued 计数 —— 用户可以自己看到这个数不动，比读代码可信。
//
// 第二个漏点是配置迁移：1.8.66 只在「并发 6 且 预生成 3」同时命中时才迁移，
// 于是「改过并发数、没碰过预生成」的配置升级后预生成仍是 3，表现同样是「关不掉」。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// installThumbQueueSpy 把全局后台队列换成测试自己的 channel，用于精确观测投了几条。
func installThumbQueueSpy(t *testing.T, capN int) chan thumbReq {
	t.Helper()
	thumbWorkerMu.Lock()
	oldQ := thumbQueue
	q := make(chan thumbReq, capN)
	thumbQueue = q
	thumbWorkerMu.Unlock()
	t.Cleanup(func() {
		thumbWorkerMu.Lock()
		thumbQueue = oldQ
		thumbWorkerMu.Unlock()
	})
	return q
}

// withPreload 临时改「预生成开关 + 缩略图总开关」，结束后还原。
func withPreload(t *testing.T, preload int, enabled bool) {
	t.Helper()
	settingsMu.Lock()
	oldPC := settings.PreloadConcurrency
	oldEn := settings.ThumbEnabled
	settings.PreloadConcurrency = preload
	settings.ThumbEnabled = enabled
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.PreloadConcurrency = oldPC
		settings.ThumbEnabled = oldEn
		settingsMu.Unlock()
	})
}

func sampleItems(n int) []FileItem {
	out := make([]FileItem, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, FileItem{
			Name: fmt.Sprintf("zz_1867_%03d.jpg", i),
			Path: fmt.Sprintf("zz_1867_%03d.jpg", i),
			Kind: "image",
		})
	}
	return out
}

// funcBody 取出函数体：从 sig 起，到第一个行首的 "}" 结束。
func funcBody(src, sig string) (string, bool) {
	i := strings.Index(src, sig)
	if i < 0 {
		return "", false
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j], true
	}
	return rest, true
}

// ---- 关掉预生成 = 一条都不投 ----

func Test1867PreloadBatchEnqueuesNothingWhenPreloadOff(t *testing.T) {
	q := installThumbQueueSpy(t, 64)
	withPreload(t, 0, true)
	before := atomic.LoadInt64(&preloadEnqueuedCount)

	// 12 就是 handleList 那一批的上限；这里故意给 30 个文件。
	n := preloadBatch(sampleItems(30), 12)

	if n != 0 {
		t.Errorf("预生成已关闭，preloadBatch 却报告投了 %d 个", n)
	}
	if got := len(q); got != 0 {
		t.Errorf("预生成已关闭，后台队列却收到 %d 条 —— 进目录的那 12 个还在投", got)
	}
	if after := atomic.LoadInt64(&preloadEnqueuedCount); after != before {
		t.Errorf("preload_enqueued 从 %d 涨到 %d —— 关掉预生成后这个数必须不动",
			before, after)
	}
}

// 正对照：同一个队列、同一个函数，把开关打开必须真的投进去。
// 没有这一步，「投了 0 条」既可能是真早退，也可能是队列压根没接上 —— 两者无法区分。
func Test1867PreloadBatchEnqueuesUpToLimitWhenOn(t *testing.T) {
	q := installThumbQueueSpy(t, 64)
	withPreload(t, 2, true)
	before := atomic.LoadInt64(&preloadEnqueuedCount)

	if n := preloadBatch(sampleItems(30), 12); n != 12 {
		t.Errorf("打开预生成后，30 个文件、上限 12：投了 %d 个，期望 12", n)
	}
	if got := len(q); got != 12 {
		t.Errorf("队列里有 %d 条，期望 12", got)
	}
	if d := atomic.LoadInt64(&preloadEnqueuedCount) - before; d != 12 {
		t.Errorf("preload_enqueued 增加 %d，期望 12", d)
	}

	// 文件比上限少时按文件数投（/api/scan 的 5 就是这种场景）
	for len(q) > 0 {
		<-q
	}
	if n := preloadBatch(sampleItems(5), 50); n != 5 {
		t.Errorf("5 个文件、上限 50：投了 %d 个，期望 5", n)
	}

	// 缩略图总开关关掉时同样一条都不投
	for len(q) > 0 {
		<-q
	}
	withPreload(t, 2, false)
	if n := preloadBatch(sampleItems(5), 12); n != 0 {
		t.Errorf("ThumbEnabled=false 时投了 %d 个，期望 0", n)
	}
	if got := len(q); got != 0 {
		t.Errorf("ThumbEnabled=false 时队列收到 %d 条，期望 0", got)
	}
}

// ---- 投递入口唯一化（源码级）----

func Test1867PreloadBatchIsTheOnlyEnqueueEntrypoint(t *testing.T) {
	med := stripLineComments(readSourceOrSkip(t, "media.go"))
	th := stripLineComments(readSourceOrSkip(t, "thumb.go"))

	// media.go 里不允许再出现裸 enqueueThumb：handleList / handleScan 必须走 preloadBatch。
	// 否则「关掉预生成」又要在每个入口各判一次，漏一个就前功尽弃。
	if c := strings.Count(med, "enqueueThumb("); c != 0 {
		t.Errorf("media.go 里还有 %d 处直接调用 enqueueThumb —— 预生成投递没有统一入口", c)
	}
	// thumb.go 里只允许 preloadBatch 内部那一处调用（`func enqueueThumb(` 是定义，不算调用）
	calls := strings.Count(th, "enqueueThumb(") - strings.Count(th, "func enqueueThumb(")
	if calls != 1 {
		t.Errorf("thumb.go 里 enqueueThumb 有 %d 处调用，期望恰好 1 处（preloadBatch 内部）", calls)
	}

	// 四处批量投递点各有归属：
	//   · 自动路径（进目录 / 扫描）走 preloadBatch —— 受「后台预生成」开关约束，这是对的；
	//   · 手动路径（?preload=N 手动生成 / 清完缓存后重建）走 preloadBatchManual ——
	//     它们是**用户刚点了按钮**的即时动作，不该被那个开关挡住
	//     （1.8.164：清缓存重建原先误用 preloadBatch，PreloadConcurrency=0 时投 0 个，
	//      与本 handler「立即触发重新生成」的承诺矛盾）。
	for _, want := range []string{
		"preloadBatch(resp.Files, len(resp.Files))", // /api/list 进入目录：整目录按序入队（自动）
		"preloadBatchManual(resp.Files, preloadN)",  // ?preload=N 手动生成（手动）
		"preloadBatch(files, 5)",                    // /api/scan 递归扫描（自动）
		"preloadBatchManual(resp.Files, 50)",        // 清完缓存重建（手动）
	} {
		if !strings.Contains(med+th, want) {
			t.Errorf("缺少预期调用点：%s", want)
		}
	}

	// 开关判断必须在投递循环**之前** —— 判据写在循环后面等于没拦。
	body, ok := funcBody(th, "func preloadBatch(")
	if !ok {
		t.Fatal("thumb.go 里找不到 preloadBatch")
	}
	iGuard := strings.Index(body, "PreloadConcurrency <= 0")
	iLoop := strings.Index(body, "for i := range files")
	if iGuard < 0 {
		t.Fatal("preloadBatch 里没有 PreloadConcurrency<=0 的开关判断 —— 唯一入口形同虚设")
	}
	if iLoop < 0 {
		t.Fatal("preloadBatch 里没有投递循环")
	}
	if iGuard > iLoop {
		t.Error("preloadBatch 的开关判断在投递循环之后 —— 等投完再判，等于没拦")
	}
	if !strings.Contains(body, "return 0") {
		t.Error("preloadBatch 关掉时必须返回 0（调用方靠它判断投了几个）")
	}
}

// ---- 配置迁移覆盖面 ----

func Test1867MigrationUpgradesEachLegacyDefaultIndependently(t *testing.T) {
	cases := []struct {
		rawTC, rawPC   int
		ver            int
		wantTC, wantPC int
		desc           string
	}{
		{6, 3, 0, thumbConcurrencyDefault(), 3, "旧默认组合 → 并发按核数 / 预生成保持 3"},
		{4, 3, 0, 4, 3, "★调过并发数(4)、没碰预生成(旧默认3) → 预生成保持原值"},
		{3, 3, 0, 3, 3, "并发已是 3、预生成仍是旧默认 3 → 预生成保持原值"},
		{2, 3, 0, 2, 3, "★并发 2（normalize 会把预生成钳到 1）→ 判据必须看原值 3，仍要关掉"},
		{6, 5, 0, thumbConcurrencyDefault(), 5, "预生成被主动调到 5：只升并发，不覆盖用户选择"},
		{8, 8, 0, 8, 8, "两个都不是旧默认：都不动"},
		{3, 0, 1, 3, 0, "旧迁移产物 0：已删除迁移，保持 0（用户关掉的不会再被打开）"},
		{6, 3, currentSettingsSchema, 6, 3, "已是当前版本：用户手动设回 6/3 必须被尊重"},
	}
	for _, c := range cases {
		s := Settings{
			ThumbConcurrency:   c.rawTC,
			PreloadConcurrency: c.rawPC,
			SchemaVersion:      c.ver,
		}
		s.migrate(c.rawTC, c.rawPC)
		if s.ThumbConcurrency != c.wantTC || s.PreloadConcurrency != c.wantPC {
			t.Errorf("%s：得到 %d/%d，期望 %d/%d",
				c.desc, s.ThumbConcurrency, s.PreloadConcurrency, c.wantTC, c.wantPC)
		}
		if s.SchemaVersion != currentSettingsSchema {
			t.Errorf("%s：迁移后没有打上版本号（会每次启动重判）", c.desc)
		}
	}
}

// 端到端走一遍真实加载路径：磁盘上的旧配置 → loadSettings → 落盘。
// 这里刻意用「并发 2 + 预生成 3」：normalize 的交叉钳制会把预生成 3 钳成 1，
// 所以断言值必须精确等于 0 —— 得到 1 就说明迁移读的是钳制后的值（等于没修）。
func Test1867LoadSettingsMigratesLegacyPreloadEvenWhenThumbIsCustom(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEDIAVIEW_ETC", dir)

	prevFile := settingsFile
	prevSettings := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsFile = prevFile
		settings = prevSettings
		settingsMu.Unlock()
	})

	path := filepath.Join(dir, "mediaview.settings.json")
	raw, _ := json.Marshal(map[string]any{
		"thumbEnabled":       true,
		"thumbConcurrency":   2, // 用户改过
		"preloadConcurrency": 3, // 更早版本的固定默认，没碰过 → schema 3 应升为新默认
	})
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	loadSettings()

	s := getSettings()
	// 1.8.139 起不再迁移 preload。本例磁盘上是 preload=3 且 thumbConcurrency=2，
	// 于是 normalize 的交叉钳制把预生成钳到 maxBg = ThumbConcurrency-1 = 1。
	// （原来的期望是 0 —— 那是旧迁移规则写的；现在只受钳制，不再被改写成"关闭"。）
	if s.PreloadConcurrency != 1 {
		t.Errorf("preloadConcurrency = %d，期望 1（已删迁移；上限由 ThumbConcurrency-1 决定）",
			s.PreloadConcurrency)
	}
	if s.SchemaVersion != currentSettingsSchema {
		t.Errorf("SchemaVersion = %d，期望 %d", s.SchemaVersion, currentSettingsSchema)
	}

	// 迁移结果必须落盘，否则每次启动都重判一遍
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Settings
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back.PreloadConcurrency != 1 || back.SchemaVersion != currentSettingsSchema {
		t.Errorf("落盘值不对：文件里 preloadConcurrency=%d（期望 1）schemaVersion=%d",
			back.PreloadConcurrency, back.SchemaVersion)
	}
}

// preloadDefaultForTest 取新装默认的预生成并发（schema 3 起默认开启）。
func preloadDefaultForTest() int { return defaultSettings().PreloadConcurrency }
