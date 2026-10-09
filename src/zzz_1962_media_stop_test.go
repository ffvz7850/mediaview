package main

import (
	"strings"
	"testing"
)

// 1.8.162 契约：视频必须被「显式停掉」，不能指望它自己停。
//
// 背景（真机实测 Chromium）：
//   · 容器 display:none / visibility:hidden 之后 paused **仍为 false** —— 音频与解码都在继续；
//   · 只有「从 DOM 移除」才会被浏览器自动暂停（HTML 规范要求）。
//
// 所以任何「隐藏而不销毁」的路径都是泄漏源。现场表现就是
// 「切换视频后旧视频还在后台出声」+「多个视频叠着播」+「关掉后还在吃 CPU/GPU」。
//
// 这组测试钉住的是「媒体生命周期只有两个出口」这件事：
//   stopMedia  = 停机（元素马上丢弃）→ pause + 断源 + 释放解码器
//   pauseMedia = 暂停（元素还要复用）→ 只 pause，绝不断源

func jsFunc(js, sig string) string {
	i := strings.Index(js, sig)
	if i < 0 {
		return ""
	}
	body := js[i:]
	if j := strings.Index(body, "\n  }"); j > 0 {
		body = body[:j]
	}
	return body
}

// closeViewer 必须「先停机、再销毁」，否则关掉查看器后视频会在后台继续出声。
func Test1962VideosStoppedBeforeViewerDiscard(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	body := jsFunc(js, "function closeViewer()")
	if body == "" {
		t.Fatal("找不到 closeViewer()")
	}

	iStop := strings.Index(body, "stopAllMediaIn(vWrap);")
	iWipe := strings.Index(body, "vWrap.innerHTML = '';")
	if iStop < 0 {
		t.Error("closeViewer 在销毁轨道前没有停掉媒体 —— 关闭查看器后视频会在后台继续出声、继续解码")
	}
	if iWipe < 0 {
		t.Fatal("closeViewer 里找不到 vWrap.innerHTML = '';")
	}
	if iStop > 0 && iWipe > 0 && iStop > iWipe {
		t.Error("closeViewer 里 stopAllMediaIn 排在 vWrap.innerHTML='' 之后 —— 顺序反了")
	}
	if !strings.Contains(body, "v.media = null;") {
		t.Error("closeViewer 没有把 v.media 清空 —— 它会一直指向已销毁的元素")
	}
}

// 媒体停止必须走统一出口；散落的手写 pause 正是当初漏掉 closeViewer 的根因。
func Test1962NoHandRolledMediaPause(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	if strings.Contains(js, "_media.pause()") {
		t.Error("又出现手写的「_media.pause()」 —— 媒体停止必须走 stopMedia/pauseMedia 统一出口，" +
			"散落的 pause 循环正是漏掉 closeViewer 那条路径的原因")
	}
	nStop := strings.Count(js, "stopMedia(")
	nPause := strings.Count(js, "pauseMedia(")
	if nStop < 4 {
		t.Errorf("stopMedia 的调用点过少（%d）—— 疑似有路径又被改回手写 pause", nStop)
	}
	if nPause < 2 {
		t.Errorf("pauseMedia 的调用点过少（%d）—— 它是「保留元素、只暂停」语义的唯一出口", nPause)
	}
}

// stopMedia 里必须「先解绑 onerror、再 load()」，且断源要在 load 之前。
func Test1962StopDetachesOnErrorBeforeLoad(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	body := jsFunc(js, "function stopMedia(")
	if body == "" {
		t.Fatal("找不到 stopMedia()")
	}

	iErr := strings.Index(body, "el.onerror = null;")
	iRm := strings.Index(body, "el.removeAttribute('src');")
	iLoad := strings.Index(body, "el.load();")
	if iErr < 0 || iRm < 0 || iLoad < 0 {
		t.Fatalf("stopMedia 缺少关键步骤（onerror=%d removeAttribute=%d load=%d）", iErr, iRm, iLoad)
	}
	if iErr > iLoad {
		t.Error("stopMedia 在 load() 之后才解绑 onerror —— removeAttribute('src')+load() 会触发一次 error，" +
			"不解绑就会误弹「该编码可能不被浏览器支持」")
	}
	if iRm > iLoad {
		t.Error("stopMedia 先 load() 才 removeAttribute('src') —— 断源必须在 load 之前，否则解码器没被释放")
	}
}

// 查看器窗口名必须固定：带时间戳 = 每次新窗口，旧窗口不复用也不关闭 → 视频叠着播。
func Test1962ViewerWindowNameIsStable(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	i := strings.Index(js, "window.open(pageURL,")
	if i < 0 {
		t.Fatal("找不到 window.open(pageURL, ...)")
	}
	line := js[i:]
	if j := strings.Index(line, "\n"); j > 0 {
		line = line[:j]
	}
	if strings.Contains(line, "Date.now()") {
		t.Error("查看器窗口名又带上了时间戳 —— 每次都是新窗口，旧窗口既不复用也不关闭；" +
			"而旧窗口里的视频只要文档还活着就一直在播（实测隐藏不停播）→ 多个视频叠着出声")
	}
	if !strings.Contains(line, "'mediaview_viewer'") {
		t.Errorf("查看器窗口名不是固定值 'mediaview_viewer'：%s", line)
	}
}

// 页面被隐藏时必须暂停，且**不能断源**（元素还要能继续播）。
func Test1962HiddenPagePausesWithoutDroppingSource(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	if !strings.Contains(js, "addEventListener('visibilitychange'") {
		t.Error("缺少 visibilitychange 兜底 —— 页面被切到后台/被宿主遮住时，视频会一直在后台出声、一直吃 CPU/GPU")
	}
	if !strings.Contains(js, "if (document.hidden) pauseAllMediaIn(vWrap);") {
		t.Error("visibilitychange 没有调用 pauseAllMediaIn —— 或者错用了 stopAllMediaIn" +
			"（那会把 src 断掉，切回前台后那一格会变成空白）")
	}

	body := jsFunc(js, "function pauseMedia(")
	if body == "" {
		t.Fatal("找不到 pauseMedia()")
	}
	if strings.Contains(body, "removeAttribute") {
		t.Error("pauseMedia 里出现了断源 —— 那是「停机」语义；pauseMedia 必须保证元素还能继续播")
	}
	if strings.Contains(body, "load()") {
		t.Error("pauseMedia 里出现了 load() —— 那是「停机」语义，会清掉已缓冲数据")
	}
}
