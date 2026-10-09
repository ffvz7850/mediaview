package main

import (
	"net/http"
	"strings"
	"testing"
)

// 二维码必须是**独立图片资源**，不能退回内嵌 base64。
//
// 背景：1.8.43 把两张二维码以 base64 贴在 index.html 里（291 KB，占该文件 96%）。
// 而 frontend.go 对文本资源按设计用 no-cache + 内容 ETag —— 虽然 body 能靠 304 省掉，
// 但每次请求服务端都要把整个 HTML 读进内存、做版本占位符替换、再算一遍 sha256；
// 更重要的是图片被绑在 HTML 上，无法享受 immutable 强缓存。
// 拆成独立文件后：index.html 回到约 13 KB，图片走一年 immutable，首次之后不再请求。
func TestQRCodesAreSeparateAssets(t *testing.T) {
	html := fetchFrontend(t, "/index.html").Body.String()

	if strings.Contains(html, "data:image") {
		t.Error("index.html 里又出现了内嵌 base64 图片 —— HTML 会重新膨胀，图片也失去强缓存")
	}
	// 两张图必须能通过 embed 访问，且体积合理（不是占位空文件）
	for _, p := range []string{"/qr-alipay.png", "/qr-wechat.png"} {
		rec := fetchFrontend(t, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 无法通过 embed 访问：状态码 %d", p, rec.Code)
			continue
		}
		if rec.Body.Len() < 5000 {
			t.Errorf("%s 内容异常：只有 %d 字节", p, rec.Body.Len())
		}
	}
	// HTML 必须引用它们，并且带上版本占位符（服务端会注入真实版本号 → 升级后 URL 变化 → 重新下载）
	for _, s := range []string{`qr-alipay.png?v=`, `qr-wechat.png?v=`} {
		if !strings.Contains(html, s) {
			t.Errorf("index.html 里没有正确引用 %s", s)
		}
	}
	// 占位符不能残留（服务端替换后应当消失）
	if strings.Contains(html, versionPlaceholder) {
		t.Errorf("发出去的 index.html 里仍残留版本占位符 %s —— 图片 URL 会带上未替换的串", versionPlaceholder)
	}
}
