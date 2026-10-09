package main

import (
	"os"
	"strings"
	"testing"
)

// 1.8.62：网格只对「用户看得到的行」请求缩略图，滚动缓冲行不许预取。
//
// 背景（真实 Edge 实测，本文件下的结论全部来自端到端探针 perf3/，非推算）：
//
//	窗口 1200x800 / 目录 200 张：修复前请求 36 张（可见 4 行 24 张 + 缓冲 2 行 12 张）
//	窗口 1600x1000 / 目录 200 张：修复前请求 56 张（可见 5 行 40 张 + 缓冲 2 行 16 张）
//	短目录（30 张）则整个目录都被请求
//
// 修复后两个场景分别是 24 张与 40 张，与实际可见张数一致；
// DOM 里的元素数不变（36 / 56），所以滚动时不会出现空白 —— 元素照建，只是不发请求。
//
// 关键：`<img loading="lazy">` 对这套网格**无效**（实测缓冲行的图也全部下载完成），
// 因为格子用 position:absolute + translate3d 定位、且都在同一个滚动容器里，
// 浏览器的懒加载判定不把它们当成"视口外"。所以必须显式判定，不能靠 lazy 属性。
func Test1862GridRequestsVisibleRowsOnly(t *testing.T) {
	s := readAppJS(t)

	// ① 按需加载函数必须在（断言完整签名 —— 只匹配前缀会被「改名加后缀」绕过）
	if !strings.Contains(s, "function loadCellImage(el, eager) {") {
		t.Error("app.js 里没有 loadCellImage(el, eager) —— 缓冲行会退回「创建即请求」，" +
			"每打开一个目录都多解码一整屏（实测 12~16 张）")
	}
	// ② 必须真的算出了「可见行」范围，而不是只有缓冲范围
	if !strings.Contains(s, "viewStartRow") || !strings.Contains(s, "viewEndRow") {
		t.Error("renderVisible 没有区分「可见行」与「缓冲行」—— 无法只请求可见的那批")
	}
	// ③ loading=lazy 已被实测证伪，不许再出现
	if strings.Contains(s, "img.loading = 'lazy'") || strings.Contains(s, `img.loading = "lazy"`) {
		t.Error("仍在依赖 loading=lazy —— 真实 Edge 实测对它无效" +
			"（缓冲行的图照样全部下载：36/36、56/56 张 complete）")
	}
	// ④ createCell 必须接收 eager，否则创建时就无法区分
	if !strings.Contains(s, "function createCell(item, idx, eager)") {
		t.Error("createCell 没有 eager 参数 —— 缓冲行与可见行无法区分")
	}
	// ⑤ 滚进可见区必须补发请求，否则滚动后是空白
	if !strings.Contains(s, "loadCellImage(el, eager)") {
		t.Error("renderVisible 复用的格子上没有补发调用 —— " +
			"缓冲行滚进可见区后不会加载，用户会看到空白格子")
	}
}

// 缩略图 URL 必须先挂在 data-src 上，由 loadCellImage 决定何时赋给 src。
// 这条 guard 防的是「有人把 data-src 改回 img.src」，那等于撤销整个修复。
func Test1862ThumbURLGoesToDataSrcFirst(t *testing.T) {
	s := readAppJS(t)

	if !strings.Contains(s, "img.setAttribute('data-src', thumbURL(") {
		t.Error("缩略图 URL 没有先挂到 data-src —— 应改为 setAttribute('data-src', thumbURL(...))")
	}
	if strings.Contains(s, "img.src = thumbURL(") {
		t.Error("createCell 仍然直接给 img.src 赋值 —— 等于「创建即请求」，" +
			"缓冲行会重新开始预取（实测多出 12~16 张/屏）")
	}
	// 1.8.96 起不直接赋 src，而是过一道并发闸（同源只有 6 条连接，不能让网格占满）
	if !strings.Contains(s, "enqueueGridThumb(img, url)") {
		t.Error("loadCellImage 没有把 data-src 交给并发闸 —— 图片永远不会加载")
	}
	// 必须防重复请求（滚动会反复调用 renderVisible）。
	// 断言完整的判断语句与赋值语句 —— 只写 "img.dataset.mvReq" 会被改名绕过。
	if !strings.Contains(s, "if (!img || img.dataset.mvReq) return;") {
		t.Error("loadCellImage 没有防重复判断 —— 滚动时同一张会被反复请求")
	}
	if !strings.Contains(s, "img.dataset.mvReq = '1';") {
		t.Error("loadCellImage 没有置位 mvReq —— 防重复标记形同虚设")
	}
}

func readAppJS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatalf("读不到 web/app.js：%v", err)
	}
	return string(b)
}
