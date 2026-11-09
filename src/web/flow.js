/*
 * 手机端「竖图连续浏览」（v2.4.29）
 * ============================================================
 * 另起炉灶：本文件是一个**自包含模块**，自己建 DOM、自己管滚动、自己排队预载，
 * 不碰 app.js 里的三盒横向轨道（.v-track）、缩放状态（v.scale/tx/ty）和手势。
 * app.js 只做四件事：
 *   ① 提供 host 契约（window.__mvFlowHost：URL 构造 + 设置 + toast）；
 *   ② 手机档且开关打开时调 MVFlow.open(list, index)；
 *   ③ 关闭时调 MVFlow.close() 拿到"看到第几张"，据此把网格滚回那个位置；
 *   ④ 手势入口短路（flow 打开时，vStage 的点击/双击/捏合/方向键一律不生效）。
 *
 * 为什么单独一个文件而不是塞进 app.js：
 *   · 这块逻辑与"左右滑轨道"是两套完全不同的模型，混在 4000 行的 app.js 里
 *     迟早互相污染（2.4.14~2.4.28 的封面问题就是"两处状态没对齐"留下的）；
 *   · 只在触屏档注入（和 touch.js 同一套做法），桌面连下载都省了。
 *
 * 设计要点（与研究报告 §4 一一对应）：
 *   · 单列：图片 width:100%（= 手机宽度，左右不留白），高度由宽高比决定，两张之间 0 间距；
 *   · 无缝：display:block、无 margin/padding/border/圆角、容器同色背景（见 style.css）；
 *   · 图源：**只用大图那一条链路**（HOST.rawURL + HOST.pickMaxdim），
 *           也就是设置里「大图画质」那一档；宽度/比例的来源同样是 /api/meta（HOST.ensureSize），
 *           与大图查看器共用 ensureItemSize。**图片绝不拿缩略图顶替**（2.4.30 修）：
 *           2.4.29 曾用 /api/thumb 当"占位 + 比例探针"，在 NAS 上转码没回来时
 *           用户看到的就是那张模糊缩略图，等于另起了一套取图标准。
 *   · 防跳版：先按 w/h（拿不到用 3:4）占位，尺寸/图片到了只替换**高度**这一件事，
 *           且改高度前记锚点、改完补偿，画面不会往上跳；
 *   · 预载：**按序号**预取后面 N 张（并发 2，带 &prefetch=1 不抢前台槽），滚到哪锚到哪；
 *   · 解码：与大图查看器同一套 —— decode() 完成才露面（避免滚动中逐行刷出来）。
 */
(function () {
  'use strict';

  // 只表示"文件被加载了"，供测试断言桌面档根本没加载（与 touch.js 的 __mvTouchLoaded 同款）
  window.__mvFlowLoaded = true;

  var HOST = null;          // 由 app.js 注入的窄接口
  var st = null;            // 打开期间的会话状态；null = 未打开

  // 窗口预算：可见区外前后各挂几张（挂载 = 真的有 <img> 在 DOM 里）
  var BEFORE = 1;           // 上方多挂 1 张（往回滑不用等）
  var AFTER = 2;            // 下方多挂 2 张（往下滑第一屏就有图）
  var PREFETCH = 3;         // 顺序预取后面几张
  var PREFETCH_CONC = 2;    // 预取并发上限（再多就和主图抢带宽/解码）
  var SIZE_AHEAD = 4;       // 打开时先问回后面几张的原始宽高（/api/meta，只读文件头）
  var DEFAULT_RATIO = 0.75; // 宽高比未知时的占位（3:4 竖图，手机上最常见）

  var root = null, scrollEl = null, listEl = null;
  var topSp = null, botSp = null, countEl = null, hudEl = null;

  // ---------- 纯函数（可测：zzz_2429 用 node 直接喂参数跑） ----------

  // vfItemHeight 一张图在给定容器宽度下的高度（**取整**：小数高度会在拼接处留 1px 亮线）
  function vfItemHeight(ratio, width) {
    var r = ratio > 0 ? ratio : DEFAULT_RATIO;
    var h = Math.round(width / r);
    return h > 0 ? h : 1;
  }

  // vfIndexAt 累计高度 pre[] 中，y 落在第几张（pre 长度 = n+1，pre[i] 是第 i 张的顶边）
  function vfIndexAt(y, pre, n) {
    var hi = n - 1;
    if (hi < 0) return 0;
    if (y <= 0) return 0;
    if (y >= pre[n]) return hi;
    var lo = 0;
    while (lo < hi) {
      var mid = (lo + hi + 1) >> 1;
      if (pre[mid] <= y) lo = mid; else hi = mid - 1;
    }
    return lo;
  }

  // vfWindow 由可见区间推出挂载窗口 [start, end)
  function vfWindow(first, last, n, before, after) {
    if (n <= 0) return [0, 0];
    var s = first - before; if (s < 0) s = 0;
    var e = last + 1 + after; if (e > n) e = n;
    if (e < s) e = s;
    return [s, e];
  }

  // ---------- DOM ----------

  function ensureDom() {
    if (root) return;
    var stage = document.getElementById('vStage');
    if (!stage) throw new Error('no vStage');

    root = document.createElement('div');
    root.className = 'vf-root';
    root.setAttribute('id', 'vfRoot');

    scrollEl = document.createElement('div');
    scrollEl.className = 'vf-scroll';
    scrollEl.setAttribute('id', 'vfScroll');

    listEl = document.createElement('div');
    listEl.className = 'vf-list';

    topSp = document.createElement('div');
    topSp.className = 'vf-spacer';
    botSp = document.createElement('div');
    botSp.className = 'vf-spacer';

    listEl.appendChild(topSp);
    listEl.appendChild(botSp);
    scrollEl.appendChild(listEl);

    hudEl = document.createElement('div');
    hudEl.className = 'vf-hud';
    countEl = document.createElement('span');
    countEl.className = 'vf-count';
    hudEl.appendChild(countEl);

    root.appendChild(scrollEl);
    root.appendChild(hudEl);
    stage.appendChild(root);
  }

  // ---------- 布局 ----------

  function contentWidth() {
    return Math.max(1, scrollEl.clientWidth || window.innerWidth || 360);
  }

  // rebuildHeight 重算 h[] / pre[]（宽度或某张比例变了都要重来；O(n) 但只在必要时跑）
  function rebuildHeight() {
    var n = st.list.length;
    st.width = contentWidth();
    st.h = new Array(n);
    st.pre = new Array(n + 1);
    st.pre[0] = 0;
    for (var i = 0; i < n; i++) {
      var h = vfItemHeight(st.ratio[i], st.width);
      st.h[i] = h;
      st.pre[i + 1] = st.pre[i] + h;
    }
  }

  function updateSpacers() {
    if (!st) return;
    var n = st.list.length;
    topSp.style.height = st.pre[st.start] + 'px';
    botSp.style.height = Math.max(0, st.pre[n] - st.pre[st.end]) + 'px';
  }

  // clearItems 删掉两个 spacer 之间的所有条目（含断源），保留 spacer 本身
  function clearItems() {
    if (!listEl) return;
    var kids = [];
    for (var i = 0; i < listEl.children.length; i++) kids.push(listEl.children[i]);
    for (var k = 0; k < kids.length; k++) {
      var c = kids[k];
      if (c === topSp || c === botSp) continue;
      var imgs = c.querySelectorAll('img');
      for (var m = 0; m < imgs.length; m++) { imgs[m].onload = null; imgs[m].src = ''; }
      listEl.removeChild(c);
    }
  }

  // ---------- 挂载 / 卸载 ----------

  function itemEl(i) {
    return st.els[i] || null;
  }

  // requestSize 把这张图的**原始宽高**要回来（走 host.ensureSize = /api/meta，只读文件头）。
  // 这是"不另起一套"的关键：尺寸来源与大图查看器完全相同，不再拿缩略图当比例探针。
  // 回来后纠正占位高度 —— 图片自己 load 完也会再纠一次（两条路都通，谁先到算谁）。
  function requestSize(i) {
    var it = st && st.list[i];
    if (!it || it.kind !== 'image') return;
    if (it.w && it.h) return;
    if (it._vfAsked) return;
    it._vfAsked = true;
    try {
      HOST.ensureSize(it, function () {
        if (!st || st.list[i] !== it) return;
        if (it.w && it.h) applyRatio(i, it.w / it.h);
      });
    } catch (e) { it._vfAsked = false; }
  }

  // 视口能滚到的最远处（spacer 撑好之后才是准的）
  function maxScrollTop() {
    return Math.max(0, (scrollEl.scrollHeight || 0) - (scrollEl.clientHeight || 0));
  }

  // centerOffset 第 i 张"居中"时，它的顶边该落在视口上方多少像素。
  //
  // 比视口还高的长图返回 0（顶对齐）：那种图居中意味着头尾都看不到，
  // 顶对齐才是它能给出的最好位置（系统相册也是这么处理的）。
  function centerOffset(i) {
    var vh = scrollEl.clientHeight || 1;
    var h = st.h[i] || 0;
    if (h <= 0 || h >= vh) return 0;
    return Math.floor((vh - h) / 2);
  }

  // pinTo 锁定"当前张"：画面没动之前，index 就以用户点的那张为准（见 render 里的说明）
  function pinTo(i, y) {
    st.pin = true;
    st.pinIdx = i;
    st.pinY = y;
  }

  // scrollToIndex 把第 i 张摆到**视口中部**（打开时"定位到你点的那张"）。
  //
  // 为什么是中部而不是顶边（2.4.44）：顶边对齐时这张图上方什么上下文都没有，
  //   一眼看去像"打开就停在最上面"，不像"我点了这一张"；中部对齐才是系统相册的手感。
  //   首张、末张会被浏览器 clamp（没有负滚动、也没有超出去的空间），那时只能贴边，
  //   是几何限制而不是 bug —— 所以下面把"读回值 == 可滚动上限"也算作成功。
  //
  // 为什么要"设 → 自检 → 再补一帧"：
  //   ① 容器滚动高度由 spacer 撑起来，spacer 又是按当前窗口算的 —— 只要有一处没同步，
  //      scrollTop 就会被浏览器**静默 clamp**（设 3000、读回 0），而且不报错；
  //   ② 图片比例是异步纠正的（/api/meta、naturalWidth），纠正后 pre[] 会变。
  //   所以设完必须读回来比对：不一致说明布局还没到位，下一帧再对一次。
  function scrollToIndex(i) {
    if (!st) return;
    var want = Math.max(0, (st.pre[i] || 0) - centerOffset(i));
    var maxY = maxScrollTop();
    scrollEl.scrollTop = want;
    // maxY > 0 这个前提不能省：布局还没撑起来时 maxY == 0，任何 want 都被 clamp 成 0，
    //   那时"贴底"分支会恒真 ⇒ 把一次失败的定位当成成功，pin 到一张画面上根本没显示的图。
    if (Math.abs(scrollEl.scrollTop - want) <= 1 || (maxY > 0 && scrollEl.scrollTop >= maxY - 1)) {
      pinTo(i, scrollEl.scrollTop);   // 定位成功：画面没动之前"当前张"就以用户点的为准
      return;
    }
    requestAnimationFrame(function () {
      if (!st) return;
      st.idx = i;
      var want2 = Math.max(0, (st.pre[i] || 0) - centerOffset(i));
      var maxY2 = maxScrollTop();
      scrollEl.scrollTop = want2;
      if (Math.abs(scrollEl.scrollTop - want2) <= 1 || (maxY2 > 0 && scrollEl.scrollTop >= maxY2 - 1)) {
        pinTo(i, scrollEl.scrollTop);
      }
      render();
      updateHud();
    });
  }

  function mount(i) {
    if (st.els[i]) return;
    var it = st.list[i];
    if (!it) return;

    var el = document.createElement('div');
    el.className = 'vf-item';
    el.setAttribute('data-i', String(i));
    // 高度用**整数 px 显式指定**，不用 CSS aspect-ratio：
    // aspect-ratio 会算出小数高度，与 spacer 的取整值对不上时，拼接处就留 1px 亮线。
    el.style.height = st.h[i] + 'px';

    // 视频：先显示封面帧 + 播放按钮，点一下才换成 <video>（多个视频同时解码会卡死）。
    // 封面帧用缩略图是合理的 —— 视频没有静态大图可取，这就是它的海报，网格/大图也用同一张。
    // **图片分支绝不这样做**（见下面 else）。
    if (it.kind === 'video') {
      el.classList.add('vf-video');
      var poster = document.createElement('img');
      poster.className = 'vf-poster';
      poster.alt = '';
      poster.decoding = 'async';
      poster.src = HOST.thumbURL(it.path, HOST.thumbSize(), it.mtime);
      el.appendChild(poster);
      var play = document.createElement('div');
      play.className = 'vf-play';
      play.innerHTML = '<span>▶</span>';
      el.appendChild(play);
      el.addEventListener('click', function () {
        if (st.moved) return;
        swapToVideo(i, el);
      }, { passive: true });
    } else {
      // 正式图：与大图浏览**逐字节同一个 URL**（同 rawURL + pickMaxdim + mtime + s 指纹）
      //   ⇒ 大图窗口里已经解码过的那张，这里直接命中浏览器缓存，0 请求、0 转码。
      //   decode 完成才露面（与大图同一套防"逐行刷出来"）。
      var img = document.createElement('img');
      img.className = 'vf-full';
      img.alt = it.name || '';
      img.decoding = 'async';
      img.fetchPriority = 'high';
      img.src = HOST.rawURL(it.path, HOST.pickMaxdim(), it.mtime);
      var reveal = function () {
        if (!st) return;
        // 图片自带真实比例 —— 比 /api/meta 更权威（meta 可能失败/超时），到了就纠一次
        var nw = img.naturalWidth || 0, nh = img.naturalHeight || 0;
        if (nw > 0 && nh > 0) applyRatio(i, nw / nh);
        img.classList.add('vf-ok');
      };
      if (img.decode) {
        img.decode().then(reveal, function () {
          // decode 失败可能是被销毁/换源；只要已经 load 过就照常显示
          if (img.complete && img.naturalWidth > 0) reveal();
        });
      } else {
        img.onload = reveal;
      }
      el.appendChild(img);
      requestSize(i);
    }

    insertInOrder(i, el);
    st.els[i] = el;
  }

  function insertInOrder(i, el) {
    var keys = Object.keys(st.els);
    var next = null;
    for (var k = 0; k < keys.length; k++) {
      var idx = parseInt(keys[k], 10);
      if (idx > i) {
        if (next === null || idx < next) next = idx;
      }
    }
    if (next !== null && st.els[next]) listEl.insertBefore(el, st.els[next]);
    else listEl.insertBefore(el, botSp);
  }

  function unmount(i) {
    var el = st.els[i];
    if (!el) return;
    // 断源释放解码器：滚远的图不该继续占内存（手机上尤其明显）
    var imgs = el.querySelectorAll('img');
    for (var k = 0; k < imgs.length; k++) {
      imgs[k].onload = null;
      imgs[k].src = '';
    }
    var v = el.querySelector('video');
    if (v) { try { v.pause(); } catch (e) {} v.removeAttribute('src'); }
    if (el.parentNode) el.parentNode.removeChild(el);
    delete st.els[i];
  }

  function swapToVideo(i, el) {
    var it = st.list[i];
    if (!it || el.querySelector('video')) return;
    var play = el.querySelector('.vf-play');
    if (play) play.style.display = 'none';
    var v = document.createElement('video');
    v.controls = true;
    v.playsInline = true;
    v.preload = 'metadata';
    v.src = HOST.rawURL(it.path, 0, it.mtime);
    v.className = 'vf-video-el';
    el.appendChild(v);
    var p = v.play();
    if (p && p.catch) p.catch(function () {});
  }

  // ---------- 比例纠正 ----------

  function applyRatio(i, r) {
    if (!st || !(r > 0)) return;
    if (Math.abs((st.ratio[i] || 0) - r) < 0.002) return;
    st.ratio[i] = r;

    // 锚点补偿：改的是**当前可见位置之前**的图 ⇒ 不清补偿的话，用户正看着的画面会往上跳。
    var anchor = st.idx;
    var off = scrollEl.scrollTop - (st.pre[anchor] || 0);
    var el = st.els[i];
    rebuildHeight();
    if (el) el.style.height = st.h[i] + 'px';
    if (i < anchor) {
      // 2.4.44：这里**不再** Math.max(0, off)。居中定位时 off 本来就是负的
      //   （目标顶边落在视口上方），clamp 成 0 会把它变回"顶边对齐"——
      //   用户正看着的那张会往上跳一下。
      scrollEl.scrollTop = Math.max(0, (st.pre[anchor] || 0) + off);
    }
    updateSpacers();
    // 2.4.44：打开那张自己的比例是**异步**纠正的（/api/meta、naturalWidth）。高度变了以后，
    //   若它还处在"刚打开、用户没滚过"的状态，重新居中一次 ——
    //   否则最终停在偏上的位置，用户看到的仍然是"没居中"。
    if (st.pin && st.pinIdx === i && Math.abs(scrollEl.scrollTop - st.pinY) <= 2) {
      scrollToIndex(i);
      render();
      return;
    }
    render();
  }

  // refreshMountedHeights 宽度变了以后，把已挂载的盒子高度同步到新的 h[]
  function refreshMountedHeights() {
    for (var k in st.els) {
      var i = parseInt(k, 10);
      if (st.els[i]) st.els[i].style.height = st.h[i] + 'px';
    }
  }

  // ---------- 渲染（窗口化） ----------

  function render() {
    if (!st) return;
    var n = st.list.length;
    if (!n) return;
    var y = scrollEl.scrollTop;
    var vh = scrollEl.clientHeight || 1;
    var first = vfIndexAt(y, st.pre, n);
    var last = vfIndexAt(y + vh - 1, st.pre, n);
    var w = vfWindow(first, last, n, BEFORE, AFTER);
    var start = w[0], end = w[1];

    // 卸载窗口外的
    for (var k in st.els) {
      var idx = parseInt(k, 10);
      if (idx < start || idx >= end) unmount(idx);
    }
    // 挂载新增的
    for (var i = start; i < end; i++) {
      if (!st.els[i]) mount(i);
    }
    st.start = start;
    st.end = end;
    updateSpacers();

    // 当前张：取**视口正中**。为什么不是顶边、也不是 35%：
    //   打开时目标图是被摆到中部的（见 scrollToIndex），中线必然落在它身上；
    //   顶边那张往往只是上一张露出的尾巴；35% 对矮图（横图只有半屏高）也会落到下一张。
    var cur = vfIndexAt(y + Math.floor(vh * 0.5), st.pre, n);
    // 2.4.32 / 2.4.44：刚打开的那一帧不许被焦点线覆盖。
    //   首张、末张在几何上顶不到中线（被 clamp 在边界），焦点线会落到邻居身上，
    //   于是 HUD 显示"你点的下一张"、退出后网格也定位到错的那张。
    //   锁的释放条件用**滚动位置**而不是帧数：画面没动 = 用户看的还是同一张。
    if (st.pin) {
      if (Math.abs(y - st.pinY) <= 2) cur = st.pinIdx;
      else st.pin = false;
    }
    if (cur !== st.idx) st.idx = cur;
    updateHud();
    schedulePrefetch(st.idx);
  }

  function updateHud() {
    if (!countEl || !st) return;
    countEl.textContent = (st.idx + 1) + ' / ' + st.list.length;
  }

  // ---------- 顺序预载 ----------

  function schedulePrefetch(idx) {
    if (!st) return;
    st.queue = [];
    for (var k = 1; k <= PREFETCH; k++) {
      var i = idx + k;
      if (i < st.list.length && !st.warm[i]) st.queue.push(i);
    }
    // 往回一张也热一下（用户回看时不至于白屏），排在最后
    if (idx - 1 >= 0 && !st.warm[idx - 1]) st.queue.push(idx - 1);
    pump();
  }

  function pump() {
    if (!st) return;
    while (st.inflight < PREFETCH_CONC && st.queue.length) {
      var i = st.queue.shift();
      var it = st.list[i];
      if (!it || it.kind !== 'image' || st.warm[i]) continue;
      var im = new Image();
      im.fetchPriority = 'low';
      // prefetch=1 是后端既有契约：不触发「用户在看大图」、不抢前台缩放槽
      im.src = HOST.rawURL(it.path, HOST.pickMaxdim(), it.mtime) + '&prefetch=1';
      st.warm[i] = im;
      st.inflight++;
      var done = function () {
        if (!st) return;
        st.inflight--;
        pump();
      };
      if (im.decode) im.decode().then(done, done);
      else im.onload = done, im.onerror = done;
    }
  }

  // ---------- 打开 / 关闭 ----------

  // ---------- 打开前的准备：自排 + 定位（纯函数，不碰 DOM，便于单测）----------
  //
  // 为什么必须**自己排一遍**（2.4.41）：传入的数组来自 app 的 state.items，而它可能是
  //   · "改完排序、列表还没回来"时的**旧顺序**；
  //   · 手机整页导航后先铺上的**旧快照**。
  // 这两种情况下数组顺序都不是当前规则，直接铺出来就是"竖图流永远按修改时间降序"。
  // 比较器只有 app.js 那一份（HOST.sortList），流里不重复实现。
  //
  // start 可以是 path（app 传的）或下标（探针/旧调用），两种都认。
  function prepareList(list, start) {
    var media = [];
    for (var i = 0; i < (list || []).length; i++) {
      if (list[i] && list[i].path) media.push(list[i]);
    }
    if (HOST && typeof HOST.sortList === 'function') {
      try {
        var sorted = HOST.sortList(media, HOST.currentSort ? HOST.currentSort() : '');
        if (sorted && sorted.length === media.length) media = sorted;
      } catch (e) { /* 排序失败就用原顺序：宁可旧序，也不能打不开 */ }
    }
    var i0 = 0;
    if (typeof start === 'string') {
      for (var k = 0; k < media.length; k++) {
        if (media[k].path === start) { i0 = k; break; }
      }
    } else if (typeof start === 'number' && isFinite(start)) {
      i0 = Math.floor(start);
    }
    if (i0 < 0) i0 = 0;
    if (i0 > media.length - 1) i0 = Math.max(0, media.length - 1);
    return { media: media, idx: i0 };
  }

  function open(list, start) {
    if (!HOST) { try { console.warn('[mv-flow] host 未注入'); } catch (e) {} return false; }
    if (st) close();
    var prep = prepareList(list, start);
    var media = prep.media;
    if (!media.length) return false;

    ensureDom();
    var vWrap = document.getElementById('vMediaWrap');
    if (vWrap) vWrap.style.display = 'none';   // 轨道挪开（不动它的内容，关闭时原样恢复）
    document.body.classList.add('mv-flow-open');
    root.classList.add('vf-on');

    var n = media.length;
    st = {
      list: media,
      ratio: new Array(n),
      h: null, pre: null,
      width: 0,
      start: 0, end: 0,
      els: {},
      idx: prep.idx,
      pin: false,     // "当前张"锁定是否有效（打开定位用，见 render 里的说明）
      pinIdx: -1,     // 锁定的下标（用户点的那张）
      pinY: 0,        // 锁定那一刻的 scrollTop —— 滚走了就自动失效
      queue: [],
      inflight: 0,
      warm: new Array(n),
      moved: false,
      scrollHandler: null,
      resizeHandler: null,
      clickHandler: null,
      raf: 0
    };
    // 比例：优先用列表里已有的 w/h（视频一定有，图片一般没有）——
    // 没有的交给 requestSize（/api/meta，只读文件头）补，图片到了再由 naturalWidth 兜底纠一次。
    for (var j = 0; j < n; j++) {
      var it0 = media[j];
      st.ratio[j] = (it0 && it0.w && it0.h) ? (it0.w / it0.h) : 0;   // 0 = 未知，先按 3:4 占位
    }

    // 清掉上一次的残留子元素（只保留上下两个 spacer）
    clearItems();

    rebuildHeight();
    // 2.4.32：**必须先把 spacer 撑起来再设 scrollTop**。
    //   2.4.31 及之前这里是 `scrollTop = pre[idx]` 然后才 updateSpacers()：
    //   设的那一刻两个 spacer 高度都还是 0（close() 里归零过），滚动高度为 0，
    //   浏览器会把 scrollTop clamp 成 0 —— 于是不管点第几张，打开都在最顶上。
    updateSpacers();
    scrollToIndex(st.idx);
    render();
    updateHud();
    // 当前张起往后几张先把尺寸问回来（本地 ReadDir 时没读，这里补；只读文件头，很便宜），
    // 这样一开始就把高度钉准，滑下去不会一张张地"高矮跳"。
    for (var q = 0; q < SIZE_AHEAD; q++) {
      if (st.idx + q < n) requestSize(st.idx + q);
    }

    // 滚动：rAF 节流（scroll 事件频率远高于帧率，直接做窗口计算会白烧 CPU）
    st.scrollHandler = function () {
      if (!st || st.raf) return;
      st.raf = requestAnimationFrame(function () {
        if (!st) return;
        st.raf = 0;
        render();
      });
    };
    scrollEl.addEventListener('scroll', st.scrollHandler, { passive: true });

    // 尺寸变化（转屏 / 地址栏收起）：宽度变了所有高度都得重算，但**保持当前张不跑偏**
    st.resizeHandler = function () {
      if (!st) return;
      var anchor = st.idx, off = scrollEl.scrollTop - (st.pre[anchor] || 0);
      rebuildHeight();
      refreshMountedHeights();
      // 2.4.44：同样保留负偏移（居中时 off < 0），否则转屏会把居中的那张推到顶边
      scrollEl.scrollTop = Math.max(0, (st.pre[anchor] || 0) + off);
      updateSpacers();
      // 刚打开就转屏（还没滚过）：重新居中一次
      if (st.pin) scrollToIndex(st.idx);
      render();
    };
    window.addEventListener('resize', st.resizeHandler);

    // 轻点切换 HUD（不关窗口、不切图：flow 里没有"双击放大"）
    st.clickHandler = function (e) {
      if (!st) return;
      if (e.target.closest && e.target.closest('video')) return;   // 点视频控件不算
      if (e.target.classList && e.target.classList.contains('vf-play')) return;
      root.classList.toggle('vf-hud-hidden');
    };
    root.addEventListener('click', st.clickHandler);

    return true;
  }

  // 返回**被看那一张的 path**（而不是下标）：
  //   流内部会按当前排序重排，下标与网格的 state.items 已经不是同一套坐标了。
  function close() {
    if (!st) return '';
    var outPath = (st.list[st.idx] && st.list[st.idx].path) || '';
    try {
      if (st.scrollHandler) scrollEl.removeEventListener('scroll', st.scrollHandler);
      if (st.resizeHandler) window.removeEventListener('resize', st.resizeHandler);
      if (st.clickHandler) root.removeEventListener('click', st.clickHandler);
      if (st.raf) cancelAnimationFrame(st.raf);
    } catch (e) {}
    // 断掉所有在挂的图与视频（关闭后不该继续占解码器/带宽）
    for (var k in st.els) unmount(parseInt(k, 10));
    st = null;

    if (root) {
      root.classList.remove('vf-on', 'vf-hud-hidden');
      clearItems();
      if (topSp) topSp.style.height = '0px';
      if (botSp) botSp.style.height = '0px';
    }
    if (scrollEl) scrollEl.scrollTop = 0;
    var vWrap = document.getElementById('vMediaWrap');
    if (vWrap) vWrap.style.display = '';
    document.body.classList.remove('mv-flow-open');
    return outPath;
  }

  // ---------- 对外接口 ----------

  window.MVFlow = {
    // app.js 注入 host（在 flow.js 加载前就写好 window.__mvFlowHost 也行，见 install）
    installHost: function (h) { HOST = h; },
    available: function () { return !!(HOST || window.__mvFlowHost); },
    isOpen: function () { return !!st; },
    index: function () { return st ? st.idx : -1; },
    open: open,
    close: close,
    // 只给单测/探针用：纯函数，不产生任何副作用
    _prepareList: prepareList
  };

  // app.js 的注入可能早于本文件（脚本按 defer 顺序执行）：两个方向都接住
  if (window.__mvFlowHost) HOST = window.__mvFlowHost;
  window.addEventListener('mv-flow-host', function () {
    if (window.__mvFlowHost) HOST = window.__mvFlowHost;
  });
})();
