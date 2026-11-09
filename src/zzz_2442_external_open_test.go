// 2.4.42 新增：外部打开（飞牛文件管理器双击 → 打开方式 → 本应用）必须不受
// 「只显示我添加的文件夹」限制。
//
// 用户报的现象：开启该开关后，除了添加过的文件夹，文件管理器里其它目录
// 双击图片/视频都打不开（"打不开"= 服务端 403，页面里就是一张碎图 + 空列表）。
//
// 契约分两层，缺一层就还是坏的：
//
//	① 服务端：带 external=1 的请求跳过 OnlyRoots 这一层，但**存储空间白名单照旧**
//	   （否则这就是一个能读 /etc 的洞）；
//	② 前端：单文件模式（URL 带 ?path=）发出的 raw / thumb / meta / list 要带这个标记；
//	   而**网格与首页**绝不能带 —— 带了等于把可见范围整个取消掉。
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 1x1 的最小合法 JPEG（/api/raw 不做转码时直接回原字节）
const tinyJPEG = "/9j/4AAQSkZJRgABAQEAYABgAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRof" +
	"Hh0aHBwcJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPDUzNDH/2wBDAQkJCQwLDBgNDRgyIRwhMjIyMjIy" +
	"MjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjL/wAARCAABAAEDASIA" +
	"AhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/9oADAMBAAIRAxEAPwCwAA8A" +
	"AAAAAAAAAA=="

// setupExternalOpenEnv 造一个「OnlyRoots 已开、只允许 inside」的世界，
// 返回 outside 目录（在存储空间白名单内，但不在可见范围内）。
func setupExternalOpenEnv(t *testing.T) (inside, outside string) {
	t.Helper()
	base := t.TempDir()
	inside = filepath.Join(base, "我的照片")
	outside = filepath.Join(base, "别人的照片")
	mkdirs(t, inside, outside)
	// 真实部署里这一层由 /vol* 提供；测试里用环境变量把 base 声明成可访问路径。
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", base)
	// 注意 roots 必须是 Clean 后的绝对路径（与保存流程一致，否则前缀比较会落空）
	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{filepath.Clean(inside)}})
	return inside, outside
}

func writeJPEG(t *testing.T, path string) {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatalf("内置 JPEG 解码失败：%v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("写测试图片失败：%v", err)
	}
}

// ---------- ① 服务端：校验口径 ----------

// Test2442ResolveReqHonoursExternal 单元层：external=1 只放开 OnlyRoots 那一层。
func Test2442ResolveReqHonoursExternal(t *testing.T) {
	_, outside := setupExternalOpenEnv(t)
	dir := filepath.Join(outside, "sub")
	mkdirs(t, dir)

	req := func(q string) *http.Request {
		return httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(dir)+q, nil)
	}

	if _, sc := resolveMediaPathReq(req(""), dir); sc != http.StatusForbidden {
		t.Errorf("★★ 可见范围外、且未声明外部打开时必须 403（否则「只显示我添加的文件夹」形同虚设），实际 %d", sc)
	}
	if _, sc := resolveMediaPathReq(req("&external=1"), dir); sc != 0 {
		t.Errorf("★★ external=1 时必须放行（文件管理器双击其它目录就是这条），实际 %d", sc)
	}
	// 只认 "1"：多余写法一律当没开，避免出现"随手加个参数就绕过了"的模糊地带
	for _, bad := range []string{"&external=true", "&external", "&external=0", "&external=yes"} {
		if _, sc := resolveMediaPathReq(req(bad), dir); sc != http.StatusForbidden {
			t.Errorf("★ %s 不应被当成外部打开（只认 external=1），实际 %d", bad, sc)
		}
	}
}

// Test2442ExternalDoesNotWidenWhitelist external=1 不能把存储空间白名单也放开。
//
// 这是本修法唯一真正危险的方向：OnlyRoots 只是"少显示"，而 pathAllowed 是
// "这个进程以 root 跑、不能读整个文件系统"的边界。两者必须严格区分。
func Test2442ExternalDoesNotWidenWhitelist(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	mkdirs(t, allowed)
	t.Setenv("TRIM_DATA_ACCESSIBLE_PATHS", allowed)

	outsideWL := t.TempDir() // 完全不在这台机器的可访问路径里
	mkdirs(t, outsideWL)
	withTestSettings(t, Settings{OnlyRoots: true, Roots: []string{allowed}})

	req := httptest.NewRequest("GET", "/api/raw?path="+url.QueryEscape(outsideWL)+"&external=1", nil)
	if _, sc := resolveMediaPathReq(req, outsideWL); sc != http.StatusForbidden {
		t.Errorf("★★ 白名单之外的路径即使带 external=1 也必须 403，实际 %d —— "+
			"external 只放开 OnlyRoots，不能变成读整个磁盘的后门", sc)
	}
}

// Test2442HandlersAllowExternalOpen 行为层：真打四个接口，范围外要能通。
//
// 只断言 "不是 403"，而不是 200：缩略图在没有 ffmpeg 的机器上会失败（那是另一件事），
// 这里要钉的是**范围判定**这一层。
func Test2442HandlersAllowExternalOpen(t *testing.T) {
	_, outside := setupExternalOpenEnv(t)
	img := filepath.Join(outside, "a.jpg")
	writeJPEG(t, img)

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"list", http.MethodGet, "/api/list?path=" + url.QueryEscape(outside) + "&sort=name:asc"},
		{"raw", http.MethodGet, "/api/raw?path=" + url.QueryEscape(img)},
		{"meta", http.MethodGet, "/api/meta?path=" + url.QueryEscape(img)},
	}
	handlers := map[string]http.HandlerFunc{
		"list": handleList,
		"raw":  handleRaw,
		"meta": handleMeta,
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 不带标记 ⇒ 必须 403（可见范围还在）
			rec := httptest.NewRecorder()
			handlers[c.name](rec, httptest.NewRequest(c.method, c.target, nil))
			if rec.Code != http.StatusForbidden {
				t.Errorf("★★ 未带 external 时应 403（说明可见范围被放开了），实际 %d", rec.Code)
			}
			// 带标记 ⇒ 不能 403
			rec = httptest.NewRecorder()
			handlers[c.name](rec, httptest.NewRequest(c.method, c.target+"&external=1", nil))
			if rec.Code == http.StatusForbidden {
				t.Errorf("★★ 带了 external=1 仍 403 —— 文件管理器里双击范围外的图片就会「打不开」")
			}
		})
	}

	// 缩略图单独一测：未带标记 403 是硬要求；带了标记只要求"不是 403"
	rec := httptest.NewRecorder()
	handleThumb(rec, httptest.NewRequest("GET", "/api/thumb?path="+url.QueryEscape(img)+"&size=320", nil))
	if rec.Code != http.StatusForbidden {
		// 缩略图被关掉时也会 403（X-MediaView-Thumb: disabled），那种情况说明前置条件不对
		t.Fatalf("★ 缩略图未带 external 时应 403，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleThumb(rec, httptest.NewRequest("GET", "/api/thumb?path="+url.QueryEscape(img)+"&size=320&external=1", nil))
	if rec.Code == http.StatusForbidden && rec.Header().Get("X-MediaView-Thumb") != "disabled" {
		t.Error("★★ 缩略图带了 external=1 仍被范围判定拒绝（403）—— 竖图流/网格的封面会全空")
	}
}

// Test2442AllPathHandlersUseReq 静态闸门：接受 path 读文件的接口都要走 resolveMediaPathReq。
//
// 为什么要逐个点名：漏改一处不会"整体坏"，只会让那条路径单独 403 ——
// 例如只改了 raw 没改 list，现象就是"图能看见、左右切不动"，很难第二次定位。
func Test2442AllPathHandlersUseReq(t *testing.T) {
	src := readSourceOrSkip(t, "media.go")
	thumb := readSourceOrSkip(t, "thumb.go")

	for _, fn := range []string{"handleList", "handleRaw", "handleMeta"} {
		body := segmentBetween(src, "func "+fn+"(", "\nfunc ")
		if body == "" {
			t.Fatalf("找不到 %s", fn)
		}
		if !strings.Contains(body, "resolveMediaPathReq(r,") {
			t.Errorf("★★ %s 没有走 resolveMediaPathReq —— 外部打开会被范围判定 403", fn)
		}
	}
	thumbBody := segmentBetween(thumb, "func handleThumb(", "\nfunc ")
	if !strings.Contains(thumbBody, "resolveMediaPathReq(r,") {
		t.Error("★★ handleThumb 没有走 resolveMediaPathReq —— 视频封面/竖图流在范围外会 403")
	}
}

// ---------- ② 前端：标记只出现在单文件模式 ----------

// Test2442FrontendExternalTag 前端契约。
//
// 三件事必须同时成立，少一件就有一半是坏的：
//   - 单文件模式的 raw / thumb / meta / list 都带上标记（少一个 = 那条请求 403；
//     漏了 list 的表现是"图能看、左右切不动"，漏了 raw 就是"碎图"）；
//   - 网格与首页**不带**（带了等于取消可见范围）；
//   - 只认 singleMode：顶层已经按 URL 定好，这里不许再另起一套判据。
func Test2442FrontendExternalTag(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	if !strings.Contains(js, "function externalTag()") {
		t.Fatal("缺 externalTag()")
	}
	tagBody := segmentBetween(js, "function externalTag() {", "\n  }")
	if !strings.Contains(tagBody, "&external=1") {
		t.Errorf("★ externalTag 的值必须是 &external=1，实际：%s", tagBody)
	}
	if !strings.Contains(tagBody, "singleMode") {
		t.Error("★★ externalTag 必须以 singleMode 为唯一判据 —— 否则网格请求也会带上，可见范围直接失效")
	}

	// 四个 URL 构造点都要拼上
	for _, fn := range []string{"function thumbURL(", "function rawURL(", "function metaURL("} {
		body := segmentBetween(js, fn, "\n  }")
		if body == "" {
			t.Fatalf("找不到 %s", fn)
		}
		if !strings.Contains(body, "externalTag()") {
			t.Errorf("★★ %s 没有拼 externalTag() —— 该接口在范围外会 403", fn)
		}
	}
	// openSingle 的两条 /list（带 sort / 读偏好后带 sort）
	listCalls := strings.Count(js, "'/list?path=' + enc(dir) + '&sort=' + enc(")
	if listCalls < 2 {
		t.Fatalf("openSingle 的 list 请求结构变了（找到 %d 处）", listCalls)
	}
	if got := strings.Count(js, "'&sort=' + enc(sort) + externalTag())"); got != 1 {
		t.Errorf("★ openSingle 的 list（显式 sort）没带标记（匹配 %d 处）", got)
	}
	if got := strings.Count(js, "'&sort=' + enc(s) + externalTag())"); got != 1 {
		t.Errorf("★ openSingle 的 list（偏好 sort）没带标记（匹配 %d 处）", got)
	}

	// 网格：loadDir 的 list 与首页聚合 list 一律不许带
	loadDir := segmentBetween(js, "function loadDir(path) {", "\n  function ")
	if strings.Contains(loadDir, "externalTag") {
		t.Error("★★ loadDir 的列表请求不许带 externalTag —— 那是应用内浏览，必须继续受可见范围约束")
	}
	if strings.Contains(js, "'/list?path=' + enc('/') + '&sort=' + enc(state.sort) + externalTag()") {
		t.Error("★★ 首页聚合列表不许带 externalTag")
	}
}

// Test2442FrontendExternalTagRuntime 运行时验证：把 app.js 里真正的那几个 URL 构造函数
// 抠出来跑一遍，看**产出的字符串**。
//
// 静态断言只能证明"代码里有 externalTag()"；这条才能证明"拼出来的 URL 里真的有"。
// 差别不是吹毛求疵：把 `return url + externalTag();` 写成漏赋值（只调用、不拼接），
// 静态照样绿，真机上却全 403。
func Test2442FrontendExternalTagRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	appPath, _ := filepath.Abs(filepath.Join("web", "app.js"))
	if _, err := os.Stat(appPath); err != nil {
		t.Skipf("找不到 web/app.js：%v", err)
	}

	harness := `
const fs = require('fs');
const src = fs.readFileSync(process.env.MV_APPJS, 'utf8');
function grab(name) {
  const i = src.indexOf('function ' + name + '(');
  if (i < 0) throw new Error('no ' + name);
  let d = 0;
  for (let k = src.indexOf('{', i); k < src.length; k++) {
    if (src[k] === '{') d++;
    else if (src[k] === '}') { d--; if (!d) return src.slice(i, k + 1); }
  }
  throw new Error('unbalanced ' + name);
}
function build(mode) {
  const body =
    'var API = "/app/mediaview/api";' +
    'var thumbCacheBust = 7;' +
    'var cfg = { thumbQuality: 82, viewerQuality: 90, viewerLowres: 0 };' +
    'var singleMode = ' + mode + ';' +
    'function enc(s) { return encodeURIComponent(s); }' +
    grab('rawSettingsTag') + grab('externalTag') + grab('rawURL') + grab('thumbURL') + grab('metaURL') +
    'return { raw: rawURL("/vol1/1000/图 库/a.jpg", 1024, 123),' +
    '         thumb: thumbURL("/vol1/1000/图 库/a.jpg", 320, 123),' +
    '         meta: metaURL("/vol1/1000/图 库/a.jpg") };';
  return new Function(body)();
}
console.log(JSON.stringify({ on: build(true), off: build(false) }));
`
	cmd := exec.Command(node, "-e", harness)
	// node -e 下 process.argv 里没有脚本参数位，路径走环境变量最稳
	cmd.Env = append(os.Environ(), "MV_APPJS="+appPath)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node 运行失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("node 运行失败：%v", err)
	}
	var res struct {
		On  map[string]string `json:"on"`
		Off map[string]string `json:"off"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("输出不是 JSON：%v\n%s", err, out)
	}
	for _, k := range []string{"raw", "thumb", "meta"} {
		if !strings.Contains(res.On[k], "external=1") {
			t.Errorf("★★ 单文件模式下 %s 的 URL 没有带 external=1：%s\n"+
				"（真机上这条请求会被范围判定 403 → 图片/视频打不开）", k, res.On[k])
		}
		if strings.Contains(res.Off[k], "external") {
			t.Errorf("★★ 网格/首页的 %s URL 带了 external：%s\n"+
				"（等于把「只显示我添加的文件夹」整个取消掉）", k, res.Off[k])
		}
	}
}

// edgeForTest 找一个可用的 Edge；找不到就 skip（CI/无浏览器环境不该红）。
func edgeForTest(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("MV_EDGE"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:/Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			`C:/Program Files\Microsoft\Edge\Application\msedge.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	t.Skip("没有 Edge，跳过真实渲染验证")
	return ""
}

// Test2442ExternalOpenRealBrowser 端到端：真后端（httptest + 真实路由）+ 真前端 + 真浏览器。
//
// 为什么非要有这一条：这次的 bug 就活在"三层之间"——
//
//	前端发什么 URL、后端怎么判、浏览器最终有没有把图解码出来。
//
// 单看任何一层都能自洽（2.4.41 的代码在范围内是完全正常的），
// 只有把三层接起来，才看得见"范围外打不开"。
//
// 断言两件事：
//
//	① 范围外的文件用 ?path= 打开：图**真的解码出来了**，且没有一条 /api/ 请求 403；
//	② 范围外的目录用 ?dir= 打开：仍然**被挡住**（网格 0 格 + 有 403）——
//	   这是为了证明修外部打开没有顺手把"只显示我添加的文件夹"整条拆掉。
func Test2442ExternalOpenRealBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实浏览器")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	edge := edgeForTest(t)

	_, outside := setupExternalOpenEnv(t)
	img := filepath.Join(outside, "范围外.jpg")
	writeJPEG(t, img)

	mux := http.NewServeMux()
	registerRoutes(mux) // 与线上完全同一套路由
	srv := httptest.NewServer(mux)
	defer srv.Close()

	script, _ := filepath.Abs(filepath.Join("..", "tools", "external_open_probe.js"))
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("找不到探针：%v", err)
	}
	cmd := exec.Command(node, script)
	cmd.Env = append(os.Environ(),
		"MV_EDGE="+edge,
		"MV_BASE="+srv.URL+"/app/mediaview/",
		"MV_FILE="+img,
		"MV_DIR="+outside,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("探针失败：%v\n%s", err, ee.Stderr)
		}
		t.Fatalf("探针失败：%v", err)
	}
	var res struct {
		Open struct {
			Img *struct {
				Src string `json:"src"`
				W   int    `json:"w"`
				H   int    `json:"h"`
			} `json:"img"`
			Ms            int  `json:"ms"`
			ViewerVisible bool `json:"viewerVisible"`
			API           []struct {
				URL    string `json:"url"`
				Status int    `json:"status"`
			} `json:"api"`
		} `json:"open"`
		Grid struct {
			Cells int `json:"cells"`
			API   []struct {
				URL    string `json:"url"`
				Status int    `json:"status"`
			} `json:"api"`
		} `json:"grid"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("探针输出不是 JSON：%v\n%s", err, out)
	}

	if !res.Open.ViewerVisible {
		t.Error("★ 单文件模式没有进入查看器（?path= 分支没生效）")
	}
	if res.Open.Img == nil || res.Open.Img.W <= 0 {
		t.Errorf("★★ 范围外的图片没解码出来（图 %+v，耗时 %dms）—— 用户在文件管理器里双击就是「打不开」",
			res.Open.Img, res.Open.Ms)
	} else if !strings.Contains(res.Open.Img.Src, "external=1") {
		t.Errorf("★★ 图能开，但请求没带 external=1（说明是靠别的路径绕过的）：%s", res.Open.Img.Src)
	}
	for _, h := range res.Open.API {
		if h.Status == http.StatusForbidden {
			t.Errorf("★★ 单文件模式下仍有 403：%d %s", h.Status, h.URL)
		}
	}

	// ② 应用内浏览必须继续被挡住
	blocked := false
	for _, h := range res.Grid.API {
		if h.Status == http.StatusForbidden {
			blocked = true
		}
	}
	if res.Grid.Cells != 0 {
		t.Errorf("★★ 直接用 ?dir= 打开范围外目录竟然画出了 %d 个格子 —— 「只显示我添加的文件夹」被放开了",
			res.Grid.Cells)
	}
	if !blocked {
		t.Error("★★ 范围外目录的 /api/list 竟然没有 403 —— 可见范围没有生效（修外部打开不该动这个）")
	}
}
