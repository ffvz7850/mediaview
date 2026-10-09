package main

import (
	"os"
	"strings"
	"testing"
)

// 1.8.173：向飞牛文件管理器接口索取顺序的"增强"已整体删除。
//
// 原因：那个接口飞牛已经**删掉** —— server.cjs 里 "file-info" 出现 0 次（路由没注册）。
// console 里的 401 **不是**"接口在、只是不授权"，而是该应用对所有非公开 /api/ 请求的
// 统一登录闸门（实测：真实路径与乱写路径返回一模一样的 {"error":"请先登录"}）。
// 也就是**在当前飞牛版本上它百分百降级、且不会因授权而恢复**：拿不到任何东西，
// 却每次打开目录都要付代价（控制台一条红字 + 白占一条同源 HTTP/1.1 连接 + 白等一个 RTT）。
//
// 而真正干活的一直是本地那套：sortLikeFnOs 复刻文件管理器界面规则。
// 服务端那份顺序本身也给不出正确结果（fs.browse 是纯字符串序 "(1) (10) (100)"，
// 界面是 localeCompare(zh,{numeric:true}) "(1) (2) (3) … (10)"），所以顺序本该本地生成。

// stripJSComments 去掉 // 与 /* */ 注释（认识 '…' / "…" / `…` 字符串，
// 字符串里的 // 不算注释）。断言前必须先剥 —— 否则「注释里有、代码里没有」
// 会算通过（假绿），「只在注释里提一句」又会误报（假红）。
func stripJSComments(src string) string {
	var b strings.Builder
	r := []rune(src)
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c == '\'' || c == '"' || c == '`' {
			b.WriteRune(c)
			for i++; i < len(r); i++ {
				b.WriteRune(r[i])
				if r[i] == '\\' {
					i++
					if i < len(r) {
						b.WriteRune(r[i])
					}
					continue
				}
				if r[i] == c {
					break
				}
			}
			continue
		}
		if c == '/' && i+1 < len(r) && r[i+1] == '/' {
			for i < len(r) && r[i] != '\n' {
				i++
			}
			if i < len(r) {
				b.WriteRune('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(r) && r[i+1] == '*' {
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Test1876NoSqliteSource 1.8.176：读飞牛 SQLite 那条来源必须整体删除。
//
// 结论（实测 + 读飞牛源码）：飞牛资源管理器**没有把目录排序持久化** ——
// 它的 folder_views 只在"视图模式/图标大小变化"时被顺带写一次
// （前端 Kw() 判断写不写时只比 viewMode 与 iconSize，sort/order 不参与），
// 单纯改排序不写库。所以那条来源在原理上拿不到当前排序。

// Test1877NoFnOsSortFile 1.8.177：恒返回 null 的 FnOsSort 整条链必须已删除。
//
// 结论（实测 + 读飞牛源码）：飞牛资源管理器**没有把目录排序持久化** ——
// 它的 folder_views 只在"视图模式/图标大小变化"时被顺带写一次
// （前端 Kw() 判断写不写只比 viewMode 与 iconSize，sort/order 不参与），
// 而且它主要把排序当"列目录请求的参数"。
// 三条来源（浏览器 IndexedDB / 签名 WebSocket / 后端读它的 SQLite）全部无效 ——
// 不要再引入第四个，也不要让任何一条回来。

// Test1884RestoresFnOsSortReader 1.8.181：必须保留"读飞牛前端 IndexedDB 偏好"的能力。
//
// 1.8.165 之所以"准"，就是因为大图路径会读它拿 {sortField, sortType}。
// 1.8.177 误删过（判断它恒返回 null —— 那是只看了飞牛服务端代码得出的错结论；
// 这份 IndexedDB 由**前端**读写，已在 /usr/trim/www/assets/openDefaultApps-*.js 中
// 确认存在 createObjectStore('file-view-preferences', {keyPath:[userId,scope,surface,location]})
// 以及 sortField/sortType 的读写）。
func Test1884RestoresFnOsSortReader(t *testing.T) {
	if _, err := os.Stat("web/fnos_sort.js"); err != nil {
		t.Error("web/fnos_sort.js 缺失 —— 大图排序要靠它读飞牛前端写的 IndexedDB 偏好")
	}
	idx, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), "fnos_sort.js") {
		t.Error("index.html 没有引入 fnos_sort.js —— 偏好读取器不会被加载")
	}
	js, err := os.ReadFile("web/fnos_sort.js")
	if err != nil {
		t.Fatal(err)
	}
	s := stripJSComments(string(js))
	for _, want := range []string{"trim-file-manager", "file-view-preferences", "indexedDB", "getDirSort"} {
		if !strings.Contains(s, want) {
			t.Errorf("fnos_sort.js 缺少 %s —— 读不到偏好，大图只能回退 name:asc", want)
		}
	}
	// 1.8.190：前端重排**必须存在**，且规则必须是飞牛用的 Intl.Collator("zh")。
	//
	// 历史：1.8.180 曾把前端重排整个删掉，理由是"后端 naturalLess 才与飞牛逐字一致"。
	// **那个结论是错的** —— 后端的 naturalLess 是**字节序**，会把**所有汉字排到最后**，
	// 而飞牛对名称用的是 a.name.localeCompare(b.name, "zh")，ICU 的层级是
	//     标点 < 数字 < 汉字（按拼音）< 小写 < 大写 < …
	// 实测真实目录（NO.072，76 个文件）：
	//     后端 ['DSC06619.jpg', …]（汉字在第 76 位）
	//     飞牛 ['小吉的快乐野餐封面.jpg', 'DSC06619.jpg', …]（第 1 位）
	// 这就是用户报的"有中文和字母时按名称排序不对"。
	// 而 Go 的 golang.org/x/text/collate(zh) 实测与 ICU 不一致（"A" vs "中"：
	// Go 判 A 在前、Node 判 中 在前），且 ICU 层级细到标点 16 类 + 全角/希腊/
	// 西里尔/韩文/假名各有位置 —— 手写复刻不现实，只能在浏览器里用 Intl.Collator。
	ap, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	aps := stripJSComments(string(ap))
	// 1.8.191：必须带 numeric: true（数字按值排，用户要求）。
	// 断言不带右括号 —— 否则加了 { numeric: true } 之后子串就不匹配了。
	if !strings.Contains(aps, "new Intl.Collator('zh'") {
		t.Error("前端重排没有用 Intl.Collator(\"zh\"…) —— 中文与拉丁的混排会与飞牛不一致")
	}
	if !strings.Contains(aps, "{ numeric: true }") {
		t.Error("缺少 numeric: true —— 数字会按字符串排（1,10,100），用户要求按值排（1,2,10,100）")
	}
	if !strings.Contains(aps, "resortLikeFnOs(") {
		t.Error("缺少 resortLikeFnOs —— 前端没有按飞牛规则重排")
	}
	// 大图路径必须真的去读偏好
	if !strings.Contains(aps, "window.FnOsSort.getDirSort(") {
		t.Error("resolveSortForDir 没有调用 window.FnOsSort.getDirSort() —— 大图排序无从获取")
	}
}
