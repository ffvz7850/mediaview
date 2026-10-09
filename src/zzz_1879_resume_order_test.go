package main

import (
	"os"
	"strings"
	"testing"
)

// 1.8.175：closeViewer 必须先"让后端恢复"，再"重发缩略图"。
//
// 症状：点左上角序号返回网格后，缩略图**全都变成空白**。
// 原因：原来 closeViewer() 一进来就同步 reloadVisibleThumbs()，
//
//	而"恢复后台生成"走的是 resumeThumbGen() —— 它延迟 1000ms 才发
//	POST /api/thumb/resume。于是重发的请求打到"仍在暂停"的后端，
//	拿到的是占位图（no-store）；<img> 收到后认为"加载完成"，
//	浏览器不会再自动重发，那一片就永远白着。
//
// 修法：走 resumeThumbGenNow(cb)，在 /thumb/resume 返回之后再重发。
func Test1879ResumeBeforeReload(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "function resumeThumbGenNow(") {
		t.Error("app.js 没有 resumeThumbGenNow —— 无法保证'先恢复、再重发'")
	}
	if !strings.Contains(s, "resumeThumbGenNow(function () { reloadVisibleThumbs(); });") {
		t.Error("closeViewer 没有把 reloadVisibleThumbs 放进 resume 回调 —— " +
			"顺序反了会拿到占位图，缩略图永远停在白图")
	}
	// closeViewer 里不能再出现"同步重发"或"末尾延迟恢复"
	i := strings.Index(s, "function closeViewer()")
	if i < 0 {
		t.Fatal("找不到 closeViewer")
	}
	tail := s[i:]
	if j := strings.Index(tail, "function showCurrent"); j > 0 {
		tail = tail[:j]
	}
	if strings.Contains(tail, "\n    reloadVisibleThumbs();") {
		t.Error("closeViewer 里仍有同步的 reloadVisibleThumbs() —— 会在后端恢复前重发")
	}
	if strings.Contains(tail, "\n    resumeThumbGen();") {
		t.Error("closeViewer 里仍有 resumeThumbGen() —— 它延迟 1 秒，会把已恢复又推回延迟")
	}
}

// Test1879AbortSilenced 预取被主动取消产生的 AbortError 不应冒泡成红字。
func Test1879AbortSilenced(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "unhandledrejection") {
		t.Error("app.js 没有 unhandledrejection 兜底 —— AbortError 会继续报红")
	}
	if !strings.Contains(s, "AbortError") {
		t.Error("兜底里没有识别 AbortError")
	}
}
