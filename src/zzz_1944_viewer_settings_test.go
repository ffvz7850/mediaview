package main

import "testing"

// TestViewerSettingsNormalize 新增四个字段的白名单收敛。
func TestViewerSettingsNormalize(t *testing.T) {
	cases := []struct {
		q, md, vl, tl     int
		wq, wmd, wvl, wtl int
	}{
		{0, 0, 0, 0, 0, 0, 0, 0},             // 默认：全自动
		{-1, 6144, -1, -1, -1, 6144, -1, -1}, // 合法值原样保留
		{95, 4096, 3, 3, 95, 4096, 3, 3},
		{88, 2048, 1, 2, 88, 2048, 1, 2},
		{84, 5120, 0, 1, 84, 5120, 0, 1},
		// 非法值 → 回落
		{77, 1234, 9, -8, 0, 0, 0, 0},
		{92, 3000, 4, 5, 0, 0, 0, 0}, // 92 不在白名单（与 88 等价，前端不给）
	}
	for _, c := range cases {
		s := defaultSettings()
		s.ViewerQuality, s.ViewerMaxdim = c.q, c.md
		s.ViewerLowres, s.ThumbLowresLevel = c.vl, c.tl
		s.normalize()
		if s.ViewerQuality != c.wq || s.ViewerMaxdim != c.wmd ||
			s.ViewerLowres != c.wvl || s.ThumbLowresLevel != c.wtl {
			t.Errorf("normalize(%d,%d,%d,%d) = (%d,%d,%d,%d)，期望 (%d,%d,%d,%d)",
				c.q, c.md, c.vl, c.tl,
				s.ViewerQuality, s.ViewerMaxdim, s.ViewerLowres, s.ThumbLowresLevel,
				c.wq, c.wmd, c.wvl, c.wtl)
		}
	}
}

// TestBigQualityFollowsViewerSetting 大图质量三种分支（这是 1.8.137 钳制留下的坑）。
func TestBigQualityFollowsViewerSetting(t *testing.T) {
	settingsMu.Lock()
	oldQ, oldTQ := settings.ViewerQuality, settings.ThumbQuality
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ViewerQuality, settings.ThumbQuality = oldQ, oldTQ
		settingsMu.Unlock()
	})

	set := func(vq, tq int) int {
		settingsMu.Lock()
		settings.ViewerQuality, settings.ThumbQuality = vq, tq
		settingsMu.Unlock()
		return bigQualityQScale()
	}
	// 自动 → 88 → -q:v 3
	if got := set(0, 80); got != qualityToQScale(88) {
		t.Errorf("自动档 = -q:v %d，期望 %d（=qualityToQScale(88)）", got, qualityToQScale(88))
	}
	// 跟随缩略图质量 → **不钳制**（1.8.137 的 [82,86] 钳制必须消失）
	if got := set(-1, 95); got != qualityToQScale(95) {
		t.Errorf("跟随档把 95 变成了 -q:v %d —— 钳制又回来了（用户设的质量必须生效）", got)
	}
	if got := set(-1, 80); got != qualityToQScale(80) {
		t.Errorf("跟随档把 80 变成了 -q:v %d，期望 %d", got, qualityToQScale(80))
	}
	// 固定值
	if got := set(95, 80); got != qualityToQScale(95) {
		t.Errorf("固定 95 档 = -q:v %d，期望 %d", got, qualityToQScale(95))
	}
}

// TestLowresLevelWrappers 档位覆盖（关闭 / 固定 / 自动）。
func TestLowresLevelWrappers(t *testing.T) {
	settingsMu.Lock()
	oldV, oldT, oldTL := settings.ViewerLowres, settings.ThumbLowres, settings.ThumbLowresLevel
	settingsMu.Unlock()
	t.Cleanup(func() {
		settingsMu.Lock()
		settings.ViewerLowres, settings.ThumbLowres, settings.ThumbLowresLevel = oldV, oldT, oldTL
		settingsMu.Unlock()
	})

	setV := func(v int) { settingsMu.Lock(); settings.ViewerLowres = v; settingsMu.Unlock() }
	// 关闭
	setV(-1)
	if got := lowresForViewer(8736, 11648, 3072); got != 0 {
		t.Errorf("关闭档 = %d，期望 0", got)
	}
	// 固定
	setV(2)
	if got := lowresForViewer(8736, 11648, 6144); got != 2 {
		t.Errorf("固定 2 档 = %d，期望 2", got)
	}
	// 自动 = 公式
	setV(0)
	if got, want := lowresForViewer(8736, 11648, 3072), lowresFor(8736, 11648, 3072); got != want {
		t.Errorf("自动档 = %d，期望公式值 %d", got, want)
	}

	// 缩略图侧：总开关关闭 → 一律 0
	settingsMu.Lock()
	settings.ThumbLowres = false
	settings.ThumbLowresLevel = 2
	settingsMu.Unlock()
	if got := lowresForThumb(8736, 11648, 320); got != 0 {
		t.Errorf("总开关关闭时 = %d，期望 0", got)
	}
	settingsMu.Lock()
	settings.ThumbLowres = true
	settings.ThumbLowresLevel = 3
	settingsMu.Unlock()
	if got := lowresForThumb(8736, 11648, 320); got != 3 {
		t.Errorf("固定 3 档 = %d，期望 3", got)
	}
}
