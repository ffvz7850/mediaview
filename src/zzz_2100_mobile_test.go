package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

var mv2100ClassAttrRe = regexp.MustCompile(`class="([^"]*)"`)

// mv2100HasClassToken 判断 HTML 里是否存在**恰好等于** name 的 class token。
//
// ⚠️ 不能用 `strings.Contains(html, "v-back")`：那会**误伤自己人** ——
// 网格那个按钮原本的类名是 `nav-back`，而 `nav-back` 里就包含子串 `v-back`，
// 于是"查看器里不该有返回按钮"这条断言被自己的类名满足，报出一个莫名其妙的 FAIL。
// （本版实测踩到；这也是"子串断言会重叠"的一个具体形态。）
func mv2100HasClassToken(html, name string) bool {
	for _, m := range mv2100ClassAttrRe.FindAllStringSubmatch(html, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if tok == name {
				return true
			}
		}
	}
	return false
}

// ============================================================================
// 2.1.0 手机适配的闸门测试
//
// 这一版的目标是"**手机上增强手势、电脑一点不动**"。光靠"我改的时候很小心"
// 保证不了，所以这里把三件事写成**可证伪**的闸门：
//
//	① 触屏模块必须是【动态注入】—— index.html 里不得出现 touch.js 的静态 script 标签
//	   （静态标签会让桌面也下载并执行整块手势代码）；
//	② 手机样式必须**全部**包在 @media (pointer: coarse) 里 —— 判据是输入能力而不是窗口宽度，
//	   否则桌面把窗口拖窄就会命中，等于把手机样式搅和到电脑上；
//	③ 手势逻辑真跑起来对不对 —— 用 node 把一个假 window/document 塞给 touch.js，
//	   真的是捏合/边缘手势事件，断言它调了哪些接口。
//
// ③ 是这一版最重要的一条：静态字符串断言只能证明"文件里提过 x"，
// 证明不了"x 被这样用了"（1.8.173 变异实测栽过）。
// ============================================================================

// mustReadWebFile 读 web/ 下的文件；**缺文件直接 Fatal 而不是 Skip** ——
// touch.js 是本版新增的产物，被删掉时应该红，不该变成一条安静的 SKIP。
func mustReadWebFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("web/" + name)
	if err != nil {
		t.Fatalf("读不到 web/%s：%v（它是本版依赖的文件，缺了必须红）", name, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

type mv2100CSSBlock struct{ prelude, body string }

// mv2100StripCSSComments 去掉 CSS 块注释。
//
// ⚠️ 这不是可选的洁癖：本文件第一版就在这上面栽过 —— **注释里写的 `@media (max-width: ...)`
// 被解析器当成了真的媒体查询**，于是它把紧随其后的 `#navBack, #vBack { ... }` 整条
// 当成"媒体块内容"挖掉了，测试报出一句莫名其妙的"顶层缺少这条规则"。
// 教训：凡是要"解析"源码的断言，第一步就得把注释剥掉 —— 否则注释会自己满足判据。
func mv2100StripCSSComments(css string) string {
	var b strings.Builder
	for {
		i := strings.Index(css, "/*")
		if i < 0 {
			b.WriteString(css)
			return b.String()
		}
		b.WriteString(css[:i])
		j := strings.Index(css[i+2:], "*/")
		if j < 0 {
			return b.String()
		}
		css = css[i+2+j+2:]
		b.WriteString(" ")
	}
}

// mv2100MediaBlocks 抽出 style.css 里所有 @media 块（prelude = @media 到 '{' 之间）。
func mv2100MediaBlocks(css string) []mv2100CSSBlock {
	css = mv2100StripCSSComments(css)
	var out []mv2100CSSBlock
	i := 0
	for {
		j := strings.Index(css[i:], "@media")
		if j < 0 {
			return out
		}
		j += i
		open := strings.Index(css[j:], "{")
		if open < 0 {
			return out
		}
		open += j
		depth, end := 0, -1
		for k := open; k < len(css); k++ {
			if css[k] == '{' {
				depth++
			} else if css[k] == '}' {
				depth--
				if depth == 0 {
					end = k
					break
				}
			}
		}
		if end < 0 {
			return out
		}
		out = append(out, mv2100CSSBlock{
			prelude: strings.TrimSpace(css[j:open]),
			body:    css[open+1 : end],
		})
		i = end + 1
	}
}

// mv2100StripMediaBlocks 把所有 @media 块整体挖掉，只留顶层规则。
func mv2100StripMediaBlocks(css string) string {
	css = mv2100StripCSSComments(css)
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(css[i:], "@media")
		if j < 0 {
			b.WriteString(css[i:])
			return b.String()
		}
		j += i
		b.WriteString(css[i:j])
		open := strings.Index(css[j:], "{")
		if open < 0 {
			return b.String()
		}
		open += j
		depth, end := 0, -1
		for k := open; k < len(css); k++ {
			if css[k] == '{' {
				depth++
			} else if css[k] == '}' {
				depth--
				if depth == 0 {
					end = k
					break
				}
			}
		}
		if end < 0 {
			return b.String()
		}
		i = end + 1
	}
}

// ---------------------------------------------------------------------------
// ① 触屏模块必须动态注入
// ---------------------------------------------------------------------------

func Test2100TouchModuleIsNotStaticallyLinkedFromHTML(t *testing.T) {
	html := mustReadWebFile(t, "index.html")
	if strings.Contains(html, "touch.js") {
		t.Fatal("index.html 里出现了 touch.js —— 必须是 app.js 在触屏档位**动态注入**。" +
			"静态 <script src> 会让桌面也下载并执行整块手势代码，那就'搅和'了电脑")
	}
}

func Test2100TouchModuleInjectedOnlyForTouchProfile(t *testing.T) {
	code := stripLineComments(mustReadWebFile(t, "app.js"))

	iGuard := strings.Index(code, "if (PROFILE === 'touch') {")
	if iGuard < 0 {
		t.Fatal("app.js 里找不到 `if (PROFILE === 'touch') {` 门控 —— 触屏分支没有分档条件")
	}
	iInject := strings.Index(code, "document.createElement('script')")
	if iInject < 0 {
		t.Fatal("app.js 里找不到动态建 script（document.createElement('script')）—— 注入方式被改掉了")
	}
	if iInject < iGuard {
		t.Fatal("touch.js 的注入没有落在 PROFILE === 'touch' 门控之后 ⇒ 桌面也会加载它")
	}
	if !strings.Contains(code, "touch.js?v=__MV_VERSION__") {
		t.Fatal("注入的 URL 必须带 `?v=__MV_VERSION__`：资源文件名固定，" +
			"不带版本号时浏览器会长期吃旧文件（历史上正是这么出过故障）")
	}
}

// 分档判据必须是【能力探测】，不能用 UA 嗅探 ——
// 用户在手机上开「请求桌面版网站」会把 UA 改掉，而 pointer/hover 反映的是物理输入能力。
func Test2100ProfileUsesCapabilityNotUserAgent(t *testing.T) {
	code := stripLineComments(mustReadWebFile(t, "app.js"))
	for _, want := range []string{"(pointer: coarse)", "(hover: none)", "maxTouchPoints"} {
		if !strings.Contains(code, want) {
			t.Fatalf("分档判据缺少 %q —— 三项能力探测缺一不可（少一项就会把某些设备判错档）", want)
		}
	}
	if strings.Contains(code, "userAgent") {
		t.Fatal("分档里出现了 UA 嗅探：它会被「请求桌面版网站」改掉，" +
			"而 pointer:coarse 反映物理输入能力、改不掉 —— 不要退回 UA 判断")
	}
}

// ---------------------------------------------------------------------------
// ② 手机样式必须全部包在 @media (pointer: coarse) 里
// ---------------------------------------------------------------------------

// 输入驱动的属性：写错位置就会"搅和"到桌面。
//
//	· touch-action         —— 决定浏览器是否把触摸交给脚本（鼠标无关，但语义属于触屏）
//	· overscroll-behavior  —— 触屏越界手势的开关
//
// 注意 style.css 里有一条历史遗留 `.v-media-wrap { touch-action: pan-y; }`（网格滚动容器用），
// 它对鼠标完全无影响，作为白名单保留；**新增**的这类属性一律必须在 coarse 块内。
var mv2100InputProps = []string{"touch-action", "overscroll-behavior", "-webkit-touch-callout", "-webkit-tap-highlight-color"}

func Test2100InputDrivenCssIsScopedToCoarsePointer(t *testing.T) {
	css := mustReadWebFile(t, "style.css")

	// (a) 顶层（非 @media）里不得出现新增的输入驱动属性
	top := mv2100StripMediaBlocks(css)
	top = strings.ReplaceAll(top, "touch-action: pan-y", "") // 白名单：.v-media-wrap 的历史规则
	for _, p := range mv2100InputProps {
		if strings.Contains(top, p) {
			t.Fatalf("style.css 顶层出现了 %q —— 输入驱动的属性必须放进 "+
				"@media (pointer: coarse)，否则鼠标设备也会命中（窄窗口就是典型的踩法）", p)
		}
	}

	// (b) 宽度媒体查询里不得出现输入驱动属性：宽度 ≠ 输入能力
	blocks := mv2100MediaBlocks(css)
	for _, b := range blocks {
		if !strings.Contains(b.prelude, "max-width") {
			continue
		}
		for _, p := range mv2100InputProps {
			if strings.Contains(b.body, p) {
				t.Fatalf("%s 块里出现了 %q —— 手机样式不要挂在宽度上："+
					"桌面把窗口拖窄到同样宽度会一起命中", b.prelude, p)
			}
		}
	}

	// (c) 正例：必须真的存在 coarse 块，且查看器的 touch-action:none 在里面
	found := false
	for _, b := range blocks {
		if !strings.Contains(b.prelude, "pointer: coarse") {
			continue
		}
		found = true
		if strings.Contains(b.body, "#vStage") && strings.Contains(b.body, "touch-action: none") {
			return
		}
	}
	if !found {
		t.Fatal("style.css 里没有任何 @media (pointer: coarse) 块 —— 手机样式全丢了")
	}
	t.Fatal("缺少 `#vStage { touch-action: none }`：" +
		"不加这条，双指捏合会被浏览器当成页面缩放、单指滑动中途会被发 pointercancel（翻页随机失败）")
}

// Test2100TouchOnlyReturnButtons 网格左上角那个「返回上级目录」按钮：
// 只在触屏档位出现，桌面不可见，并且行为真的接上了。
//
// ⚠️ 同时**反向**钉住一件事：**大图查看器里不许再有返回按钮**。
// 2.1.1 按用户要求去掉了它 —— 手机飞牛窗口自带返回箭头，从文件管理器点开单图时
// 它等价于"关闭看图窗口"。留一条反向断言，是为了防止以后有人"顺手补一个"回去
// （那会让查看器里多一个和宿主重复的按钮）。
func Test2100TouchOnlyReturnButtons(t *testing.T) {
	html := mustReadWebFile(t, "index.html")
	if !strings.Contains(html, `id="navBack"`) {
		t.Fatal(`index.html 缺少网格的「返回上级目录」按钮 id="navBack"`)
	}
	if strings.Contains(html, `id="vBack"`) || mv2100HasClassToken(html, "v-back") {
		t.Fatal("查看器里又出现了返回按钮（#vBack / .v-back）—— 2.1.1 已按用户要求去掉：" +
			"手机飞牛窗口自带返回箭头，查看器内退回网格走宿主窗口的返回箭头，" +
			"页面内不再有手势、也不该再多一个重复的按钮")
	}

	css := mustReadWebFile(t, "style.css")
	// 默认隐藏（否则桌面也会长出一个 ← 按钮 —— 那就是"搅和"了）
	top := mv2100StripMediaBlocks(css)
	if !strings.Contains(top, "#navBack { display: none; }") {
		t.Fatal("style.css 顶层缺少 `#navBack { display: none; }`：" +
			"没有这条，桌面也会显示这个按钮")
	}
	// 只有 coarse 块里才显示
	shown := false
	for _, b := range mv2100MediaBlocks(css) {
		if strings.Contains(b.prelude, "pointer: coarse") && strings.Contains(b.body, "#navBack {") {
			shown = true
		}
	}
	if !shown {
		t.Fatal("没有在 @media (pointer: coarse) 里显示 #navBack —— 手机上就看不到这个按钮了")
	}

	// 行为要接上（文字存在 ≠ 接了线）
	code := stripLineComments(mustReadWebFile(t, "app.js"))
	if !strings.Contains(code, "$('navBack')") {
		t.Fatal("app.js 没有给 #navBack 接线 —— 按钮点下去不会有任何反应")
	}
}

// ---------------------------------------------------------------------------
// ③ 触屏手势真跑一遍（node + 假 DOM）
// ---------------------------------------------------------------------------

// Test2100TouchGestureBehavior 用 node 真实执行 web/touch.js：
// 把假的 window/document 塞进去，真的派发 pointerdown/move/up，
// 断言它**调了哪些接口**。这是唯一能证明"手势逻辑真的这么跑"的手段 ——
// 静态断言挡不住"改一行就让功能失效"的回归。
//
// ⚠️ 2.2.0 起本模块**只做双指捏合**：右边缘横滑返回整块删除（用户要求）。
//
//	所以下面既验"捏合该怎么跑"，也**反向**验"右边缘横滑不许再接管任何东西"。
func Test2100TouchGestureBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	// ⚠️ 这是 Go 的 raw string（反引号），里面不能出现反引号；
	//    也不要写 \n（会变成两个字符）—— 拼接一律用 ';'。
	script := `
var fs = require('fs');
var SRC = fs.readFileSync('web/touch.js', 'utf8');

function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL ' + msg + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function ok(c, msg) { if (!c) { console.error('FAIL ' + msg); process.exit(1); } }
function cnt(rec, prefix) {
  var n = 0;
  for (var i = 0; i < rec.length; i++) { if (String(rec[i]).indexOf(prefix) === 0) n++; }
  return n;
}

// makeApi：__mvMobile 的假实现。
// ⚠️ 这里**故意不放** closeViewer / goUpLevel —— 真实 app.js 在 2.2.0 也不再暴露它们。
//    若哪天有人把"返回动作"又接回手势，这两个桩的缺失会让下面的反向断言立刻变红。
function makeApi(rec) {
  var st = { scale: 1, tx: 0, ty: 0 };
  var api = {
    version: 'stub-version',
    viewerOpen: function () { return true; },
    stageRect: function () { return { left: 0, top: 0, width: 400, height: 300 }; },
    view: function () { return { scale: st.scale, tx: st.tx, ty: st.ty }; },
    maxScale: function () { return 6; },
    claimGesture: function () { rec.push('claim'); },
    releaseGesture: function () { rec.push('release'); },
    setView: function (s, tx, ty) { st.scale = s; st.tx = tx; st.ty = ty; rec.push('setView:' + s); return true; },
    commitView: function () { rec.push('commit'); }
  };
  api._st = st;
  return api;
}

// load：把 touch.js 装进一个假环境。
//   timerMode='sync'  —— setTimeout 立刻执行（用于"拿不到接口"那类要跑满轮询的用例）
//   timerMode='queue' —— 定时器进队列，由 flush() 手动推进
function load(api, timerMode) {
  var L = {};
  var queue = [];
  // 注意：2.2.0 的 touch.js 只找 vStage（不再找 viewer —— 它唯一的用途是给边缘手势
  // 做跟手位移，那条手势已经删了）。这里就按真实契约只给 stage。
  var stage = { addEventListener: function (t, fn) { (L[t] = L[t] || []).push(fn); } };
  var win = {
    setTimeout: function (fn, ms) {
      if (timerMode === 'sync') { fn(); return 0; }
      if (timerMode === 'queue') { queue.push(fn); return queue.length; }
      return setTimeout(fn, ms || 0);
    },
    addEventListener: function () {},
    __mvMobile: api
  };
  var doc = {
    getElementById: function (id) {
      if (id === 'vStage') return stage;
      return null;
    }
  };
  new Function('window', 'document', 'console', SRC)(win, doc, console);
  return {
    L: L, win: win,
    flush: function () { while (queue.length) { queue.shift()(); } }
  };
}

var PLAIN = { closest: function () { return null; } };
function fire(env, type, id, x, y, target) {
  var fns = env.L[type] || [];
  for (var i = 0; i < fns.length; i++) {
    fns[i]({ pointerId: id, clientX: x, clientY: y, target: target || PLAIN });
  }
}

// --- 1. 拿不到 __mvMobile：什么都不许装（宁可不生效，也不要半套手势）---
var env0 = load(undefined, 'sync');
ok(env0.win.__mvTouchLoaded === true, 'touch.js 应设置"已加载"标记（用于验证桌面没加载它）');
eq(env0.win.__mvTouchReady, undefined, '拿不到 __mvMobile 时不得安装手势');
ok(!env0.L.pointerdown, '拿不到 __mvMobile 时不得注册任何监听');

// --- 2. 正常装上：暴露诊断信息 + 注册监听 ---
var rec = [], api = makeApi(rec), env = load(api, false);
ok(env.win.__mvTouchReady && env.win.__mvTouchReady.version === 'stub-version', '装上后应暴露诊断信息');
ok(!!env.L.pointerdown && !!env.L.pointerup, '应注册 pointerdown/pointerup');

// --- 3. 单指在中间起手：**不得**让 app.js 让位（单指滑动切图仍归 app.js）---
fire(env, 'pointerdown', 1, 200, 150);
eq(rec.indexOf('claim'), -1, '单指在中间起手不得抢手势（否则滑动切图会被抢走）');

// --- 4. 双指捏合：必须让位 + 按距离缩放 + 焦点保持 ---
fire(env, 'pointerdown', 2, 250, 150);
ok(rec.indexOf('claim') >= 0, '第二根手指落下必须让 app.js 让位（否则捏合会顺手翻页）');
fire(env, 'pointermove', 1, 150, 150);
fire(env, 'pointermove', 2, 300, 150);
eq(api._st.scale, 3, '两指距离 50→150 应缩放 3 倍');
eq(api._st.tx, -50, '焦点保持：中点(225,150) 在 3 倍时应左移 50px');
eq(api._st.ty, 0, '垂直方向没有位移不该产生纵向平移');
fire(env, 'pointerup', 2, 300, 150);
eq(rec.indexOf('release'), -1, '还剩一根手指时不得提前解除让位');
fire(env, 'pointerup', 1, 150, 150);
ok(rec.indexOf('release') >= 0, '全部抬起后必须解除让位（否则 app.js 会一直"让位"，单指滑动永久失效）');
ok(rec.indexOf('commit') >= 0, '捏合结束要定稿一次（≥1.5x 才会触发原图升级）');

// --- 5. 视频不支持缩放：只许探测一次，不许每帧空转 ---
var recV = [], apiV = makeApi(recV);
apiV.setView = function (s) { recV.push('setView:' + s); return false; };
var envV = load(apiV, false);
fire(envV, 'pointerdown', 31, 100, 150);
fire(envV, 'pointerdown', 32, 150, 150);
fire(envV, 'pointermove', 31, 60, 150);
fire(envV, 'pointermove', 32, 200, 150);
eq(cnt(recV, 'setView'), 1, '视频（不支持缩放）只应探测一次，不该每帧都空转');

// --- 6. ★★ 2.2.0 的核心反向断言：**右边缘横滑不许再接管任何东西** ---
// 用户实测反复报过这个手势"快滑一种结果、慢滑另一种结果"（它与宿主自带的返回手势
// 抢同一条边缘，谁先判定谁生效）。现在整块删掉、返回改走左上角箭头 ——
// 这条确保它**真的**没了：真实调用记录必须**一条都没有**。
var recE = [], apiE = makeApi(recE), envE = load(apiE, 'queue');
fire(envE, 'pointerdown', 41, 390, 150);   // 从最右边缘起手
fire(envE, 'pointermove', 41, 300, 150);   // 明确向左横滑
fire(envE, 'pointerup', 41, 300, 150);
envE.flush();
eq(recE.length, 0, '右边缘横滑不得再调用任何接口（连让位都不该）—— 手势返回已整块删除');

// --- 7. 其余起手位置同样不得接管（原来只有右边缘接管，现在哪都不接管）---
var recL = [], apiL = makeApi(recL), envL = load(apiL, 'queue');
fire(envL, 'pointerdown', 11, 10, 150);
fire(envL, 'pointermove', 11, -40, 150);
fire(envL, 'pointerup', 11, -40, 150);
eq(recL.indexOf('claim'), -1, '左边缘起手不得抢手势（那是系统返回手势的地盘）');

// --- 8. 查看器没打开：不得接管（捏合只在查看器里有意义）---
var rec5 = [], api5 = makeApi(rec5);
api5.viewerOpen = function () { return false; };
var env5 = load(api5, 'queue');
fire(env5, 'pointerdown', 13, 390, 150);
fire(env5, 'pointerdown', 14, 380, 150);
fire(env5, 'pointermove', 13, 300, 150);
fire(env5, 'pointerup', 13, 300, 150);
eq(rec5.indexOf('claim'), -1, '查看器没打开时不得抢手势');

// --- 9. 落在大图控件上的起手：不得被当成手势起手 ---
var rec6 = [], api6 = makeApi(rec6), env6 = load(api6, 'queue');
var onControl = { closest: function (sel) { return sel.indexOf('.v-controls') >= 0 ? {} : null; } };
fire(env6, 'pointerdown', 21, 390, 150, onControl);
fire(env6, 'pointerdown', 22, 380, 150, onControl);
eq(rec6.indexOf('claim'), -1, '控件上的起手不得被当成手势（否则点按钮会误触发缩放）');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("touch.js 行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}

// Test2100ViewerYieldsToTouchGestures 真执行 app.js 的「让位守卫」。
//
// 守卫本身是 app.js 里的三行判断，看着无害，但**删掉它们的后果很重**：
//
//	· 两指捏合时 pointerup 逐指触发，第一指抬起时位移≈0 ⇒ 被判成"点击" ⇒ **捏合会翻页**；
//	· 桌面行为必须不变（只有一个 pointerId ⇒ 永远不进让位）。
//
// 所以这里把 mtCount/tgestureClaim/tgestureRelease/endSwipeGuarded 四个函数
// 从源码里抠出来真跑，两端行为都验一遍。
func Test2100ViewerYieldsToTouchGestures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');

function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 源码里找不到 ' + name); process.exit(1); }
  var depth = 0, j = appjs.indexOf('{', i);
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL ' + name + ' 括号不配平');
  process.exit(1);
}
function eq(a, b, msg) {
  if (a !== b) { console.error('FAIL ' + msg + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
function ok(c, msg) { if (!c) { console.error('FAIL ' + msg); process.exit(1); } }

// 只搭守卫需要的外部状态；endSwipe 换成计数器（我们验的是"它跑没跑"）
var code =
  'var mtDown = {};' +
  'var tgesture = { own: false, muteClickUntil: 0 };' +
  'var swipe = { active: false, track: null };' +
  'var endCalls = 0;' +
  'function endSwipe() { endCalls++; }' +
  grab('mtCount') + ';' + grab('tgestureClaim') + ';' +
  grab('tgestureRelease') + ';' + grab('endSwipeGuarded') + ';' +
  'return {' +
  '  guarded: endSwipeGuarded,' +
  '  claim: tgestureClaim,' +
  '  snap: function () { return { own: tgesture.own, mute: tgesture.muteClickUntil,' +
  '    endCalls: endCalls, swipeActive: swipe.active, track: swipe.track }; },' +
  '  arm: function () { swipe.active = true; swipe.track = {}; }' +
  '};';
var H = new Function(code)();

// --- 1. 桌面（鼠标）：只有一个 pointerId，行为必须与改动前**完全一致** ---
// 收尾守卫只挂在 pointerup/pointercancel 上（没有 down 侧的配对调用），
// 所以"一个触点"就是**一次** guarded 调用 —— 它必须原样转发给 endSwipe。
H.guarded({ pointerId: 1 });
eq(H.snap().endCalls, 1, '单指收尾必须照常调用 endSwipe（否则滑动切图/点击切图整条都失效）');
eq(H.snap().own, false, '单指不得进入让位状态（否则桌面单指滑动会被废掉）');

// --- 2. claim 必须丢弃 app.js 侧已开始的手势状态 ---
H.arm();
H.claim();
eq(H.snap().swipeActive, false, 'claim 必须把 swipe.active 清掉（否则轨道停在半路）');
eq(H.snap().track, null, 'claim 必须把 swipe.track 清掉');
eq(H.snap().own, true, 'claim 之后应处于让位状态');

// --- 3. 让位期间收尾：不跑 endSwipe，且全部抬起后解除让位 + 抑制 click ---
var before = H.snap().endCalls;
H.guarded({ pointerId: 2 });
eq(H.snap().endCalls, before, '让位期间的收尾不得跑 endSwipe（否则多指手势会被当成点击翻页）');
eq(H.snap().own, false, '全部触点抬起后必须解除让位（否则单指滑动永久失效）');
var delta = H.snap().mute - Date.now();
ok(delta > 250 && delta < 600, '解除让位时要设约 400ms 的 click 抑制窗，实际 ' + delta + 'ms');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js 让位守卫行为探针失败：%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("探针未输出 OK：%s", out)
	}
}
