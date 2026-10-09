package main

// 设置持久化回归：完整走一遍
// 前端 body → handleSettingsSave → settingsFile 落盘 → 模拟进程重启 loadSettings → 读回。
//
// 历史：1.8.17 曾经出现「源文件直出开关保存不住」，根因是浏览器命中了旧版 app.js，
// 提交的 body 里缺字段，被后端全量覆盖回默认值。开关本身已在 1.8.22 移除，
// 但"前端提交的字段必须真的落盘、并按同名 JSON key 返回"这条约束对所有设置项都成立，
// 所以这里改用【非默认值】的 viewerAnimation / viewerMode / viewerPreload 继续守着。
//
//	go test -run TestSettingsPersistRoundtrip -v .

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSettingsPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEDIAVIEW_ETC", dir)

	// 这个用例会把全局 settings / settingsFile 指到临时目录（模拟一次真实保存），
	// 结束后必须还原，否则会污染同批次里的其它用例。
	prevFile := settingsFile
	prevSettings := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsFile = prevFile
		settings = prevSettings
		settingsMu.Unlock()
	})

	loadSettings()
	t.Logf("settingsFile = %s", settingsFile)

	// 模拟前端 saveSettings() 提交的完整 body（与 web/app.js 字段一一对应）。
	// 三个 viewer* 字段全部取非默认值，这样"被覆盖回默认"一定会被抓到。
	body := `{
		"thumbEnabled": true,
		"thumbDir": "",
		"thumbSize": 320,
		"thumbQuality": 80,
		"takeoverSystemThumb": false,
		"cpuCores": 0,
		"thumbConcurrency": 6,
		"preloadConcurrency": 3,
		"gpuDecode": true,
		"viewerPreload": 4,
		"viewerAnimation": "fade",
		"viewerMode": "overlay"
	}`

	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleSettingsSave(w, req)

	t.Logf("POST 状态=%d", w.Code)
	if w.Code != 200 {
		t.Fatalf("保存失败: %s", w.Body.String())
	}

	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	t.Logf("--- 落盘内容 ---\n%s", string(raw))

	for _, key := range []string{`"viewerAnimation": "fade"`, `"viewerMode": "overlay"`, `"viewerPreload": 4`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("落盘文件里缺少 %s", key)
		}
	}
	got := getSettings()
	if got.ViewerAnimation != "fade" || got.ViewerMode != "overlay" || got.ViewerPreload != 4 {
		t.Errorf("保存后内存里的值不对：anim=%q mode=%q preload=%d",
			got.ViewerAnimation, got.ViewerMode, got.ViewerPreload)
	}

	// 模拟进程重启：内存清空，重新从文件加载
	settingsMu.Lock()
	settings = defaultSettings()
	settingsMu.Unlock()

	loadSettings()

	got = getSettings()
	if got.ViewerAnimation != "fade" || got.ViewerMode != "overlay" || got.ViewerPreload != 4 {
		t.Errorf("重启读回后值丢了：anim=%q mode=%q preload=%d（应 fade/overlay/4）",
			got.ViewerAnimation, got.ViewerMode, got.ViewerPreload)
	} else {
		t.Log("重启读回 OK：fade / overlay / 4")
	}

	// 复现用户现象：再走一次 GET，看返回给前端的字段
	view := buildSettingsView()

	// 关键：前端读的是 JSON 里的字段名，必须验证序列化结果（结构体访问测不出来）
	js, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	t.Logf("GET /api/settings 实际 JSON：\n%s", string(js))

	var m map[string]any
	if err := json.Unmarshal(js, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if v, ok := m["viewerAnimation"]; !ok {
		t.Errorf("JSON 顶层没有 viewerAnimation 键！前端 s.viewerAnimation 会永远是 undefined → 设置被重置")
	} else if v != "fade" {
		t.Errorf("JSON 里 viewerAnimation = %v（应为 fade）", v)
	}
	if v, ok := m["viewerMode"]; !ok {
		t.Errorf("JSON 顶层没有 viewerMode 键！")
	} else if v != "overlay" {
		t.Errorf("JSON 里 viewerMode = %v（应为 overlay）", v)
	}
	// 已移除的开关不应该再出现在 JSON 里，否则老前端/文档会继续引用它
	if _, ok := m["rawOriginal"]; ok {
		t.Errorf("JSON 里仍有已移除的 rawOriginal 字段，应彻底删掉")
	}
	if _, ok := m["raw_original"]; ok {
		t.Errorf("/api/health 与设置里的 raw_original 字段应已移除")
	}
}
