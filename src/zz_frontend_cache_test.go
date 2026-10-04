package main

// 1.8.18 修复「设置保存不住」的回归测试（当初的受害者是"源文件直出"开关，
// 该开关已在 1.8.22 移除，但"浏览器必须拿到新版 app.js"这条约束依然成立）。
//
// 故障成因：前端资源文件名固定（app.js / style.css / bridge.js），靠 URL 上的
// ?v=<版本> 区分新旧；而 1.8.17 的 index.html 里写死了 ?v=1.8.16（忘了同步），
// 加上响应头是 immutable，浏览器直接命中 1.8.16 的缓存 JS：
//   HTML 是新的 → 用户看得见新加的开关
//   JS 是旧的   → 保存请求里缺字段 → 后端全量覆盖写回默认值
// 表现为"设置保存不住"。
//
// 修法：版本号改为服务端注入（__MV_VERSION__ 占位符），并且文本资源用
// no-cache + 内容哈希 ETag，彻底不依赖"记得改版本号"。
//
//	go test -run TestFrontend -v .

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func fetchFrontend(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", gwPrefix+path, nil)
	w := httptest.NewRecorder()
	(&frontendHandler{}).ServeHTTP(w, req)
	return w
}

// HTML 里的资源版本串必须等于当前 version 常量，且不能残留占位符。
func TestFrontendHTMLVersionInjection(t *testing.T) {
	w := fetchFrontend(t, "/")
	if w.Code != 200 {
		t.Fatalf("index.html 状态码=%d", w.Code)
	}
	body := w.Body.String()

	if strings.Contains(body, versionPlaceholder) {
		t.Errorf("响应的 HTML 里仍残留占位符 %s", versionPlaceholder)
	}
	for _, asset := range []string{
		"style.css?v=" + version,
		"bridge.js?v=" + version,
		"app.js?v=" + version,
	} {
		if !strings.Contains(body, asset) {
			t.Errorf("HTML 里缺少带版本串的资源引用：%s", asset)
		}
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("HTML 的 Cache-Control=%q，期望 no-cache（否则 ?v= 永远更新不了）", cc)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("HTML 缺少 ETag，无法做条件请求")
	}
	// 完整资源串不能出现旧版本
	if regexp.MustCompile(`\?v=1\.8\.1[0-7]\b`).MatchString(body) {
		t.Errorf("HTML 里出现了旧版本资源引用，缓存不会失效")
	}
}

// 源文件里不允许再出现硬编码的 ?v=<数字>：那正是这次事故的成因。
func TestNoHardcodedVersionInWebSource(t *testing.T) {
	for _, name := range []string{"web/index.html", "web/bridge.js"} {
		data, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		hits := regexp.MustCompile(`\?v=[0-9]`).FindAllString(string(data), -1)
		if len(hits) > 0 {
			t.Errorf("%s 里存在硬编码版本号 %v，应改为 %s 占位符", name, hits, versionPlaceholder)
		}
	}
}

// bridge.js 里的 vendor import 也要带版本串（相对 URL 不继承 query）。
func TestFrontendBridgeVendorImportVersioned(t *testing.T) {
	w := fetchFrontend(t, "/bridge.js")
	if w.Code != 200 {
		t.Fatalf("bridge.js 状态码=%d", w.Code)
	}
	body := w.Body.String()
	want := "trimjs-web-app.js?v=" + version
	if !strings.Contains(body, want) {
		t.Errorf("bridge.js 的 vendor import 缺少版本串：期望包含 %s", want)
	}
}

// 文本资源一律 no-cache + ETag，条件请求必须返回 304。
func TestFrontendTextAssetRevalidation(t *testing.T) {
	for _, path := range []string{"/app.js", "/style.css", "/bridge.js"} {
		w1 := fetchFrontend(t, path)
		if w1.Code != 200 {
			t.Fatalf("%s 状态码=%d", path, w1.Code)
		}
		if cc := w1.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s 的 Cache-Control=%q，期望 no-cache", path, cc)
		}
		etag := w1.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s 缺少 ETag", path)
			continue
		}
		if strings.Contains(w1.Body.String(), versionPlaceholder) {
			t.Errorf("%s 里残留占位符", path)
		}

		req := httptest.NewRequest("GET", gwPrefix+path, nil)
		req.Header.Set("If-None-Match", etag)
		w2 := httptest.NewRecorder()
		(&frontendHandler{}).ServeHTTP(w2, req)
		if w2.Code != 304 {
			t.Errorf("%s 带匹配 ETag 应返回 304，实际=%d", path, w2.Code)
		}
	}
}

// 内容变了 ETag 必须变，否则浏览器会拿着旧内容返回 304。
func TestContentETagChangesWithContent(t *testing.T) {
	a := contentETag([]byte("v1.8.18-A"))
	b := contentETag([]byte("v1.8.18-B"))
	if a == b {
		t.Error("不同内容的 ETag 相同，条件请求会误命中旧内容")
	}
	if a != contentETag([]byte("v1.8.18-A")) {
		t.Error("相同内容的 ETag 不稳定")
	}
}

// TestServedAppJSIsCurrent 发出去的 app.js 必须是当前版本：
// 含 1.8.19 引入的放大升级逻辑，且不含 1.8.22 已移除的「源文件直出」开关。
// 前半段防"旧 JS 被缓存",后半段防"开关被改回来又忘了同步别处"。
func TestServedAppJSIsCurrent(t *testing.T) {
	body := fetchFrontend(t, "/app.js").Body.String()
	if !strings.Contains(body, "maybeUpgradeToFull") {
		t.Error("发出去的 app.js 里没有 maybeUpgradeToFull —— 前端仍是旧版")
	}
	for _, s := range []string{"setRawOriginal", "rawOriginal"} {
		if strings.Contains(body, s) {
			t.Errorf("app.js 里仍有已移除的 %q —— 源文件直出开关应彻底删掉", s)
		}
	}
	// HTML 里的开关行也必须消失，否则用户会看到一个点了没反应的复选框
	html := fetchFrontend(t, "/index.html").Body.String()
	if strings.Contains(html, "setRawOriginal") {
		t.Error("index.html 里仍有 setRawOriginal 开关行")
	}
}
