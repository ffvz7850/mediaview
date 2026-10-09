package main

// 1.8.39（P0+P1 修复）的回归测试：守住这轮安全与正确性修复不被回退。
import (
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0：/api/raw 必须有类型闸门。白名单是整卷 /vol{n}，没有闸门就等于
// 给任何登录用户一个「读整卷任意文件」的接口（私钥/配置/别人家目录）。
func TestRawRejectsNonMedia(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", dir)

	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 顺手放一个 .html：不加闸门时它会在同源下被浏览器当页面执行
	html := filepath.Join(dir, "x.html")
	if err := os.WriteFile(html, []byte("<script>alert(1)</script>"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{secret, html} {
		rec := httptest.NewRecorder()
		handleRaw(rec, httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(p), nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("非媒体文件 %s 应返回 403，实际 %d（body=%q）", filepath.Base(p), rec.Code, rec.Body.String())
		}
	}

	// 媒体文件仍必须放行，并且带 nosniff
	img := filepath.Join(dir, "ok.png")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	rec := httptest.NewRecorder()
	handleRaw(rec, httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(img), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("正常图片应放行，实际 %d", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("媒体响应缺少 X-Content-Type-Options: nosniff")
	}
}

// P1：扫描隐藏目录时不该整棵树被跳过（原写法把 basename 和绝对路径比较，恒不相等）。
func TestScanRecursiveHiddenRoot(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".hidden-album")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(hidden, "a.png")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	files, err := scanRecursive(hidden, 100)
	if err != nil {
		t.Fatalf("scanRecursive 报错：%v", err)
	}
	if len(files) != 1 {
		t.Errorf("隐藏目录作为扫描根时应返回 1 个文件，实际 %d（整棵树被 SkipDir 了）", len(files))
	}
}

// 前端修复的契约断言：这些都是「用户能感知」的行为，被回退时必须拦住。
func TestFrontendAuditFixPieces(t *testing.T) {
	js := fetchFrontend(t, "/app.js").Body.String()
	bridge := fetchFrontend(t, "/bridge.js").Body.String()

	must := map[string]string{
		"var swipeSeq = 0":                      "切换序号：作废过期的延迟回调",
		"if (mySeq !== swipeSeq)":               "doIt / settleAfterSwipe 的过期判定",
		"clearTimeout(timer);":                  "僵尸 handler 的兜底定时器要被清掉",
		"var loadSeq = 0":                       "目录加载序号",
		"if (mySeq !== loadSeq)":                "快速连点目录时丢弃过期结果",
		"function layout(keepScroll)":           "layout 支持保留滚动位置",
		"layout(true)":                          "resize / 切换图标大小要保留滚动位置",
		"btn.disabled = true;":                  "设置面板加载完成前禁止保存",
		"if (!ok) host._ready = null;":          "桥接失败不缓存，允许重试",
		"case 'bridge-error': return '桥接通讯异常';": "补上缺失的失败原因文案",
		// 1.8.40
		"API + '/volumes'":      "首页改走 /api/volumes（不再硬编码盘位）",
		"new AbortController()": "悬停预取可被取消",
		"v.hiSrc = '';":         "切换图片时复位「已升级原图」标记",
	}
	for s, why := range must {
		if !strings.Contains(js, s) {
			t.Errorf("app.js 缺少 %q（%s）", s, why)
		}
	}
	if strings.Contains(js, "for (var i = 1; i <= 10; i++)") {
		t.Error("首页又硬编码 /vol1..10 了 —— 盘位超过 10 的机器会看不到后面的存储空间")
	}
	if n := strings.Count(js, "bumpThumbCacheBust();"); n < 3 {
		t.Errorf("bumpThumbCacheBust() 只调用 %d 次：保存设置 / 清空缓存 / 清除数据后都要换 URL", n)
	}
	// 缩略图 URL 必须带画质，否则改完画质浏览器仍用 immutable 一年的旧图
	if !strings.Contains(js, "'&q=' + (cfg.thumbQuality") {
		t.Error("thumbURL 没有带画质参数")
	}
	if !strings.Contains(bridge, "if (!r.ok) readyPromise = null;") {
		t.Error("bridge.js 的握手失败结果仍被永久缓存（一次超时后整个会话都用不了独立窗口）")
	}
}

// 1.8.40：首页盘位列表接口。/vol* 依赖运行环境（Windows 上通常为空），
// 这里只验证接口可用、返回合法 JSON 且条目字段完整。
func TestVolumesEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	handleVolumes(rec, httptest.NewRequest("GET", "/api/volumes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	var resp struct {
		Volumes []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（body=%q）", err, rec.Body.String())
	}
	for _, v := range resp.Volumes {
		if v.Path == "" || v.Name == "" {
			t.Errorf("盘位条目字段缺失：%+v", v)
		}
	}
}
