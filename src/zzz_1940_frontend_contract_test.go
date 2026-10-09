package main

import (
	"strings"
	"testing"
)

// TestUpgradeToFullRequestsOriginal 守住「1:1 必须真的去取原图」。
//
// 背景：1.8.138 我把「跳过 4096 中间档」写成了 `_upgradeStage = 2` 再接 `return`，
// 等于**既跳过 4096、又不取原图** —— 点 1:1 后一直停在预览档。
// 前端逻辑没有 Go 测试覆盖，`node --check` 只查语法（`= 2` 完全合法），所以没兜住。
// 这条契约把关键取值钉住。
func TestUpgradeToFullRequestsOriginal(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")

	// ① 1:1 **必须直接取原图**（用户明确要求）。
	//    理由：每换一次源就是一次重绘，大图会肉眼可见地闪；中间档在 1:1 下
	//    只贡献"多闪一次"、不贡献画质（4096 在 8.87x 放大下与 3072 同样糊）。
	//    → 换源次数最少的方案 = 最不闪的方案。
	if !strings.Contains(js, "if (force && v.actual) m._upgradeStage = 1;") {
		t.Error("1:1 没有直接取原图 —— 用户要求「点击直接显示原图」")
	}
	//    也不能用 1.8.138 的写法（设 2 再 return = 既不取 4096 也不取原图）
	if strings.Contains(js, "if (force && v.actual) m._upgradeStage = 2;") {
		t.Error("1:1 处把阶段设成 2 再 return —— 既不取 4096 也不取原图（1.8.138 的原 bug）")
	}
	//    滚轮放大的两级仍需保留（那里 4096 是 1.4x 上采样，确实有效）
	if !strings.Contains(js, "rawURL(item.path, PICK_MAX, item.mtime)") {
		t.Error("找不到 4096 预览档的 URL 构造 —— 滚轮放大的一级没了")
	}
	// ② 取原图的那一支必须存在且用的是不带 maxdim 的 URL
	if !strings.Contains(js, "rawURL(item.path, 0, item.mtime)") {
		t.Error("找不到「取原图」的 URL 构造（rawURL(item.path, 0, ...)）")
	}
	// ③ upgradingToFull 必须由 stage 推出，而不是拿档位比较
	if !strings.Contains(js, "var upgradingToFull = (m._upgradeStage === 1);") {
		t.Error("upgradingToFull 不是由 _upgradeStage 推出的 —— 拿档位比较会在" +
			"「基础档位恰好 = PICK_MAX」时误判")
	}
}

// TestActualScaleLockOnlyWithRealWidth 守住「1:1 的 scale 必须按原图宽度算」。
//
// 背景：1.8.139 之前无条件 `_actualScaleLocked = true`，而点 1:1 那一刻原图还没加载、
// item.w 又常因 omitempty 缺失 → scale 按预览档 3072 算，且锁定后不再修正，
// 表现是「点 1:1 放大的不是原图尺寸，要手动放大再点一次才对」。
func TestActualScaleLockOnlyWithRealWidth(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	if !strings.Contains(js, "v._actualScaleLocked = !!_actW;") {
		t.Error("_actualScaleLocked 不是「仅在拿到原图宽度时才锁定」—— " +
			"无条件锁定会让 1:1 定格在预览档尺寸")
	}
	// 关键不是"有没有 = true"，而是"1:1 那次是否**按真实原图宽度**决定是否锁定"。
	// （ensureItemSize 里也有一处 = true，但那是在 `!v._actualScaleLocked` 前提下、
	//   拿到真实宽度后的修正，是有条件的，不算问题。）
	if !strings.Contains(js, "v._actualScaleLocked = !!_actW;") {
		t.Error("1:1 没有「只在拿到原图宽度时才锁定」—— 尺寸会定格在预览档")
	}
	// 必须存在"打开/切换图片时就预取原图尺寸"的机制（否则第一次点 1:1 拿不到宽度）
	if !strings.Contains(js, "ensureItemSize(") || !strings.Contains(js, "function ensureItemSize") {
		t.Error("没有 ensureItemSize —— 1:1 又会退回用预览图宽度算 scale（二次放大）")
	}
	// apply 里必须保留「未锁定时用真实宽度重算」的兜底
	if !strings.Contains(js, "!v._actualScaleLocked") {
		t.Error("升级回调里没有「未锁定时重算 scale」的兜底")
	}
}

// TestNoRawPreloadMigration 守住「预生成迁移已删除」（用户明确要求）。
func TestNoRawPreloadMigration(t *testing.T) {
	c := stripLineComments(readSourceOrSkip(t, "config.go"))
	if strings.Contains(c, "rawPreload == 3 || rawPreload == 0") {
		t.Error("预生成迁移又回来了 —— 它没有版本守卫，每次 schema 升级都会" +
			"把用户主动关掉的预生成重新打开")
	}
}
