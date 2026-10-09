package main

import (
	"strings"
	"testing"
)

// Test1950EveryImageHiddenUntilDecoded 露面闸门必须覆盖**所有**图片盒。
//
// 1.8.149 及以前：闸门只给 isCurrent 那一格（`if (isCurrent) img.style.visibility = 'hidden';`），
// 而切换目标盒是 isCurrent:false —— 它从 src 赋值那刻就可见，
// 浏览器边解码边逐层绘制，于是「图片从上往下刷出来」。
func Test1950EveryImageHiddenUntilDecoded(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	if strings.Contains(js, "if (isCurrent) img.style.visibility") {
		t.Error("闸门又只装给 isCurrent 那一格了 —— 切换目标盒会立即可见，必然从上往下刷")
	}
	if !strings.Contains(js, `img.style.visibility = 'hidden';`) {
		t.Error("createMediaBox 里没有无条件隐藏图片盒")
	}
}

// Test1950GateWaitsForDecodeNotComplete 露面判据必须是 decode()，不能是 complete。
func Test1950GateWaitsForDecodeNotComplete(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))

	i := strings.Index(js, "function revealImage(")
	if i < 0 {
		t.Fatal("找不到 revealImage —— 闸门没有收敛成唯一一处")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "img.decode()") {
		t.Error("revealImage 没有等 img.decode()")
	}
	if !strings.Contains(rest, "img._dropSpin") {
		t.Error("revealImage 露面时没有撤转圈 —— 会出现「图出来了圈还在」")
	}
	if !strings.Contains(rest, "classList.add('loaded')") {
		t.Error("revealImage 露面时没有打 loaded")
	}

	k := strings.Index(js, "function ensureMediaVisible(")
	if k < 0 {
		t.Fatal("找不到 ensureMediaVisible")
	}
	em := js[k:]
	if j := strings.Index(em, "\n  function "); j > 0 {
		em = em[:j]
	}
	if strings.Contains(em, "style.visibility") {
		t.Error("ensureMediaVisible 又在自己改 visibility —— 它必须只调 revealImage")
	}
	if strings.Contains(js, "_revealGuard") {
		t.Error("又出现了 _revealGuard（绑 load 就露面的第三条旁路）")
	}

	// 结构性不变量：整个前端只允许一处把图放出来
	if n := strings.Count(js, `style.visibility = ''`); n != 1 {
		t.Errorf("`style.visibility = ''` 出现了 %d 次，期望恰好 1 次"+
			"（闸门必须唯一，否则迟早再被旁路）", n)
	}
}

// Test1950RevealFallbackIsLongEnough 兜底不能短于最坏真实耗时。
func Test1950RevealFallbackIsLongEnough(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "var VIEWER_REVEAL_TIMEOUT = ")
	if i < 0 {
		t.Fatal("找不到 VIEWER_REVEAL_TIMEOUT")
	}
	rest := js[i:]
	rest = rest[:strings.Index(rest, ";")]
	num := 0
	for _, ch := range rest {
		if ch >= '0' && ch <= '9' {
			num = num*10 + int(ch-'0')
		}
	}
	// NAS 实测：1 亿像素冷转码 2.5~2.9s + 传输 + 浏览器解码 ≈ 4.5s。
	if num < 6000 {
		t.Errorf("VIEWER_REVEAL_TIMEOUT = %dms 太短 —— 会先于真实解码完成触发，"+
			"等于把「等解码」的闸门作废（实测最坏约 4.5s）", num)
	}
}

// Test1950FallbackStartsAfterLoad 兜底必须从「数据到位」之后起算，而不是从建盒起算。
func Test1950FallbackStartsAfterLoad(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function revealImage(")
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "addEventListener('load'") {
		t.Error("revealImage 没有在 load 之后再启动兜底计时 —— " +
			"传输慢时会先吃掉兜底配额，仍可能提前放行")
	}
	if strings.Contains(js, "if (isCurrent) setTimeout(reveal") {
		t.Error("又出现只给 isCurrent 的兜底定时器")
	}
}

// Test1950WarmOnlyCurrentBox 预热不得对整个轨道逐个 decode（3 份 1 亿像素并发解码）。
//
// 1.8.151 起 warmTrackBoxes 会遍历轨道 —— 但那只是**建"哪些张已经在轨道里"的索引**，
// 用来避免对它们重复发请求；真正调 decode 的只有当前格（其余交给 revealImage / 新建 Image）。
// 所以判据要精确到"有没有对轨道里的每个 _media 调 decode"。
func Test1950WarmOnlyCurrentBox(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function warmTrackBoxes(")
	if i < 0 {
		t.Fatal("找不到 warmTrackBoxes")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	// 禁止：遍历轨道并对每个 child 的 _media 调 decode
	if strings.Contains(rest, "children[i]._media") && strings.Contains(rest, "decode") {
		t.Error("warmTrackBoxes 又对整个轨道逐个 decode —— 冷启动会同时解码 3 份 1 亿像素" +
			"（各约 300MB），而且与目标格的 revealImage 重复")
	}
	// 必须热当前格
	if !strings.Contains(rest, "v.media") {
		t.Error("warmTrackBoxes 没有热当前格（v.media）")
	}
	// 必须用"在不在轨道里"来避免重复请求（1.8.151 的方向化预载依赖它）
	if !strings.Contains(rest, "inTrack") {
		t.Error("warmTrackBoxes 没有用 inTrack 判断哪些张已在轨道里 —— " +
			"轨道外的才需要新建 Image，否则会退回纯重复预取")
	}
}

// Test1950DropSpinImmediate 撤圈不得自己延迟（该由 revealImage 统一决定时机）。
//
// 1.8.151 起语义是：转圈的最短停留仍然生效，但**不是让圈压在图上多转**，
// 而是把这个时长交给 revealImage，让它等够了一起结束 —— 这样既不会"圈瞬间闪一下"，
// 也不会"图出来了圈还在"。
func Test1950DropSpinImmediate(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "var dropSpin = function (")
	if i < 0 {
		t.Fatal("找不到 dropSpin")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n      };"); j > 0 {
		rest = rest[:j]
	}
	if strings.Contains(rest, "setTimeout") && strings.Contains(rest, "VIEWER_SPIN_MIN") {
		t.Error("dropSpin 又在自己延迟撤圈 —— 那会造成「图已画完、圈还压着」")
	}
	if !strings.Contains(rest, "classList.remove('show')") {
		t.Error("dropSpin 没有立即移除 show")
	}
	// 最短停留必须仍然存在，且由 revealImage 使用
	if !strings.Contains(js, "VIEWER_SPIN_MIN") {
		t.Error("VIEWER_SPIN_MIN 被删了 —— 圈刚出现就消失会造成瞬间闪屏")
	}
	if !strings.Contains(js, "img._spinWait") {
		t.Error("没有把「圈还需停多久」交给 revealImage —— 露面与撤圈无法同时结束")
	}
}

// Test1951WarmIsDirectionalAndCounted 方向化预载：按序号热"你正要去的那一侧"后 N 张。
func Test1951WarmIsDirectionalAndCounted(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	i := strings.Index(js, "function warmTrackBoxes(")
	if i < 0 {
		t.Fatal("找不到 warmTrackBoxes")
	}
	rest := js[i:]
	if j := strings.Index(rest, "\n  function "); j > 0 {
		rest = rest[:j]
	}
	if !strings.Contains(rest, "cfg.viewerPreload") {
		t.Error("warmTrackBoxes 没有按设置里的「大图预加载数量」决定预载张数")
	}
	if !strings.Contains(rest, "step") {
		t.Error("warmTrackBoxes 没有分清左右（缺少方向 step）")
	}
	if !strings.Contains(rest, "v.index + step * k") {
		t.Error("warmTrackBoxes 没有按**序号**取后续张（应该 v.index + step*k）")
	}
	if !strings.Contains(rest, "new Image()") {
		t.Error("轨道外的那几张必须新建 Image 才会真的发请求")
	}
}

// Test1950ContractReadIsLineEndingAgnostic 契约测试不得对行尾敏感。
func Test1950ContractReadIsLineEndingAgnostic(t *testing.T) {
	s := readSourceOrSkip(t, "zzz_1866_preview_pause_placeholder_test.go")
	if !strings.Contains(s, `ReplaceAll(string(b), "`) {
		t.Error("readSourceOrSkip 没有做行尾归一 —— 在 CRLF 检出上，" +
			"跨行字面量断言会假红（本项目的 app.js 曾出现 17 行 CRLF）")
	}
	if !strings.Contains(s, `
`) {
		t.Error("readSourceOrSkip 的归一没有针对 CRLF")
	}
}
