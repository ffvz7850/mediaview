package main

// 2.4.17：修「点击添加文件夹没反应」。
//
// 根因不是 JS 没接线，而是**DOM 位置**：目录选择器 #dirBrowser 当时嵌在
// #thumbOpts 内部，而 #thumbOpts 在「生成缩略图」关闭时会被整块 display:none
// （见 syncThumbOpts）。于是点「添加文件夹」= 打开一个在隐藏容器里的面板，
// 界面上什么都不发生。
//
// 即使缩略图是开着的，面板也在下方「缩略图存放位置」块里展开，而用户点的是上方
// 「只显示我添加的文件夹」块里的按钮 —— 看不到展开结果，同样像"没反应"。
//
// 所以这里守两件事：
//   ① 结构：#dirBrowser 必须在 #thumbOpts 之外（静态可判，零依赖）；
//   ② 交互：打开时要滚到可见，且按用途改按钮文案/分流（否则共用面板会写错地方）。
//
// 另有一条真实交互探针（jsdom 加载真实页面 + 模拟点击）在本机跑过，
// 四个变异体（面板放回 thumbOpts / 去掉滚动 / 无视用途 toggle / 不分流）全部被抓。

import (
	"os"
	"strings"
	"testing"
)

// htmlElementSpan 返回 id="<id>" 所在元素的 [start, end) 字节区间。
// 只处理 <div>（本项目的面板都是 div），够用且不必引入 HTML 解析依赖。
//
// 注释先剥掉：注释里出现 "<div" 会让计数错位（本项目恰好有说明性注释在写标签）。
func htmlElementSpan(t *testing.T, html, id string) (int, int) {
	t.Helper()
	clean := stripHTMLComments(html)
	key := `id="` + id + `"`
	i := strings.Index(clean, key)
	if i < 0 {
		t.Fatalf("找不到元素 id=%q", id)
	}
	// 回退到该标签的 "<"
	start := strings.LastIndex(clean[:i], "<")
	if start < 0 {
		t.Fatalf("元素 id=%q 之前没有 '<'", id)
	}
	// 向后扫描 div 配对
	depth := 0
	for j := start; j < len(clean); {
		switch {
		case strings.HasPrefix(clean[j:], "<div"):
			depth++
			j += 4
		case strings.HasPrefix(clean[j:], "</div"):
			depth--
			j += 5
			if depth == 0 {
				// 吃掉 ">"
				for j < len(clean) && clean[j] != '>' {
					j++
				}
				if j < len(clean) {
					j++
				}
				return start, j
			}
		default:
			j++
		}
	}
	t.Fatalf("元素 id=%q 的 div 没有配平", id)
	return 0, 0
}

func stripHTMLComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return b.String()
		}
		s = s[i+j+3:]
	}
}

// Test2417DirBrowserOutsideThumbOpts 目录选择器绝不能嵌在 #thumbOpts 里。
//
// 这条断言就是用户报的 bug 本身：嵌在里面时，关闭「生成缩略图」后
// 点「添加文件夹」打开的是一个 display:none 容器内的面板 —— 界面上毫无反应。
func Test2417DirBrowserOutsideThumbOpts(t *testing.T) {
	raw, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)

	// 前提：两个元素都在
	for _, id := range []string{"thumbOpts", "dirBrowser", "btnAddRoot", "dbPick", "dbClose"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Fatalf("index.html 缺少 id=%q", id)
		}
	}

	ts, te := htmlElementSpan(t, html, "thumbOpts")
	ds, _ := htmlElementSpan(t, html, "dirBrowser")

	if ds > ts && ds < te {
		t.Fatalf("★★ #dirBrowser 落在了 #thumbOpts 内部（%d..%d 之间）——\n"+
			"关闭「生成缩略图」时整个 #thumbOpts 被 display:none，"+
			"点「添加文件夹」就会打开一个隐藏容器里的面板，表现就是点了没反应。\n"+
			"必须把目录选择器放到 #thumbOpts 之外。", ts, te)
	}
}

// Test2417OpenBrowserScrollsIntoView 打开选择器必须滚到可见。
//
// 面板为了躲开 #thumbOpts 被放在设置主体末尾（赞助框之上），而「添加文件夹」
// 按钮在上方的文件块里。不滚过去，用户看到的就是"按钮点下去没动静"。
func Test2417OpenBrowserScrollsIntoView(t *testing.T) {
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)

	i := strings.Index(js, "function openBrowser(startPath)")
	if i < 0 {
		t.Fatal("找不到 openBrowser")
	}
	end := strings.Index(js[i:], "\n  }")
	if end < 0 {
		t.Fatal("openBrowser 没有正常结束")
	}
	body := js[i : i+end]

	if !strings.Contains(body, "scrollIntoView") {
		t.Error("★ openBrowser 里没有 scrollIntoView —— 面板在设置主体末尾，" +
			"不滚过去用户看不到，会以为「添加文件夹」没反应")
	}
	if !strings.Contains(body, "dbPickTarget === 'root'") {
		t.Error("★ openBrowser 没有按用途切换按钮文案（共用面板必须标出来，否则容易把添加写成改缩略图目录）")
	}
}

// Test2417BrowserOpensInPlace 面板必须**就地展开**在按钮所在区块里。
//
// 这一条来自用户的第二次反馈：「点了会跳到下面的缩略图缓存位置」。
// 2.4.17 把面板固定放在设置主体末尾，而那里紧跟着 #thumbOpts 的最后一块
// 「缩略图缓存」（清空缓存 / 清除所有数据）—— scrollIntoView 滚过去，用户看到的就是
// 缩略图区域，以为跳错了地方。功能其实是开的，但心智上完全没对上。
//
// 正解是让面板跟着入口走：点「添加文件夹」→ 出现在 #rootBrowserHost；
// 点「选择目录」→ 出现在 #thumbBrowserHost。同一个 DOM 用 appendChild 移动，不复制。
func Test2417BrowserOpensInPlace(t *testing.T) {
	rawHTML, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(rawHTML)

	// 两个宿主都要在，且各自位于对应区块内
	for _, id := range []string{"rootBrowserHost", "thumbBrowserHost"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Fatalf("index.html 缺少宿主 id=%q —— 面板没有可就地展开的位置", id)
		}
	}

	// 宿主必须落在「只显示我添加的文件夹」这个块内：
	// 即位于 rootList 之后、下一个块（一键生成封面）之前。
	// 用相邻块的锚点界定，比按 div 配平更直接（块里混着 <input>/<button>）。
	iList := strings.Index(html, `id="rootList"`)
	iHost := strings.Index(html, `id="rootBrowserHost"`)
	iNext := strings.Index(html, "一键生成文件夹封面")
	iBtn := strings.Index(html, `id="btnAddRoot"`)
	if iList < 0 || iHost < 0 || iNext < 0 || iBtn < 0 {
		t.Fatal("找不到 rootList / rootBrowserHost / 下一个块 / btnAddRoot 的锚点")
	}
	if !(iList < iHost && iHost < iNext) {
		t.Errorf("★ #rootBrowserHost 不在「只显示我添加的文件夹」块内"+
			"（rootList=%d 宿主=%d 下一块=%d）—— 面板会展开到别的区块", iList, iHost, iNext)
	}
	if !(iBtn < iHost) {
		t.Errorf("宿主应在「添加文件夹」按钮之后（按钮=%d 宿主=%d）", iBtn, iHost)
	}

	rawJS, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(rawJS)

	if !strings.Contains(js, "function browserHostFor") {
		t.Fatal("缺少 browserHostFor() —— 面板没有按用途选择宿主")
	}
	i := strings.Index(js, "function openBrowser(startPath)")
	if i < 0 {
		t.Fatal("找不到 openBrowser")
	}
	end := strings.Index(js[i:], "\n  }")
	if end < 0 {
		t.Fatal("openBrowser 没有正常结束")
	}
	body := js[i : i+end]

	if !strings.Contains(body, "appendChild(box)") {
		t.Error("★★ openBrowser 里没有 appendChild(box) —— 面板不会被移到宿主里，" +
			"仍会固定在设置末尾（用户看到的就是\"跳到缩略图缓存位置\"）")
	}
	if !strings.Contains(body, "browserHostFor(dbPickTarget)") {
		t.Error("★ openBrowser 没有按 dbPickTarget 选宿主（两处入口会展开到同一处）")
	}

	// 反向：面板的静态位置绝不能落在 #thumbOpts 内（那条由另一个测试守），
	// 但也不能是唯一位置 —— 必须能被移动，所以 modal-body 里要留一个"仓库"。
	if !strings.Contains(html, `id="dirBrowser"`) {
		t.Error("缺少面板本体 #dirBrowser")
	}
}

// Test2417AddRootWiring 三个入口的接线与分流必须在位。
func Test2417AddRootWiring(t *testing.T) {
	raw, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(raw)

	cases := []struct{ anchor, why string }{
		{"$('btnAddRoot').onclick", "「添加文件夹」按钮必须接线（否则点了真的一点反应都没有）"},
		{"$('dbClose').onclick", "选择器要有显式关闭入口（只能靠「用此目录」退出会逼用户做选择）"},
		{"dbPickTarget = 'root'", "点「添加文件夹」必须把用途标记为 root"},
		{"pendingRoots.push(browsePath)", "选定目录必须进 pendingRoots 工作副本（否则保存时列表是空的）"},
		{"$('rootList').scrollIntoView", "添加成功后要滚回列表，否则看不到刚加的那一项，会以为没加上"},
	}
	for _, c := range cases {
		if !strings.Contains(js, c.anchor) {
			t.Errorf("缺少 %q —— %s", c.anchor, c.why)
		}
	}

	// 共用一个面板 ⇒ 两处入口的 toggle 都必须看用途，不能"开着就关"
	for _, fn := range []string{"$('btnAddRoot').onclick", "$('btnBrowse').onclick"} {
		i := strings.Index(js, fn)
		if i < 0 {
			t.Errorf("找不到 %s", fn)
			continue
		}
		seg := js[i:]
		if n := strings.Index(seg, "\n  };"); n > 0 {
			seg = seg[:n]
		}
		if !strings.Contains(seg, "dbPickTarget") {
			t.Errorf("★ %s 的收起判断没看 dbPickTarget —— 面板是两处共用的，"+
				"只看「是否已打开」会在切换用途时把面板关掉，用户以为点不动", fn)
		}
	}
}
