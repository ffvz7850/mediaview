package main

// 2.4.50 测试债补齐（二次审查 #14）：
// 2.4.49 的头条修复「清空缩略图不要让封面白闪」的成败关键在
// bumpThumbCacheBust(keepCover) 的**函数体**是否尊重 keepCover —— 但此前所有测试
// 只钉"调用点传了 true"（字符串）与"resetCoverMem 出现 ≥2 次"（计数），
// 把函数体改回无条件 resetCoverMem()，351 个测试仍全绿。
//
// 这条探针把断言从"源码文本"下沉到"函数体行为"：真实 eval 三个函数，
// 用 localStorage + coverSeen/coverToneCache 桩观察副作用。
// 反向验证：把 app.js 副本里 `if (!keepCover)` 删掉，本测试必须变红。

import (
	"os/exec"
	"testing"
)

func Test2450BumpThumbKeepCoverPreservesCoverMem(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node 不可用：%v", err)
	}
	script := `
var fs = require('fs');
var appjs = fs.readFileSync('web/app.js', 'utf8');
` + coverGrabFn + `

// ---- 环境桩：三个函数依赖的全部外部符号 ----
var LS = {};
var localStorage = {
  getItem: function (k) { return Object.prototype.hasOwnProperty.call(LS, k) ? LS[k] : null; },
  setItem: function (k, v) { LS[k] = String(v); },
  removeItem: function (k) { delete LS[k]; }
};
var thumbCacheBust = 0, coverCacheBust = 0;
var coverSeen = {}, coverSeenOrder = [];
var coverToneCache = {};

eval(grab('resetCoverMem'));
eval(grab('bumpThumbCacheBust'));
eval(grab('bumpCoverCacheBust'));

function fail(m) { console.error('FAIL ' + m); process.exit(1); }
function eq(a, b, m) { if (a !== b) fail(m + ' | expect ' + JSON.stringify(b) + ' got ' + JSON.stringify(a)); }

function seed() {
  thumbCacheBust = 0; coverCacheBust = 0;
  coverSeen = { '/vol1/1000/a': 1, '/vol1/1000/b': 1 };
  coverSeenOrder = ['/vol1/1000/a', '/vol1/1000/b'];
  coverToneCache = { '/vol1/1000/a|1': 'warm', '/vol1/1000/b|1': 'cool' };
  LS['mediaview_cover_seen'] = JSON.stringify({ order: coverSeenOrder.slice() });
  LS['mediaview_thumb_bust'] = '0';
  LS['mediaview_cover_bust'] = '0';
}

// ① keepCover=true：只 bump 缩略图计数，封面记忆（三处）必须原样保留
seed();
bumpThumbCacheBust(true);
eq(thumbCacheBust, 1, 'keepCover 也必须 bump thumbCacheBust');
eq(LS['mediaview_thumb_bust'], '1', 'thumb bust 必须持久化');
eq(Object.keys(coverSeen).length, 2, '★ keepCover=true 后 coverSeen 不得被清空');
eq(coverSeenOrder.length, 2, '★ keepCover=true 后 coverSeenOrder 不得被清空');
eq(Object.keys(coverToneCache).length, 2, '★ keepCover=true 后 coverToneCache 不得被清空');
eq(Object.prototype.hasOwnProperty.call(LS, 'mediaview_cover_seen'), true, '★ keepCover=true 后不得删除 localStorage.cover_seen');
eq(coverCacheBust, 0, 'keepCover=true 不得动 coverCacheBust');

// ② 无参调用（改画质/尺寸等场景）：封面确实要重建，三处记忆必须清空
seed();
bumpThumbCacheBust();
eq(thumbCacheBust, 1, '无参也要 bump thumbCacheBust');
eq(Object.keys(coverSeen).length, 0, '无参后 coverSeen 必须清空');
eq(coverSeenOrder.length, 0, '无参后 coverSeenOrder 必须清空');
eq(Object.keys(coverToneCache).length, 0, '无参后 coverToneCache 必须清空');
eq(Object.prototype.hasOwnProperty.call(LS, 'mediaview_cover_seen'), false, '无参后必须删 localStorage.cover_seen');

// ③ bumpCoverCacheBust：封面真被重建，bust + 记忆清空
seed();
bumpCoverCacheBust();
eq(coverCacheBust, 1, 'bumpCover 必须 bump coverCacheBust');
eq(LS['mediaview_cover_bust'], '1', 'cover bust 必须持久化');
eq(Object.keys(coverSeen).length, 0, 'bumpCover 后 coverSeen 必须清空');
eq(Object.keys(coverToneCache).length, 0, 'bumpCover 后 coverToneCache 必须清空');

// ④ 两个计数器互相独立：清缩略图不动封面 bust，清封面不动缩略图 bust
seed();
bumpThumbCacheBust(true);
eq(coverCacheBust, 0, '只清缩略图：cover bust 保持 0');
bumpCoverCacheBust();
eq(thumbCacheBust, 1, '只清封面：thumb bust 保持 1');

console.log('OK');
`
	runNodeCoverProbe(t, node, script)
}
