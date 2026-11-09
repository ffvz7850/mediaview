package main

// 2.4.19 引入、2.4.20 改了默认值的设置项 coverNameBelow：
// 「文件夹名称显示在封面**下方**的独立一行」。
//
// 2.4.19 时这个开关默认 **开**（名字单独一行）；2.4.20 按用户要求把默认那套改成
// "压在封面底部的半透明条 + 灰度自适应字色"（见 zzz_2420_cover_tone_test.go），
// 于是本开关默认变为 **关**，开启后才回到"独立一行"。
//
// 本文件守四层：
//
//	① 默认值 + JSON 键名：默认 false，且键名就是 coverNameBelow；
//	② mergeAbsent：整份替换接口下，请求里没带这个字段时**必须保留原值**
//	   —— 用户打开了"独立一行"，一个缓存着老 app.js 的窗口保存一次不能把它关掉；
//	③ 前端结构：开启时名称节点必须挂在 .cell-inner 下（封面**外面**），
//	   挂在 .cell-cover 里就还是"压在图上"，等于没改；
//	④ CSS 契约：两条模式互斥显示；flex 列里 .cell-cover 必须可收缩（否则名字被顶出格子）。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- ① 默认值 ----------

func Test2419CoverNameBelowDefaultOff(t *testing.T) {
	// 2.4.20 起默认 false：默认那套是"压在封面底部的半透明条 + 灰度自适应字色"。
	// 这个断言同时守住"别有人又把默认改回 true" —— 那会让所有人突然换成另一种排版。
	if defaultSettings().CoverNameBelow {
		t.Fatal("★ 默认必须是 false（默认压在封面底部），2.4.20 起用户要的是灰度自适应那套")
	}
	// JSON 键名是前后端的契约：写成 coverName 之类的别名，前端读到的永远是 undefined
	b, err := json.Marshal(defaultSettings())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	v, ok := m["coverNameBelow"]
	if !ok {
		t.Fatal("★ JSON 里没有 coverNameBelow 键 —— 前端 s.coverNameBelow 恒为 undefined")
	}
	if v != false {
		t.Errorf("默认值应为 false，实际 %v", v)
	}
}

// ---------- ② mergeAbsent ----------

func Test2419MergeAbsentKeepsCoverNameBelow(t *testing.T) {
	prev := defaultSettings()
	prev.CoverNameBelow = true

	// 用户在面板里关掉了 → 本次保存后 prev 是 false，之后再收到"不带该字段"的请求也要保持 false
	prevOff := defaultSettings()
	prevOff.CoverNameBelow = false

	cases := []struct {
		name    string
		present bool
		value   bool
		prev    Settings
		want    bool
	}{
		{name: "字段缺失且原来是开", present: false, prev: prev, want: true},
		{name: "字段缺失且原来是关", present: false, prev: prevOff, want: false},
		{name: "显式关闭", present: true, value: false, prev: prev, want: false},
		{name: "显式开启", present: true, value: true, prev: prevOff, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 严格复刻 handler 的两步：① 把请求体反序列化进 Settings（缺失字段 = 零值）
			// ② mergeAbsent 用旧值补齐。只做第 ② 步测不出"显式传值"那条分支。
			raw := map[string]json.RawMessage{}
			if c.present {
				b, _ := json.Marshal(c.value)
				raw["coverNameBelow"] = b
			}
			body, _ := json.Marshal(raw)
			var in Settings
			if err := json.Unmarshal(body, &in); err != nil {
				t.Fatalf("反序列化失败: %v", err)
			}
			in.mergeAbsent(raw, c.prev)
			if in.CoverNameBelow != c.want {
				t.Errorf("★ coverNameBelow = %v，应为 %v（字段缺失时被清零 = 老前端保存一次就把开关关掉）",
					in.CoverNameBelow, c.want)
			}
		})
	}
}

// ---------- ③ HTTP 端到端：POST → 落盘 → GET ----------

func Test2419CoverNameBelowPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MEDIAVIEW_ETC", dir)

	// 这个用例会真的走保存并改写全局 settings/settingsFile，结束后必须还原，
	// 否则同批次其它用例会读到被污染的配置（与 zz_settings_persist_test.go 同一套纪律）。
	prevFile := settingsFile
	prevSettings := getSettings()
	t.Cleanup(func() {
		settingsMu.Lock()
		settingsFile = prevFile
		settings = prevSettings
		settingsMu.Unlock()
	})
	loadSettings()

	post := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleSettingsSave(w, req)
		if w.Code != 200 {
			t.Fatalf("保存失败(%d): %s", w.Code, w.Body.String())
		}
		var resp struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是预期 JSON: %v / %s", err, w.Body.String())
		}
		return resp.Settings
	}

	// GET 时要真的带这个键（前端读的是 JSON 字段名，读结构体测不出来）
	getView := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		handleSettings(w, httptest.NewRequest("GET", "/api/settings", nil))
		if w.Code != 200 {
			t.Fatalf("GET 失败(%d)", w.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("GET 响应不是 JSON: %v", err)
		}
		return m
	}

	// 默认关（2.4.20 起默认那套是"压在封面底部 + 灰度自适应"）
	if v, ok := getView()["coverNameBelow"]; !ok || v != false {
		t.Fatalf("★ GET 应返回 coverNameBelow=false，实际 ok=%v v=%v", ok, v)
	}

	// 用户打开"独立一行" → 必须真的落盘、立即可读
	got := post(`{"thumbEnabled":true,"coverEnabled":true,"thumbSize":320,"thumbQuality":80,"coverNameBelow":true}`)
	if got["coverNameBelow"] != true {
		t.Errorf("开启没生效，实际 %v", got["coverNameBelow"])
	}

	// ★ 模拟"老前端/第三方"提交：body 里根本不含 coverNameBelow
	// → 必须保留 true，不能被整份替换清成 false（用户打开了、别的窗口一保存就白开了）
	got = post(`{"thumbEnabled":true,"coverEnabled":true,"thumbSize":320,"thumbQuality":80}`)
	if got["coverNameBelow"] != true {
		t.Errorf("★ 请求里没带 coverNameBelow 时应保留原值 true，实际 %v", got["coverNameBelow"])
	}
	if v := getView()["coverNameBelow"]; v != true {
		t.Errorf("★ 落盘后再 GET 应为 true，实际 %v", v)
	}

	// 模拟进程重启：内存清空后从文件读回
	settingsMu.Lock()
	settings = defaultSettings()
	settingsMu.Unlock()
	loadSettings()
	if !getSettings().CoverNameBelow {
		t.Error("★ 重启读回后变成了 false —— 说明这个字段没写进配置文件（POST 里带了却没存）")
	}

	// 用户再关掉，确认能回来
	got = post(`{"thumbEnabled":true,"coverEnabled":true,"thumbSize":320,"thumbQuality":80,"coverNameBelow":false}`)
	if got["coverNameBelow"] != false {
		t.Errorf("重新关闭没生效，实际 %v", got["coverNameBelow"])
	}
}

// ---------- ④ 前端结构：名称节点在封面之外 ----------

func Test2419DirCellNameOutsideCover(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "if (item.kind === 'dir') {")
	if i < 0 {
		t.Fatal("找不到 dir 分支")
	}
	seg := js[i:]
	if j := strings.Index(seg, "} else if (!cfg.thumbEnabled) {"); j > 0 {
		seg = seg[:j]
	}

	// 开关来源：必须读 cfg.coverNameBelow（默认开：!== false）
	if !strings.Contains(seg, "var nameBelow = cfg.coverNameBelow === true;") {
		t.Error("★ dir 分支必须从 cfg.coverNameBelow 取值（缺省视为关 = 压在封面底部）")
	}
	// 格子要加 .mv-name-below，CSS 靠它切换布局
	if !strings.Contains(seg, "inner.classList.add('mv-name-below')") {
		t.Error("★ 缺少 inner.classList.add('mv-name-below') —— CSS 的两套布局靠这个 class 切换")
	}
	// 名称节点
	if !strings.Contains(seg, "className = 'cell-name-below'") {
		t.Error("★ 缺少 .cell-name-below 名称行")
	}
	if !strings.Contains(seg, "inner.appendChild(nb)") {
		t.Error("★ 名称行必须 appendChild 到 .cell-inner")
	}
	// 核心：绝不能挂进 .cell-cover —— 那样还是"压在图上"
	if strings.Contains(seg, "cover.appendChild(nb)") {
		t.Error("★★ 名称行被塞进了 .cell-cover：那就还是压在封面图上，用户要的「下方独立一行」没有实现")
	}
	// DOM 顺序：先封面后名称（名称在下面，不是上面）
	ic := strings.Index(seg, "inner.appendChild(cover)")
	in := strings.Index(seg, "inner.appendChild(nb)")
	if ic < 0 || in < 0 {
		t.Fatalf("找不到挂载点（cover=%d nb=%d）", ic, in)
	}
	if in < ic {
		t.Error("★ 名称行必须排在封面之后（先图后字）")
	}
}

// ---------- ⑤ CSS 契约 ----------

// cssRule 取出选择器对应的声明块（单层大括号，够用于本项目）。
//
// 先把 /* … */ 注释剥掉：我们的 CSS 注释里会写「z-index:auto」「padding:18px」
// 这类**反面**说明，不剥就会让断言因为注释而假通过（"注释里写了 = 测试绿"是最蠢的假绿）。
func cssRule(t *testing.T, css, selector string) string {
	t.Helper()
	for {
		i := strings.Index(css, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(css[i:], "*/")
		if j < 0 {
			break
		}
		css = css[:i] + css[i+j+2:]
	}
	i := strings.Index(css, selector+" {")
	if i < 0 {
		t.Fatalf("CSS 里找不到规则 %q", selector)
	}
	seg := css[i+len(selector):]
	j := strings.Index(seg, "}")
	if j < 0 {
		t.Fatalf("规则 %q 没有闭合", selector)
	}
	return squashWS(seg[:j])
}

func squashWS(s string) string {
	return strings.Join(strings.Fields(s), "")
}

func Test2419CSSLayoutContract(t *testing.T) {
	css := readSourceOrSkip(t, "web/style.css")

	// 格子内部改成纵向 flex，封面让出高度
	inner := cssRule(t, css, ".cell-inner.mv-name-below")
	if !strings.Contains(inner, "display:flex") || !strings.Contains(inner, "flex-direction:column") {
		t.Errorf("★ .cell-inner.mv-name-below 必须是纵向 flex，实际 %q", inner)
	}

	// 关键回归：封面必须允许收缩（flex 的 shrink）。
	//
	// .cell-cover 自身是 height:100%。在纵向 flex 里如果不让它收缩，它会把下面的
	// 名称行顶出格子，而 .cell-inner 有 overflow:hidden —— 界面上名称与封面一起被裁掉。
	// 这一条做过截图验证：把 flex 改成 flex:none / 把 inner 改回 block，两种变异下
	// 卡片里只剩一块底色，名字和封面都看不见。
	cover := cssRule(t, css, ".cell-inner.mv-name-below .cell-cover")
	if !strings.Contains(cover, "flex:11auto") && !strings.Contains(cover, "flex:01auto") {
		t.Errorf("★★ .cell-cover 必须可收缩（flex: 1 1 auto），否则名称行被顶出格子、连同封面一起被 overflow 裁掉；实际 %q", cover)
	}

	// 两套模式互斥：开了下方名称，就不能再让封面遮罩把同一个名字显示一遍
	label := cssRule(t, css, ".cell-inner.mv-name-below .cell-label")
	if !strings.Contains(label, "display:none") {
		t.Errorf("★ 名称在下方时，封面上的 .cell-label 必须隐藏（否则同一个名字显示两遍，截图可见）实际 %q", label)
	}
	fbName := cssRule(t, css, ".cell-inner.mv-name-below .cell-fallback-name")
	if !strings.Contains(fbName, "display:none") {
		t.Errorf("★ 名称在下方时，📁 兜底里的小字也必须隐藏（名字已在下方的行里）实际 %q", fbName)
	}

	// 名称行本身：定高、单行、超长省略（窄格子下不能换行把行高撑破）
	nb := cssRule(t, css, ".cell-name-below")
	for _, want := range []string{"height:", "line-height:", "text-overflow:ellipsis", "white-space:nowrap", "overflow:hidden"} {
		if !strings.Contains(nb, want) {
			t.Errorf("★ .cell-name-below 缺少 %q，实际 %q", want, nb)
		}
	}
	if !strings.Contains(nb, "pointer-events:none") {
		t.Errorf(".cell-name-below 应 pointer-events:none —— 名字行不能挡住格子的点击，实际 %q", nb)
	}
}

// ---------- ⑥ 设置界面接线 ----------

func Test2419SettingsUIWiring(t *testing.T) {
	html := readSourceOrSkip(t, "web/index.html")
	for _, want := range []string{`id="setCoverName"`, `id="rowCoverName"`, "文件夹名称显示在封面下方"} {
		if !strings.Contains(html, want) {
			t.Errorf("设置页缺少 %q", want)
		}
	}

	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	cases := []struct{ frag, why string }{
		{"coverNameBelow: false,", "cfg 默认值必须是 false（与后端默认一致：压在封面底部）"},
		{"cfg.coverNameBelow = s.coverNameBelow === true", "applySettings 必须把它读进 cfg"},
		{"$('setCoverName').checked = cfg.coverNameBelow", "fillSettingsForm 必须回填开关"},
		{"coverNameBelow: $('setCoverName').checked", "saveSettings 必须提交开关状态（漏了就是「点了保存不生效」）"},
	}
	for _, c := range cases {
		if !strings.Contains(js, c.frag) {
			t.Errorf("缺少：%s（%s）", c.frag, c.why)
		}
	}

	// 保存后必须重绘：新样式只在建格子时生效，不重绘就是"保存了但界面没变"
	if !strings.Contains(js, "function redraw()") || !strings.Contains(js, "redraw();") {
		t.Error("saveSettings 之后必须 redraw()（否则改了开关当前视图不跟着变）")
	}
}
