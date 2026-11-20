package main

// 2.4.20：封面名称「半透明底 + 灰度自适应字色」。
//
// 用户诉求原话：「把白底改成半透明，然后字体稍微大一点点，注意反差色让文字显示的清晰些，
// 黑的多就白字，白的多就黑字，进行灰度自适应」。
//
// 落地方式（用户在三选一里选的是"压在封面底部（图透出来）"）：
//   · 名称压在封面底部的一条 rgba 半透明条上，图片会隐约透出来；
//   · 字色按**文字背后那一块图**（封面底部 30%）的平均灰度自动切：暗 ⇒ 白字、亮 ⇒ 黑字；
//   · 字号 11 → 13 → 15px（两轮反馈后的最终值）。
//
// 本文件守三层：
//
//	① 接线：onload 与"封面已在缓存"两条路径都要触发灰度判断，缓存要能失效；
//	② 行为（真跑 node）：阈值、取样区域、亮度权重、以及"算不出来"时的静默降级；
//	③ CSS：半透明底确实是半透明、两套配色齐备、字号已放大。

import (
	"os/exec"
	"strings"
	"testing"
)

// ---------- ① 接线 ----------

func Test2420CoverToneWiring(t *testing.T) {
	js := stripLineComments(readSourceOrSkip(t, "web/app.js"))
	cases := []struct{ frag, why string }{
		{"function pickCoverTone(mean, minCol)", "缺灰度→配色的判定函数（要两个入参：整体均值 + 最暗列）"},
		{"function sampleCoverTone(img)", "缺取样函数（把封面底部画到小画布上取平均灰度）"},
		{"function applyCoverTone(cover, ckey, img)", "缺上色函数（把结论变成 class）"},
		{"applyCoverTone(cover, ckey, cimg);", "★ 封面 onload 里没有触发灰度判断 —— 那么永远是默认配色"},
		{"applyCoverTone(cover, ckey, null);",
			"★ \"封面已在缓存\"那条快路径也要按缓存上色，否则每次进目录先闪一下默认配色"},
		{"coverToneCache[ckey] = tone;", "灰度结论必须缓存（每次重算会拖慢滚动）"},
		{"coverToneCache = {};", "换 URL 后要清空缓存（封面重新合成，结论可能变）"},
	}
	for _, c := range cases {
		if !strings.Contains(js, c.frag) {
			t.Errorf("缺少：%s（%s）", c.frag, c.why)
		}
	}

	// 小视图档位：两个切换按钮 + 初始化都必须同步 body.mv-small，
	// 漏掉任何一条路径，都会出现"进了小视图名称还是 15px"（或反过来）。
	if n := strings.Count(js, "syncCellModeClass();"); n != 3 {
		t.Errorf("★ syncCellModeClass() 应被调用 3 次（大/小视图按钮各一次 + 初始化一次），实际 %d 次", n)
	}

	// 2.4.48：封面记忆的清理收敛到 resetCoverMem()，由两个 bump 共用。
	//   （旧断言写死 function bumpThumbCacheBust() 空参数签名，签名一加参数就假失败。）
	i := strings.Index(js, "function resetCoverMem()")
	if i < 0 {
		t.Fatal("找不到 resetCoverMem：封面灰度结论必须在某处统一作废")
	}
	seg := js[i:]
	if j := strings.Index(seg, "\n  }"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "coverToneCache = {}") {
		t.Error("★ resetCoverMem 里必须清空 coverToneCache（否则换了封面还在用旧灰度结论）")
	}
	// 两个 URL 计数器都要走这条清理路径：封面真被重建时灰度结论必须作废
	n := strings.Count(js, "resetCoverMem();")
	if n < 2 {
		t.Errorf("★ resetCoverMem() 应在两个 bump 里各调一次（bumpThumb 的 !keepCover 分支 + bumpCover），实际 %d 处", n)
	}

	// 灰度判断不能挡住封面显示：失败路径要 return ''（CSS 保持默认深色条+白字）
	if !strings.Contains(js, "if (!cx) return '';") {
		t.Error("取不到 canvas context 时必须返回空（保持默认配色），不能抛错")
	}
	if !strings.Contains(js, "catch (e) {\n      return '';") && !strings.Contains(js, "catch (e) { return ''") {
		// 允许多行写法，这里放宽：只要有 catch + return ''
		if !strings.Contains(js, "catch (e)") {
			t.Error("取样必须包 try/catch（跨域污染 / canvas 不可用时不能把整个格子渲染搞崩）")
		}
	}
}

// ---------- ② 行为：真跑 node ----------

const toneGrabFn = `
function grab(name) {
  var i = appjs.indexOf('function ' + name + '(');
  if (i < 0) { console.error('FAIL 找不到 ' + name); process.exit(1); }
  var j = appjs.indexOf('{', i), depth = 0;
  for (var k = j; k < appjs.length; k++) {
    if (appjs[k] === '{') depth++;
    else if (appjs[k] === '}') { depth--; if (depth === 0) return appjs.slice(i, k + 1); }
  }
  console.error('FAIL 括号不配平'); process.exit(1);
}
`

func Test2420CoverToneBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + toneGrabFn + `

var pickCoverTone = grab('pickCoverTone');
var sampleCoverTone = grab('sampleCoverTone');
var applyCoverTone = grab('applyCoverTone');
var coverToneCache = {};

// --- 环境桩：document.createElement('canvas') 给一个假 2d 上下文 ---
var fake = { pixels: null, drawn: null, noCtx: false, throwOnDraw: false };
var document = {
  createElement: function (tag) {
    return {
      width: 0, height: 0,
      getContext: function () {
        if (fake.noCtx) return null;
        return {
          drawImage: function (img, sx, sy, sw, sh, dx, dy, dw, dh) {
            if (fake.throwOnDraw) throw new Error('tainted');
            fake.drawn = { sx: sx, sy: sy, sw: sw, sh: sh, dw: dw, dh: dh };
          },
          getImageData: function () { return { data: fake.pixels }; },
        };
      },
    };
  },
};

function eq(a, b, m) {
  if (a !== b) { console.error('FAIL ' + m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); process.exit(1); }
}
// 造一张纯色图（24×8，每像素 rgba）
function solid(r, g, b) {
  var d = [];
  for (var i = 0; i < 24 * 8; i++) d.push(r, g, b, 255);
  return d;
}
// 造一张"某一列是深色、其余是浅色"的图（getImageData 行优先：offset=(y*24+x)*4）
function mixedColumn(darkCol, brightV, darkV) {
  var d = [];
  for (var y = 0; y < 8; y++) {
    for (var x = 0; x < 24; x++) {
      var v = (x === darkCol) ? darkV : brightV;
      d.push(v, v, v, 255);
    }
  }
  return d;
}

eval(pickCoverTone);
eval(sampleCoverTone);
eval(applyCoverTone);

// ---- 阈值：暗 ⇒ 白字(dark)，亮 ⇒ 黑字(bright)。门槛 190（亮图那套没有底色兜底）----
eq(pickCoverTone(0), 'dark', '纯黑应白字');
eq(pickCoverTone(60), 'dark', '很暗应白字');
eq(pickCoverTone(189), 'dark', '阈值下沿应白字（中间偏亮仍走白字+深条）');
eq(pickCoverTone(190), 'bright', '阈值应黑字');
eq(pickCoverTone(255), 'bright', '纯白应黑字');

// ---- 最暗列兜底：整体够亮、但文字压着的某一段是深色细节 ⇒ 退回白字+深条 ----
// （亮图那套不铺底，没有底色可依，只能靠"每一列都不能太暗"来保证可读）
eq(pickCoverTone(220, 100), 'dark', '有一列很暗就不能用黑字（黑字会糊在那一段上）');
eq(pickCoverTone(220, 149), 'dark', '最暗列低于 150 仍要退回白字');
eq(pickCoverTone(220, 150), 'bright', '最暗列刚好 150 可以用黑字');
eq(pickCoverTone(220), 'bright', '单参数调用等价于 minCol=mean（只看整体）');

// ---- 取样区域：只取底部 30%（名称压在那里，取整张会把"上亮下暗"的图判反）----
var img = { naturalWidth: 320, naturalHeight: 400, width: 320, height: 400 };
fake.pixels = solid(10, 10, 10);
var tone = sampleCoverTone(img);
eq(tone, 'dark', '底部很暗 ⇒ 白字');
eq(fake.drawn.sh, 120, '取样高度应为高度的 30%（400×0.3=120）');
eq(fake.drawn.sy, 280, '取样起点应为 400-120=280（底部那条）');
eq(fake.drawn.sw, 320, '取满整宽');

// ---- 亮度权重：(200,255,0) 按 Rec.601 是 209（亮），RGB 算术平均只有 152（暗）----
// 用它钉住"按人眼亮度加权"：用算术平均会判反。
fake.pixels = solid(200, 255, 0);
eq(sampleCoverTone(img), 'bright', '(200,255,0) 应按 Rec.601 判为"亮"（算术平均会判成暗）');

// ---- 混合图：整体很亮、但有一列是深色细节 ⇒ 必须退回白字 ----
fake.pixels = mixedColumn(5, 246, 20);   // 第 5 列深色，其余接近纯白
eq(sampleCoverTone(img), 'dark', '整体亮但有一列深色 ⇒ 退回白字（否则黑字糊在那一段上）');

// ---- 图亮 ⇒ 黑字；图暗 ⇒ 白字（同一条链路）----
fake.pixels = solid(240, 240, 240);
eq(sampleCoverTone(img), 'bright', '接近纯白应黑字');

// ---- 降级：没有 canvas / 画布被污染 都必须返回空串（调用方保持默认配色）----
fake.noCtx = true;
eq(sampleCoverTone(img), '', '没有 canvas 应静默返回空');
fake.noCtx = false;
fake.throwOnDraw = true;
eq(sampleCoverTone(img), '', '画布被污染应静默返回空');
fake.throwOnDraw = false;

// ---- applyCoverTone：上色 + 缓存命中（第二次不重新取样）----
function coverStub() {
  var cls = {};
  return {
    classList: {
      add: function (c) { cls[c] = 1; },
      remove: function (a, b) { delete cls[a]; delete cls[b]; },
      has: function (c) { return !!cls[c]; },
      all: function () { return Object.keys(cls).join(','); },
    },
  };
}
var cv = coverStub();
fake.pixels = solid(20, 20, 20);
applyCoverTone(cv, '/a|1', img);
eq(cv.classList.has('mv-tone-dark'), true, '暗图应加 mv-tone-dark');
eq(cv.classList.has('mv-tone-bright'), false, '暗图不该同时有 bright');

// 换一张亮图、但用同一个 key 的缓存 → 不应该重算（调用方行为：同一封面不会变灰）
var cv2 = coverStub();
applyCoverTone(cv2, '/a|1', null);
eq(cv2.classList.has('mv-tone-dark'), true, '缓存命中应直接上色（img 传 null 也能上）');

// 不同 key + 亮图 → bright，且要清掉旧的 dark（同一个格子被复用的情况）
var cv3 = coverStub();
cv3.classList.add('mv-tone-dark');
fake.pixels = solid(250, 250, 250);
coverToneCache = {};
applyCoverTone(cv3, '/b|2', img);
eq(cv3.classList.has('mv-tone-bright'), true, '亮图应加 mv-tone-bright');
eq(cv3.classList.has('mv-tone-dark'), false, '切换 tone 时必须先移除旧的（否则两套配色同时生效）');

console.log('OK');
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("灰度自适应的行为探针失败：%v\n%s", err, out)
	}
}

// ---------- ③ CSS 契约 ----------

func Test2420CSSContract(t *testing.T) {
	css := readSourceOrSkip(t, "web/style.css")

	// 半透明底：必须是 rgba(...) 且 alpha < 1；不能再是不透明的纯色块
	label := cssRule(t, css, ".cell-label")
	if !strings.Contains(label, "rgba(") {
		t.Errorf("★ .cell-label 的底色必须是 rgba 半透明（图片要透出来），实际 %q", label)
	}
	if strings.Contains(label, "background:#") {
		t.Errorf("★ .cell-label 还是纯色底 —— 用户要的「半透明」没做到，实际 %q", label)
	}
	// 不再是 18px 的大渐变内边距：现在是一条紧凑的条
	if strings.Contains(label, "padding:18px") {
		t.Errorf(".cell-label 不该再用 18px 的渐变顶边距（现在是紧凑的半透明条），实际 %q", label)
	}

	// ★ 层叠顺序：.cell-cover-img 也是 position:absolute，且在 DOM 里排在名称**之后**。
	//   两者都是 z-index:auto 时按 DOM 顺序绘制 ⇒ 图片会把名称整条盖住。
	//   这是视觉对照截图抓出来的真 bug（2.4.14 起就存在：有封面就看不到名字，
	//   只有 📁 兜底时才看得见），必须显式抬高。
	if !strings.Contains(label, "z-index:2") && !strings.Contains(label, "z-index:1") {
		t.Errorf("★★ .cell-label 必须显式 z-index（否则封面图按 DOM 顺序盖住名称，名字在有封面时完全看不见）实际 %q", label)
	}

	// 亮图那套**不铺底**（2.4.21 按用户反馈改的：白蒙层压在亮图上是条扎眼的白带）
	brightLabel := cssRule(t, css, ".cell-cover.mv-tone-bright .cell-label")
	if !strings.Contains(brightLabel, "background:none") {
		t.Errorf("★★ 亮图那套必须 background:none —— 白蒙层会盖出一条白带、把封面割断（用户原话：影响封面效果）；实际 %q", brightLabel)
	}
	if strings.Contains(brightLabel, "rgba(255,255,255") || strings.Contains(brightLabel, "linear-gradient") {
		t.Errorf("★★ 亮图那套不能再有白色蒙层/渐变，实际 %q", brightLabel)
	}

	// 亮图必须用深色字（白色字压白图看不见），但不是纯黑 —— 纯反差太硬，用户要"淡一点"
	brightName := cssRule(t, css, ".cell-cover.mv-tone-bright .cell-label-name")
	if !strings.Contains(brightName, "color:#191c21") {
		t.Errorf("★ 图偏亮时应用接近黑但不纯黑的字色（#191c21），实际 %q", brightName)
	}
	for _, pure := range []string{"color:#000", "color:#111", "color:#000000"} {
		if strings.Contains(brightName, pure) {
			t.Errorf("★ 不要用纯黑（用户反馈「过于反差」，要淡一点），实际 %q", brightName)
		}
	}
	// 没有底色就得靠双层柔光（硬边 + 光晕）
	if !strings.Contains(brightName, "rgba(255,255,255") {
		t.Errorf("★ 亮图的黑字必须靠白色柔光托起来，实际 %q", brightName)
	}

	// 字号放大：11 → 13
	name := cssRule(t, css, ".cell-label-name")
	if !strings.Contains(name, "font-size:15px") {
		t.Errorf("★ 名称字号应放大到 15px（用户两轮反馈：11→13 还不够大），实际 %q", name)
	}
	// "字发灰"不只是颜色问题：字重也决定笔画站不站得住。13px/500 那套在深底细纹上读着发虚。
	if !strings.Contains(name, "font-weight:600") {
		t.Errorf("★ 名称字重应为 600 —— 只靠加亮字色救不了「发灰」，笔画得站住，实际 %q", name)
	}
	// 半透明挡不住花纹 → 需要一层柔光才清晰
	if !strings.Contains(name, "text-shadow:") {
		t.Errorf("名称需要 text-shadow 柔光（压在半透明条上看碎纹理也能读），实际 %q", name)
	}

	// 默认那套是"淡白"字（图偏暗时的配色），不是纯白：纯白 + 深条的反差太硬
	if !strings.Contains(name, "color:#f8fafc") {
		t.Errorf("★ .cell-label-name 的默认色应是近白 #f8fafc（不是纯白，也不能灰到 #e6e9ef 那档），实际 %q", name)
	}
	if strings.Contains(name, "color:#fff;") || strings.Contains(name, "color:#ffffff") {
		t.Errorf("★ 不要用纯白（用户反馈字色「过于反差」），实际 %q", name)
	}

	// 独立一行那套也一起放大，免得两种模式字号不一致
	below := cssRule(t, css, ".cell-name-below")
	if !strings.Contains(below, "font-size:15px") {
		t.Errorf("「独立一行」模式的字号也要跟着放大到 15px，实际 %q", below)
	}

	// 小视图（96px 格子）必须收一档：15px 的名称条在 96px 里占到近四成，
	// 封面被挤成一条 —— 小视图的意义是"一排看更多图"，名称不能抢戏。
	if !strings.Contains(css, ".mv-small .cell-label-name { font-size: 13px") {
		t.Error("★ 小视图档位缺失 —— 96px 的格子里名称条会占到近四成，封面被挤成一条")
	}
	if !strings.Contains(css, ".mv-small .cell-label { padding: 8px 5px 4px") {
		t.Error("★ 小视图连名称条的上留白也要收：只缩字号不缩 padding，条还是那么高")
	}
}
