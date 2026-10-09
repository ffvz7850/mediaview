package main

import (
	"strings"
	"testing"
)

// TestApplySettingsUsesCorrectParamName 守住「applySettings 里必须用 s. 而不是 data.」。
//
// 背景：1.8.144 我加新字段时写成了 `cfg.viewerQuality = data.viewerQuality ...`，
// 而这个函数的参数名是 **s** —— `data` 未定义 → **ReferenceError** →
// 被 openSettings 的 .catch() 捕获 → 设置面板整块显示「读取设置失败，请关闭后重试」。
//
// 这类"拼错变量名"的错误：node --check 查不出（语法合法）、go test 也覆盖不到（那是 JS 运行时），
// 所以用契约测试把参数名前缀钉住。
func TestApplySettingsUsesCorrectParamName(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	i := strings.Index(js, "function applySettings(")
	if i < 0 {
		t.Fatal("找不到 applySettings")
	}
	rest := js[i+10:]
	j := strings.Index(rest, "\n  function ")
	if j < 0 {
		t.Fatal("找不到 applySettings 的结束位置")
	}
	body := rest[:j]

	if strings.Contains(body, "data.") {
		t.Error("applySettings 里出现了 `data.` —— 该函数参数名是 **s**，" +
			"`data` 未定义会抛 ReferenceError，表现为设置面板显示「读取设置失败」" +
			"（1.8.144 就是这样坏的）")
	}
	// 新增的四个字段必须在里面，且都通过 s. 读取
	for _, f := range []string{"s.viewerQuality", "s.viewerMaxdim", "s.viewerLowres", "s.thumbLowresLevel"} {
		if !strings.Contains(body, f) {
			t.Errorf("applySettings 里没有读取 %s —— 设置面板回填会丢值", f)
		}
	}
}

// TestFillSettingsFormCoversNewFields 回填必须覆盖四个新字段（否则界面显示旧值）。
func TestFillSettingsFormCoversNewFields(t *testing.T) {
	js := readSourceOrSkip(t, "web/app.js")
	i := strings.Index(js, "function fillSettingsForm(")
	if i < 0 {
		t.Fatal("找不到 fillSettingsForm")
	}
	rest := js[i+10:]
	j := strings.Index(rest, "\n  function ")
	body := rest
	if j > 0 {
		body = rest[:j]
	}
	for _, id := range []string{"setViewerQuality", "setViewerMaxdim", "setViewerLowres", "setThumbLowresLevel"} {
		if !strings.Contains(body, id) {
			t.Errorf("fillSettingsForm 没有回填 %s", id)
		}
	}
}
