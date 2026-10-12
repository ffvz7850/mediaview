package main

// 2.0.0：后台预生成改由**前端重排后**回传有序 path 驱动（POST /api/pregen），
// 取代原先挂在 /api/list 上的自动预生成。
//
// 根因：/api/list 的顺序是后端**字节序**（naturalLess，汉字排最后），而前端实际显示顺序
// 是 Intl.Collator('zh') 的 ICU 序（数字 < 汉字 < 拉丁）。实测「X:\测试排序」129 个文件里
// 两者有 121 个位置不一致 —— 挂在 list 上等于把绝大部分算力花在用户**最后**才看到的
// 文件上，首屏缩略图反而更慢，而大图浏览根本不用列表缩略图。
//
// 本文件覆盖：
//   ① 行为：/api/pregen 顺序**原样保留**、非媒体被丢弃、manual 绕过后台预生成开关；
//   ② 契约：/api/list 不再自动预生成、路由已注册、前端两个入口接上、加载提示存在。

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// ---- ① 行为 ----

// Test2000HandlePregenPreservesOrderExec 顺序必须**原样保留**（后端不重排），非媒体被丢弃。
func Test2000HandlePregenPreservesOrderExec(t *testing.T) {
	q := installThumbQueueSpy(t, 64)
	withPreload(t, 2, true) // 自动通道开（PreloadConcurrency=2）

	// 故意用**非有序**输入：若实现偷排或空转，这里会暴露。
	paths := []string{"/d/m.jpg", "/d/b.jpg", "/d/x.txt", "/d/a.png", "/d/v.mp4"}
	body, _ := json.Marshal(map[string]any{"paths": paths, "manual": false})
	rec := httptest.NewRecorder()
	handlePregen(rec, httptest.NewRequest("POST", "/api/pregen", bytes.NewReader(body)))

	var got []string
	for len(q) > 0 {
		got = append(got, filepath.Base((<-q).path))
	}
	want := []string{"m.jpg", "b.jpg", "a.png", "v.mp4"} // x.txt 被丢弃；顺序保持
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("/api/pregen 没有原样保留顺序：got=%v want=%v", got, want)
	}
	if rec.Code != 200 {
		t.Fatalf("状态码 = %d", rec.Code)
	}
}

// Test2000HandlePregenManualBypassesSwitch：PreloadConcurrency=0 时自动通道投 0 个，
// 但 manual=true 仍必须入队（「立即生成」是用户即时意图，不该被后台预生成开关挡住）。
// 断言用**响应里的 enqueued**：它由投递那一刻算出，不受 worker 何时消费队列影响。
func Test2000HandlePregenManualBypassesSwitch(t *testing.T) {
	installThumbQueueSpy(t, 8)
	withPreload(t, 0, true) // 后台预生成关闭，但缩略图总开关开

	autoRec := httptest.NewRecorder()
	autoBody, _ := json.Marshal(map[string]any{"paths": []string{"/d/a.jpg"}, "manual": false})
	handlePregen(autoRec, httptest.NewRequest("POST", "/api/pregen", bytes.NewReader(autoBody)))
	if n := pregenEnqueued(t, autoRec); n != 0 {
		t.Fatalf("PreloadConcurrency=0 时自动通道不该入队，实际 enqueued=%d", n)
	}

	manRec := httptest.NewRecorder()
	manBody, _ := json.Marshal(map[string]any{"paths": []string{"/d/a.jpg"}, "manual": true})
	handlePregen(manRec, httptest.NewRequest("POST", "/api/pregen", bytes.NewReader(manBody)))
	if n := pregenEnqueued(t, manRec); n != 1 {
		t.Fatalf("manual=true 应绕过后台预生成开关，实际 enqueued=%d", n)
	}
}

func pregenEnqueued(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	var out struct {
		Enqueued int `json:"enqueued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 /api/pregen 响应失败：%v（body=%s）", err, rec.Body.String())
	}
	return out.Enqueued
}

// ---- ② 契约（源码级）----

// Test2000ListNoLongerAutoPreloads：handleList 里不得再出现 preloadBatch*（自动预生成已挪走）。
func Test2000ListNoLongerAutoPreloads(t *testing.T) {
	med := stripLineComments(readSourceOrSkip(t, "media.go"))
	body, ok := funcBody(med, "func handleList(")
	if !ok {
		t.Fatal("找不到 handleList")
	}
	if strings.Contains(body, "preloadBatch") {
		t.Error("handleList 里又出现了 preloadBatch —— 预生成会退回后端字节序，与显示顺序不一致（2.0.0 已挪到 /api/pregen）")
	}
	// 换目录清队列的能力必须保留（清理要发生在 listDir 之前，见 zzz_1947）
	if !strings.Contains(body, "dropQueuedThumbs()") || !strings.Contains(body, "listDir(") {
		t.Error("handleList 丢了 dropQueuedThumbs / listDir")
	}
}

// Test2000PregenRouteRegistered：main.go 必须把 /api/pregen 路由到 handlePregen。
func Test2000PregenRouteRegistered(t *testing.T) {
	m := readSourceOrSkip(t, "main.go")
	if !strings.Contains(m, `gwPrefix+"/api/pregen", handlePregen`) {
		t.Error("main.go 没有注册 /api/pregen → handlePregen")
	}
	// handlePregen 必须按 manual 分流（自动走 preloadBatch、手动走 preloadBatchManual）
	med := stripLineComments(readSourceOrSkip(t, "media.go"))
	hb, ok := funcBody(med, "func handlePregen(")
	if !ok {
		t.Fatal("找不到 handlePregen")
	}
	for _, want := range []string{"body.Manual", "preloadBatchManual(", "preloadBatch("} {
		if !strings.Contains(hb, want) {
			t.Errorf("handlePregen 缺少 %s —— manual/自动没有分流", want)
		}
	}
}

// Test2000FrontendDrivesPregenOrder：前端必须在**重排之后**用最终顺序驱动预生成；
// 大图（singleMode）不得预生成整目录；旧的 /api/list?preload= 必须消失。
func Test2000FrontendDrivesPregenOrder(t *testing.T) {
	js := stripJSComments(readSourceOrSkip(t, "web/app.js"))

	// loadDir：重排后按 state.files 的顺序回传
	if !strings.Contains(js, "postPregen(state.files)") {
		t.Error("loadDir 没有在重排后调用 postPregen(state.files) —— 预生成顺序仍与显示顺序不一致")
	}
	// 手动「立即生成」走 manual 通道
	if !strings.Contains(js, "postPregen(files, true)") {
		t.Error("「立即生成」没有走 postPregen(files, true)")
	}
	// 旧口径必须彻底消失
	if strings.Contains(js, "&preload=") {
		t.Error("app.js 里还有 /api/list?preload= —— 旧口径会绕过前端顺序")
	}
	// openSingle 不得预生成整目录（大图用 /api/raw，列表缩略图是白耗）。
	// 用**出现次数**而不是抓函数体：app.js 是 2 空格缩进，funcBody 依赖列 0 的 `}`，
	// 对 JS 会一直抓到文件尾（把后面的手动按钮也包进来），判据会失真。
	// 期望恰好 3 处：定义 + loadDir 一处 + 手动一处；多一处（如塞进 openSingle）即失败。
	if c := strings.Count(js, "postPregen("); c != 3 {
		t.Errorf("postPregen( 出现 %d 次，期望恰好 3 次（定义 + loadDir + 手动）—— 多出来的很可能是 openSingle 又在预生成整目录", c)
	}
}

// Test2000ListLoadingHint：大图列表就绪前必须有「正在加载目录…」提示。
func Test2000ListLoadingHint(t *testing.T) {
	html := readSourceOrSkip(t, "web/index.html")
	if !strings.Contains(html, `id="vListHint"`) {
		t.Error("index.html 缺少 #vListHint 元素")
	}
	js := stripJSComments(readSourceOrSkip(t, "web/app.js"))
	if !strings.Contains(js, "function showListHint(") {
		t.Fatal("app.js 缺少 showListHint")
	}
	if !strings.Contains(js, "showListHint(true)") || !strings.Contains(js, "showListHint(false)") {
		t.Error("showListHint 没有被真正调用（打开显示 / 就绪或失败隐藏）")
	}
	// 同样用出现次数（openSingle 打开时显示一次；隐藏有 .then/.catch 两处）
	if c := strings.Count(js, "showListHint(true)"); c != 1 {
		t.Errorf("showListHint(true) 出现 %d 次，期望恰好 1 次（openSingle 打开时显示）", c)
	}
}
