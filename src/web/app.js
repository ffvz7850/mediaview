/* 极速媒体浏览 - 原生 JS，无框架，性能优先 */
(function () {
  'use strict';

  // ---------- API base (auto-detect gateway prefix) ----------
  var GW = '/app/mediaview';
  var API = GW + '/api';
  // 缩略图 URL 必须把「源文件 mtime」编进去，否则服务端给的 immutable(一年) 会锁死旧图：
  // 同名文件被覆盖后 URL 不变 → 浏览器一年内不回源，一直显示旧缩略图。
  // 再叠加「清除缓存计数器」：清缓存后 mtime 没变，只能靠这个换 URL。
  // 它必须持久化，否则刷新页面归零 → 又命中一年前那份旧缓存，清缓存等于白清。
  var thumbCacheBust = 0;
  try { thumbCacheBust = parseInt(localStorage.getItem('mediaview_thumb_bust') || '0', 10) || 0; } catch (e) {}
  function bumpThumbCacheBust() {
    thumbCacheBust++;
    try { localStorage.setItem('mediaview_thumb_bust', String(thumbCacheBust)); } catch (e) {}
  }
  function thumbURL(p, size, mtime) {
    // 带上画质：服务端的缓存标识含画质（改画质会重新生成），URL 也必须跟着变，
    // 否则浏览器会继续用 immutable(一年) 的旧图，用户以为设置没生效。
    return API + '/thumb?path=' + enc(p) + '&size=' + (size || 320) +
      '&q=' + (cfg.thumbQuality || 80) +
      '&_t=' + (mtime || 0) + '&_v=' + thumbCacheBust;
  }
  function rawURL(p, maxdim) {
    // maxdim 是"这张图最终要被显示到多大"的预算（见 pickMaxdim），后端据此用
    // ffmpeg lanczos 降采样 —— 质量远好于把上万像素的原图丢给浏览器自己缩。
    // 不带 maxdim 只在"要原文件"时才用（目前只有大图升级原图那一处）。
    var url = API + '/raw?path=' + enc(p);
    if (maxdim) url += '&maxdim=' + maxdim;
    return url;
  }
  function metaURL(p) { return API + '/meta?path=' + enc(p); }
  function enc(s) { return encodeURIComponent(s); }

  // 浏览用的预缩预算。不能写死常量：写死 2048 只对视口长边 × dpr ≤ 2048 的屏幕刚好，
  // 高分辨率屏全屏时 2048 预缩版被上采样会糊。按视口需求 × 1.4 过采样动态计算。
  var PICK_MIN = 2048, PICK_MAX = 4096, PICK_OVERSAMPLE = 1.4;
  function viewportNeedPx() {
    var dpr = window.devicePixelRatio || 1;
    var w = window.innerWidth || 1280, h = window.innerHeight || 720;
    return Math.min(w, h * 1.5) * dpr;
  }
  // 预算必须量化到固定档位：服务端缩放缓存按 maxdim 命名
  // （mediaview-scaled-v3-<key>-<maxdim>.jpg），若把视口算出的任意像素值直接当预算，
  // 拖窗口/全屏/转屏每变一次尺寸就多生成一份互不复用的缓存，每次都要重跑一遍 ffmpeg。
  // 对齐到 PICK_STEP 的整数倍后档位有限、可复用；用 ceil 保证不低于需求（偏清晰侧）。
  var PICK_STEP = 256;
  function pickMaxdim() {
    var m = Math.round(viewportNeedPx() * PICK_OVERSAMPLE);
    m = Math.ceil(m / PICK_STEP) * PICK_STEP;
    if (m < PICK_MIN) return PICK_MIN;
    if (m > PICK_MAX) return PICK_MAX;
    return m;
  }

  // ---------- Settings cache (缩略图开关等) ----------
  // 默认"开"，与后端默认一致；加载失败也保持开，避免误伤原有体验。
  var cfg = {
    thumbEnabled: true,
    thumbDir: '',
    thumbSize: 320,
    thumbQuality: 80,
    activeThumbDir: '',
    defaultThumbDir: '',
    note: '',
    cacheFiles: 0,
    cacheBytes: 0,
    settingsFile: '',
    takeoverSystemThumb: false,
    systemThumbAvail: false,
    systemThumbActive: false,
    cpuCores: 0,
    thumbConcurrency: 6,
    preloadConcurrency: 3,
    gpuDecode: true,
    gpuImageDecode: false,
    viewerPreload: 2,
    viewerAnimation: 'slide',
    viewerMode: 'window' // window=独立桌面窗口 / overlay=窗口内遮罩
  };
  var settingsReady = null; // Promise

  // ===== 宿主桥接（消费 bridge.js 暴露的 window.fnosBridge）=====
  // 历史教训：本文件原先自己手搓了一份 penpal 客户端，发的是数字枚举
  // （Syn=0 / Call=3），而飞牛桌面用的是字符串枚举（"syn" / "call" / "ack"），
  // 且握手是三回合、子端收到 SYN-ACK 后必须回 ACK —— 两条都没对齐，宿主
  // 直接静默丢弃，表现就是「点了没反应」。
  // 现在协议全部交给 vendor 在包内的官方 @trimjs/web-app SDK（见 bridge.js），
  // 这里只做三件事：等它就绪 / 查宿主方法表 / 转发调用。
  var host = {
    connected: false,
    methods: {},
    reason: 'not-initialized',
    _ready: null
  };

  function hostLog() {
    var args = ['[mediaview-penpal]'].concat(Array.prototype.slice.call(arguments));
    try { console.log.apply(console, args); } catch (e) {}
  }

  // bridge.js 是 ES module，执行时机永远晚于本脚本，所以要轮询等它挂上
  function waitBridge(timeoutMs) {
    return new Promise(function (resolve) {
      var t0 = Date.now();
      (function poll() {
        if (window.fnosBridge) return resolve(window.fnosBridge);
        if (Date.now() - t0 >= timeoutMs) return resolve(null);
        setTimeout(poll, 50);
      })();
    });
  }

  function hostConnect() {
    if (host._ready) return host._ready;
    host._ready = waitBridge(4000).then(function (b) {
      if (!b) {
        host.reason = 'bridge-script-missing';
        hostLog('bridge.js 未加载（window.fnosBridge 缺失）');
        return false;
      }
      return b.ready().then(function (r) {
        host.reason = r.reason;
        if (r.ok) {
          host.connected = true;
          r.methods.forEach(function (m) { host.methods[m] = true; });
          hostLog('handshake OK, methods:', r.methods);
          return true;
        }
        hostLog('handshake failed:', r.reason, r.error || '');
        return false;
      });
    }).then(function (ok) {
      // 失败不缓存：慢网首次握手超时不该让整个会话都用不了独立窗口
      // （原来成功失败都被 host._ready 记住，只能刷新页面才能重试）
      if (!ok) host._ready = null;
      return ok;
    }).catch(function (e) {
      host.reason = 'bridge-error';
      hostLog('bridge error:', e);
      host._ready = null;
      return false;
    });
    return host._ready;
  }

  function hostCall(method) {
    var args = Array.prototype.slice.call(arguments, 1);
    hostLog('call:', method, args);
    return waitBridge(4000).then(function (b) {
      if (!b) throw new Error('bridge-unavailable');
      return b.call.apply(b, [method].concat(args));
    });
  }

  // 是否处于「宿主 iframe」内（跨域读取 parent 会抛错，也按在 iframe 内处理）
  function inHostIframe() {
    try { return window.parent !== window; } catch (e) { return true; }
  }

  // 把桥接失败原因转成人话，用于 toast
  function hostReasonText() {
    switch (host.reason) {
      case 'standalone': return '当前不在飞牛桌面内';
      case 'bridge-script-missing': return '桥接脚本未加载';
      case 'handshake-timeout': return '与桌面握手超时';
      case 'bridge-error': return '桥接通讯异常';
      case 'sdk-unavailable': return '宿主桥接 SDK 不可用';
      case 'empty-host-methods': return '宿主要求未响应';
      case 'not-web-runtime': return '当前运行时不支持';
      default: return '宿主未开放该能力';
    }
  }

  // 页面加载时立即握手（不等用户点击，避免弹窗拦截问题）。
  // 握手完成后再补设一次标题：首图显示时通常还没握上手，那一次 setTitle 已经落空。
  hostConnect().then(function () { updateWindowTitle(); });

  // ---------- Elements ----------
  var $ = function (id) { return document.getElementById(id); }
  var crumbEl = $('crumb'),
    scroller = $('gridScroller'),
    spacer = $('gridSpacer'),
    content = $('gridContent'),
    emptyEl = $('empty'), emptyText = $('emptyText'),
    loadingEl = $('loading'),
    viewer = $('viewer'),
    vWrap = $('vMediaWrap'), vCounterFloat = $('vCounterFloat');

  // ---------- State ----------
  var state = {
    path: null,
    items: [],      // unified: dirs (kind='dir') + files
    files: [],
    cols: 5,
    cell: 140,
    gap: 6,
    sort: 'date',
    cellMode: (function(){ try { return localStorage.getItem('mediaview_cellMode') || 'large'; } catch(e){ return 'large'; } })(), // large | small
    root: false,
  };
  var cellPool = {};   // data-idx -> element
  var rafPending = false;

  // ---------- Helpers ----------
  function toast(msg, ms) {
    var t = $('toast');
    t.textContent = msg;
    t.classList.add('show');
    clearTimeout(t._h);
    t._h = setTimeout(function () { t.classList.remove('show'); }, ms || 1800);
  }
  function fetchJSON(url, opts) {
    return fetch(url, opts).then(function (r) {
      if (!r.ok) { return r.text().then(function (tx) { throw new Error(tx || r.status); }); }
      return r.json();
    });
  }
  function fmtB(n) {
    if (!n) return '0 B';
    if (n >= 1073741824) return (n / 1073741824).toFixed(2) + ' GB';
    if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
    if (n >= 1024) return (n / 1024).toFixed(0) + ' KB';
    return n + ' B';
  }
  function fmtSize(s) {
    if (s >= 1073741824) return (s / 1073741824).toFixed(1) + ' GB';
    if (s >= 1048576) return (s / 1048576).toFixed(1) + ' MB';
    if (s >= 1024) return (s / 1024).toFixed(0) + ' KB';
    return s + ' B';
  }
  function fmtDur(d) {
    if (!d) return '';
    var m = Math.floor(d / 60), s = Math.floor(d % 60);
    var h = Math.floor(m / 60); m = m % 60;
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return h > 0 ? h + ':' + p(m) + ':' + p(s) : m + ':' + p(s);
  }

  // ---------- Root (storage spaces) ----------
  function buildRoot() {
    state.root = true; state.path = null;
    loadingEl.classList.remove('hidden');
    // 一次拿到真实盘位列表：原来是硬编码 /vol1..10（盘位更多的机器看不到后面的存储空间），
    // 而且每一轮还先发一个返回值完全没用的 /api/health —— 首屏白白多打 10 个请求。
    fetchJSON(API + '/volumes').then(function (d) {
      loadingEl.classList.add('hidden');
      var vols = (d && d.volumes) || [];
      state.items = vols.map(function (v) { return { kind: 'dir', name: v.name, path: v.path }; });
      state.files = [];
      renderCrumb();
      layout();
    }).catch(function () {
      loadingEl.classList.add('hidden');
      state.items = [];
      state.files = [];
      renderCrumb();
      layout();
    });
  }

  // ---------- Load directory ----------
  // 目录加载序号：快速连点两个目录时先发的请求可能后返回 ——
  // 没有这个守卫就会出现「面包屑是后点的目录、网格却是先点目录的内容」。
  var loadSeq = 0;
  function loadDir(path) {
    state.root = false;
    state.path = path;
    loadingEl.classList.remove('hidden');
    emptyEl.classList.add('hidden');
    var mySeq = ++loadSeq;
    var url = API + '/list?path=' + enc(path) + '&sort=' + state.sort;
    fetchJSON(url).then(function (d) {
      if (mySeq !== loadSeq) return; // 已有更新的目录请求，丢弃这次结果
      loadingEl.classList.add('hidden');
      var dirs = d.dirs.map(function (x) { return { kind: 'dir', name: x.name, path: x.path }; });
      state.items = dirs.concat(d.files);
      state.files = d.files;
      renderCrumb();
      layout();
      try { localStorage.setItem('mediaview_last', path); } catch (e) {}
    }).catch(function (e) {
      if (mySeq !== loadSeq) return;
      loadingEl.classList.add('hidden');
      // 清掉上一个目录的内容：否则旧格子会压在空状态下面继续占位、继续发缩略图请求
      state.items = []; state.files = [];
      for (var k in cellPool) {
        var el = cellPool[k];
        el.parentNode && el.parentNode.removeChild(el);
        delete cellPool[k];
      }
      renderCrumb();
      emptyText.textContent = '无法打开：' + (e.message || '');
      emptyEl.classList.remove('hidden');
    });
  }

  // ---------- Breadcrumb ----------
  function renderCrumb() {
    crumbEl.innerHTML = '';
    var home = makeCrumb('存储空间', null, false);
    crumbEl.appendChild(home);
    if (state.path) {
      var parts = state.path.split('/').filter(Boolean);
      var acc = '';
      parts.forEach(function (seg, i) {
        crumbEl.appendChild(makeSep());
        acc += '/' + seg;
        var cur = i === parts.length - 1;
        crumbEl.appendChild(makeCrumb(seg, acc, cur));
      });
    }
  }
  function makeCrumb(label, path, current) {
    var d = document.createElement('div');
    d.className = 'crumb-item' + (current ? ' current' : '');
    d.textContent = label;
    d.onclick = function () { if (path === null) buildRoot(); else loadDir(path); };
    return d;
  }
  function makeSep() {
    var s = document.createElement('span');
    s.className = 'crumb-sep';
    s.textContent = '/';
    return s;
  }

  // ---------- Layout / virtual grid ----------
  function computeCols() {
    var w = scroller.clientWidth - 12;
    // 大视图 target=180，小视图 target=96（一排更多更小的预览图，缩略图不重新生成）
    var target = state.cellMode === 'small' ? 96 : 180;
    var cols = Math.max(2, Math.floor((w + state.gap) / (target + state.gap)));
    state.cols = cols;
    state.cell = Math.floor((w - (cols - 1) * state.gap) / cols);
  }

  // keepScroll=true 时保留当前滚动位置（窗口 resize、切换图标大小用）；
  // 只有切换目录/重建列表才把位置归零 —— 否则拖一下窗口、点一下图标大小，
  // 用户辛苦滚到的地方就被弹回顶部了。
  function layout(keepScroll) {
    var keepTop = keepScroll ? scroller.scrollTop : 0;
    computeCols();
    var n = state.items.length;
    var rows = Math.ceil(n / state.cols);
    var totalH = rows * (state.cell + state.gap);
    spacer.style.height = totalH + 'px';
    content.style.height = totalH + 'px';
    if (n === 0) {
      emptyEl.classList.remove('hidden');
      emptyText.textContent = '此文件夹没有图片或视频';
    } else {
      emptyEl.classList.add('hidden');
    }
    scroller.scrollTop = keepTop;
    // clear pool on fresh layout
    for (var k in cellPool) {
      var el = cellPool[k]; el.parentNode && el.parentNode.removeChild(el);
      delete cellPool[k];
    }
    requestRender();
  }

  function requestRender() {
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(function () { rafPending = false; renderVisible(); });
  }

  function renderVisible() {
    var n = state.items.length;
    if (n === 0) { return; }
    var cell = state.cell + state.gap;
    var top = scroller.scrollTop;
    var h = scroller.clientHeight;
    var startRow = Math.max(0, Math.floor(top / cell) - 2);
    var endRow = Math.ceil((top + h) / cell) + 2;
    var startIdx = startRow * state.cols;
    var endIdx = Math.min(n, endRow * state.cols);

    var need = {};
    for (var i = startIdx; i < endIdx; i++) { need[i] = true; }

    // remove cells no longer visible
    for (var key in cellPool) {
      var ki = parseInt(key, 10);
      if (!need[ki]) {
        var old = cellPool[key];
        old.parentNode && old.parentNode.removeChild(old);
        delete cellPool[key];
      }
    }
    // create / update
    for (var idx = startIdx; idx < endIdx; idx++) {
      var item = state.items[idx];
      var el = cellPool[idx];
      var r = Math.floor(idx / state.cols), c = idx % state.cols;
      var x = c * (state.cell + state.gap) + 6;
      var y = r * (state.cell + state.gap);
      if (!el) {
        el = createCell(item, idx);
        cellPool[idx] = el;
        content.appendChild(el);
      }
      el.style.transform = 'translate3d(' + x + 'px,' + y + 'px,0)';
      el.style.width = state.cell + 'px';
      el.style.height = state.cell + 'px';
    }
  }

  function createCell(item, idx) {
    var el = document.createElement('div');
    el.className = 'cell';
    var inner = document.createElement('div');
    inner.className = 'cell-inner';
    if (item.kind === 'dir') {
      inner.style.background = 'var(--bg-cell)';
      inner.innerHTML = '<div style="display:flex;flex-direction:column;align-items:center;justify-content:center;height:100%;gap:6px;">' +
        '<div style="font-size:42px;line-height:1;">📁</div>' +
        '<div style="font-size:11px;color:var(--text-dim);padding:0 6px;text-align:center;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;width:100%;">' +
        escapeHtml(item.name) + '</div></div>';
      inner.onclick = function () { loadDir(item.path); };
    } else if (!cfg.thumbEnabled) {
      // 缩略图已关闭：降级为「图标 + 扩展名 + 文件名」，不产生任何缩略图请求
      inner.appendChild(fileCard(item));
      (function (it) { inner.onclick = function () { openViewer(it); }; })(item);
    } else {
      var img = new Image();
      img.className = 'cell-thumb'; img.loading = 'lazy'; img.decoding = 'async';
      img.src = thumbURL(item.path, cfg.thumbSize || 320, item.mtime);
      img.alt = item.name;
      // 缩略图加载失败：只对当前图片降级为图标，不影响其他图片（避免一张图失败导致全局关闭缩略图）
      img.onerror = function () {
        if (img.dataset.mvFailed) return;
        img.dataset.mvFailed = '1';
        if (!cfg.thumbEnabled) return;
        inner.innerHTML = '';
        inner.appendChild(fileCard(item));
        inner.onclick = function () { openViewer(item); };
      };
      inner.appendChild(img);
      if (item.kind === 'video') {
        var play = document.createElement('div');
        play.className = 'cell-video-icon'; play.textContent = '▶';
        inner.appendChild(play);
        if (item.duration) {
          var bd = document.createElement('div');
          bd.className = 'cell-badge'; bd.textContent = fmtDur(item.duration);
          inner.appendChild(bd);
        }
      }
      (function (it) { inner.onclick = function () { openViewer(it); }; })(item);
    }
    el.appendChild(inner);
    attachHoverPrefetch(inner, item);
    return el;
  }

  // ---------- 打开查看器时释放浏览器连接池 ----------
  //
  // 浏览器对同源并发只有 6 条连接，网格滚动会把它们全占满（每条都在等后端 ffmpeg）。
  // 此时点开大图，/api/raw 只能排在队尾 —— 用户感觉到的"点了没反应"，
  // 很大一部分其实是排队，而不是转码本身慢。
  // 打开查看器时把网格里在途的缩略图请求换成 1x1 占位（等价于取消请求），
  // 关闭查看器时再恢复，让连接和大图转码都拿到资源。
  var gridSuspended = [];
  var BLANK_PIXEL = 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7';

  // 悬停预取：鼠标在某个 cell 上停留 HOVER_PREFETCH_DELAY，说明用户在挑这一张，
  // 提前把大图预览转好（**与点击用同一个 maxdim**，点开时直接命中浏览器缓存）。
  // 服务端对 prefetch=1 的请求是非阻塞抢槽，抢不到立即放弃，
  // 因此预取永远不会排在用户真正点击的那次请求前面。
  var HOVER_PREFETCH_DELAY = 250;
  var hoverPrefetched = {}, hoverPrefetchCount = 0;
  var canHover = (function () {
    try { return !window.matchMedia || window.matchMedia('(hover: hover)').matches; } catch (e) { return true; }
  })();

  var HOVER_PREFETCH_MAX = 2; // 同时最多几个预取在飞：预取是尽力而为，忙就跳过
  var hoverPrefetchInFlight = 0;

  function attachHoverPrefetch(el, item) {
    if (!canHover || !item || item.kind !== 'image') return;
    var timer = null;
    var ctl = null;
    el.addEventListener('mouseenter', function () {
      if (hoverPrefetched[item.path]) return;
      clearTimeout(timer);
      timer = setTimeout(function () {
        if (hoverPrefetched[item.path]) return;
        if (hoverPrefetchInFlight >= HOVER_PREFETCH_MAX) return; // 已经够忙，不添乱
        if (hoverPrefetchCount > 500) { hoverPrefetched = {}; hoverPrefetchCount = 0; }
        hoverPrefetched[item.path] = true;
        hoverPrefetchCount++;
        // 用 fetch + 自定义头而不是 <img>：**URL 与真正打开时完全一致**，
        // 于是打开的 <img> 能直接命中 HTTP 缓存（省掉一次几百 KB 传输与重新解码）。
        // 「这是预取」的信息走请求头，服务端据此非阻塞抢槽（抢不到就放弃，绝不挡住点击）。
        var url = rawURL(item.path, pickMaxdim());
        hoverPrefetchInFlight++;
        var done = function () { if (hoverPrefetchInFlight > 0) hoverPrefetchInFlight--; };
        var run = function (signal) {
          fetch(url, { headers: { 'X-MediaView-Prefetch': '1' }, priority: 'low', signal: signal })
            .then(function (r) { return r.blob(); }) // 读掉 body，浏览器才会把它写进缓存
            .catch(function () { /* 预取失败/被取消都无所谓，用户点击时再取 */ })
            .then(done, done);
        };
        try {
          ctl = new AbortController();
          run(ctl.signal);
        } catch (e) {
          // 不支持 AbortController 就退化成不可取消的预取；若 run 仍然同步抛，
          // 必须把并发计数还回去，否则计数只增不减，预取会被永久闸住。
          try { run(undefined); } catch (e2) { done(); }
        }
      }, HOVER_PREFETCH_DELAY);
    });
    el.addEventListener('mouseleave', function () {
      clearTimeout(timer);
      // 鼠标已经移开：中止还没结束的预取，
      // 否则快速划过一排格子会留下一串正在跑的大图转码，白占 CPU 与带宽。
      if (ctl) {
        try { ctl.abort(); } catch (e) { /* 忽略 */ }
        ctl = null;
      }
    });
  }

  function suspendGridRequests() {
    resumeGridRequests(); // 先清掉上一次可能残留的记录
    for (var k in cellPool) {
      var el = cellPool[k];
      var im = el && el.querySelector ? el.querySelector('img.cell-thumb') : null;
      if (!im || im.complete) continue;
      gridSuspended.push({ img: im, src: im.getAttribute('src'), onerr: im.onerror });
      im.onerror = null; // 占位不是"加载失败"，别触发降级成文件图标
      im.src = BLANK_PIXEL;
    }
  }

  function resumeGridRequests() {
    for (var i = 0; i < gridSuspended.length; i++) {
      var e = gridSuspended[i];
      if (!e.img || !e.img.isConnected) continue;
      e.img.onerror = e.onerr;
      e.img.src = e.src; // 重新发起（多半直接命中浏览器缓存或服务端缓存）
    }
    gridSuspended = [];
  }

  // fileCard 缩略图关闭时的占位单元格
  function fileCard(item) {
    var box = document.createElement('div');
    box.className = 'cell-file';
    var ico = document.createElement('div');
    ico.className = 'cf-ico';
    ico.textContent = item.kind === 'video' ? '🎬' : '🖼️';
    var ext = document.createElement('div');
    ext.className = 'cf-ext';
    ext.textContent = String(item.ext || '').replace('.', '') || item.kind;
    var nm = document.createElement('div');
    nm.className = 'cf-name';
    nm.textContent = item.name;
    nm.title = item.name;
    box.appendChild(ico);
    box.appendChild(ext);
    box.appendChild(nm);
    return box;
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (m) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m];
    });
  }

  // 路径规范化：反斜杠转正斜杠、去末尾斜杠，用于跨平台比较
  function normPath(p) {
    return String(p || '').replace(/\\/g, '/').replace(/\/+$/, '');
  }
  function baseName(p) {
    var s = normPath(p);
    var i = s.lastIndexOf('/');
    return i >= 0 ? s.slice(i + 1) : s;
  }
  function dirName(p) {
    var s = normPath(p);
    var i = s.lastIndexOf('/');
    return i > 0 ? s.slice(0, i) : (i === 0 ? '/' : '');
  }

  // ================= VIEWER =================
  var v = {
    index: 0,
    list: [],
    media: null,
    scale: 1, tx: 0, ty: 0,
    zoomed: false,
    actual: false, // 是否处于「原始大小 1:1」查看（由右下角按钮切换）
    hiSrc: '', // 已升级为原图的 src（空 = 当前仍是缩放版）
  };

  function openViewer(target) {
    // 独立窗口模式：尝试通过宿主桥接打开独立看图窗口
    if (cfg.viewerMode === 'window') {
      openViewerWindow(target);
      return;
    }
    // 遮罩模式：原有逻辑
    openViewerOverlay(target);
  }

  // 原有遮罩模式逻辑（从 openViewer 重命名而来）
  function openViewerOverlay(target) {
    // build a list of only media items (skip dirs)
    v.list = state.items.filter(function (it) { return it.kind !== 'dir'; });
    var realIdx = v.list.indexOf(target);
    if (realIdx < 0) realIdx = 0;
    v.index = realIdx;
    viewer.classList.remove('hidden');
    applyViewerAnimation();
    // 先把网格在途的缩略图请求撤下来，再请求大图 —— 否则大图要排在它们后面
    suspendGridRequests();
    showCurrent(false); // 打开时只加载当前图，不预加载
    pauseThumbGen();
  }

  // 独立窗口模式：三层降级
  //   1) 宿主桌面子窗口（openApp —— 需在飞牛桌面 iframe 内，且宿主公开了该方法）
  //   2) 浏览器独立窗口（window.open）
  //   3) 窗口内遮罩
  // 每条失败分支都必须给 toast：历史 bug 里最常见的失败路径恰恰是静默的，
  // 用户以为「点了没反应」，开发者也拿不到任何线索。
  function openViewerWindow(target) {
    var path = target.path;
    var sort = state.sort || 'name';
    // 桌面子窗口是 app/ui/config 里单独注册的入口 mediaview.viewer，
    // 参数走 URL 查询串（宿主会把 params 合并进入口 URL），
    // 所以这里传的是一个「锚点」字符串，而不是位置参数。
    var anchor = 'mediaview.viewer?path=' + enc(path) + '&sort=' + enc(sort);
    var pageURL = GW + '/viewer?path=' + enc(path) + '&sort=' + enc(sort);

    // —— 第 1 层：在飞牛桌面 iframe 内 → 请宿主开桌面子窗口
    if (inHostIframe()) {
      hostLog('in host iframe, try openApp, anchor =', anchor);
      hostConnect().then(function (ok) {
        if (!ok) {
          hostLog('bridge not connected, reason =', host.reason);
          toast('独立窗口不可用（' + hostReasonText() + '），已切换为窗口内浏览');
          openViewerOverlay(target);
          return;
        }
        hostLog('available host methods:', Object.keys(host.methods));
        if (!host.methods['openApp']) {
          hostLog('host does not expose openApp');
          fallbackToBrowserWindow(target, pageURL, '宿主未开放独立窗口');
          return;
        }
        // 官方签名是单参 openApp(anchor)；传多个位置参数会被解析错。
        hostCall('openApp', anchor).then(function (ret) {
          hostLog('openApp success:', ret);
          toast('已在看图窗口打开');
        }).catch(function (err) {
          hostLog('openApp failed:', err);
          fallbackToBrowserWindow(target, pageURL, '桌面唤起失败');
        });
      });
      return;
    }

    // —— 非桌面 iframe：直接开浏览器窗口（同步发起，避免被弹窗拦截）
    hostLog('not in host iframe, open browser window directly');
    fallbackToBrowserWindow(target, pageURL, '当前不在飞牛桌面内');
  }

  // 第 2 层（浏览器独立窗口）+ 第 3 层（窗口内遮罩）兜底。
  // 注意：这里故意不加 noopener —— 加了 window.open 会返回 null，
  // 就没法判断「窗口到底开出来没有」，只能盲判成功。
  function fallbackToBrowserWindow(target, pageURL, reason) {
    var w = null;
    try {
      w = window.open(pageURL, 'mediaview_viewer_' + Date.now(), 'width=1280,height=880');
    } catch (e) {
      hostLog('window.open threw:', e);
    }
    if (w && !w.closed) {
      toast('已在独立窗口打开');
      return;
    }
    hostLog('window.open blocked, fallback to overlay');
    toast('独立窗口不可用（' + reason + '），已切换为窗口内浏览');
    openViewerOverlay(target);
  }

  // 大图浏览时暂停后台缩略图生成，退出后延迟恢复
  var resumeTimer = null;
  function pauseThumbGen() {
    if (resumeTimer) { clearTimeout(resumeTimer); resumeTimer = null; }
    fetch(API + '/thumb/pause', { method: 'POST' }).catch(function () {});
  }
  function resumeThumbGen() {
    if (resumeTimer) clearTimeout(resumeTimer);
    resumeTimer = setTimeout(function () {
      resumeTimer = null;
      fetch(API + '/thumb/resume', { method: 'POST' }).catch(function () {});
    }, 1000);
  }

  // Single-file open mode (fileTypes entry): ?path=...
  // 打开后异步加载同目录媒体，支持左右切换到相邻图片/视频
  function openSingle(path, sort) {
    var ext = baseName(path).toLowerCase().split('.').pop();
    var kind = videoExtJS(ext) ? 'video' : 'image';
    var name = baseName(path);
    v.list = [{ kind: kind, path: path, name: name }];
    v.index = 0;
    viewer.classList.remove('hidden');
    $('topbar').style.display = 'none';
    $('main').style.display = 'none';
    suspendGridRequests();
    showCurrent(false); // 打开时只加载当前图
    pauseThumbGen();

    // 异步拉取同目录列表，把当前文件定位进去，即可左右切换
    var dir = dirName(path);
    if (!dir) return;
    var sortParam = sort || 'name';
    fetchJSON(API + '/list?path=' + enc(dir) + '&sort=' + enc(sortParam)).then(function (d) {
      var files = d.files || [];
      if (files.length <= 1) return;
      var target = normPath(path);
      var idx = -1;
      for (var i = 0; i < files.length; i++) {
        if (normPath(files[i].path) === target) { idx = i; break; }
      }
      if (idx < 0) {
        // 当前文件不在列表里（排序/过滤差异）：插到开头，仍可浏览目录其他文件
        files.unshift({ kind: kind, path: path, name: name });
        idx = 0;
      }
      // 列表返回后**绝不能重建 current**：重建会新建 <img>、重新解码，
      // 而新图在 decode 完成前是 visibility:hidden —— 用户看到的就是
      // 「图片消失一下再出现」。悬停预取让首图秒出之后，这个问题才暴露出来
      // （过去转码要等好几秒，列表早就返回并重建完了，图还没显示，所以看不见）。
      var shown = v.list[v.index];
      var sameFile = shown && normPath(shown.path) === normPath(files[idx].path);
      v.list = files;
      v.index = idx;
      flashCounter();
      if (sameFile) {
        ensureNextBox(); // 当前图原地不动，只补建 next 盒
      } else {
        showCurrent(false);
      }
    }).catch(function () { /* 列表加载失败不影响当前文件查看 */ });
  }
  function videoExtJS(ext) {
    return ['mp4','mov','mkv','avi','webm','m4v','flv','wmv','ts','mts','m2ts','mpg','mpeg','3gp','3g2','rmvb','vob','f4v','rm'].indexOf(ext) >= 0;
  }

  // 把「当前文件名」写到窗口标题：宿主小窗标题栏（setTitle）+ document.title 兜底。
  // 抽成函数是因为握手是**异步**的：独立窗口刚打开时 host 还没连上，那一次 setTitle
  // 会被跳过 —— 现象就是「刚打开时左上角标题栏空白，切到下一张才出现文件名」。
  // 所以除了换图要调它，握手完成时也要补调一次（见下面的 hostConnect().then）。
  function updateWindowTitle() {
    if (!singleMode) return;
    var item = v.list && v.list[v.index];
    if (!item || !item.name) return;
    // 本地先改 document.title（SDK 的 setTitle 内部也会改），
    // 再请宿主改小窗标题栏 —— 宿主不支持时不影响浏览。
    document.title = item.name + ' · 极速媒体浏览';
    if (host.connected && host.methods['setTitle']) {
      hostCall('setTitle', item.name).catch(function () {});
    }
  }

  function showCurrent(preload) {
    if (preload === undefined) preload = true;
    // 统一兜底：v.index 越界时直接返回，避免下游 item.kind / createMediaBox 抛错。
    // 比逐点贴 `item &&` 更可靠，一处守卫覆盖整条调用链。
    if (!v.list || v.list.length === 0 || v.index < 0 || v.index >= v.list.length) return;
    var item = v.list[v.index];
    updateWindowTitle();
    resetView();
    flashCounter();
    var vBtnDisp = item && item.kind === 'video' ? 'none' : '';
    $('vFit').style.display = vBtnDisp;
    if ($('vActual')) $('vActual').style.display = vBtnDisp;
    rebuildTrackInPlace(preload);
  }

  // 原地重建 track：移除旧子元素，添加 prev/current/next，设置 transform 为 -100%
  // 始终保持三个子元素（无 prev/next 时用空占位），确保 translateX(-100%) 始终显示中间的 current
  // preload=false（打开时）：current + next 进 DOM（current 排前面优先加载），不预加载更多后面的图
  // preload=true（切换后）：current + next 进 DOM，并预加载 viewerPreload-1 张后面的图
  function rebuildTrackInPlace(preload) {
    var track = vWrap.querySelector('.v-track');
    if (!track) {
      track = document.createElement('div');
      track.className = 'v-track';
      vWrap.appendChild(track);
    }
    track.classList.remove('anim');
    track.style.transitionDuration = '';
    // 移除所有子元素
    while (track.firstChild) {
      var old = track.firstChild;
      if (old._media && old._media.tagName === 'VIDEO') {
        old._media.pause();
      }
      track.removeChild(old);
    }
    // current 始终渲染（优先创建，浏览器优先加载）
    var curBox = createMediaBox(v.list[v.index], true);
    // prev：空占位（不预加载上一张，减少内存；左滑时才加载）
    var prevBox = emptyBox();
    // next：始终渲染（只要 viewerPreload >= 1），确保第一次切换时 next 已在 DOM、不闪屏
    // current 在 DOM 中排前面，浏览器仍优先加载 current
    var nextBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index < v.list.length - 1) {
      nextBox = createMediaBox(v.list[v.index + 1], false);
    }

    track.appendChild(prevBox);
    track.appendChild(curBox);
    v.media = curBox._media;
    v.hiSrc = ''; // 换了一张（或原地重建）→ 重新从缩放版开始，放大时再按需升级
    v.actual = false; // 换图后回到「适应窗口」
    updateFitBtn();
    track.appendChild(nextBox);

    // 重置位置（无动画），中间子元素 = current
    track.style.transform = 'translateX(-100%)';

    // 预加载后面的图片（next 已在 track 里，从 next+1 开始预加载 viewerPreload-1 张）
    if (preload && cfg.viewerPreload > 1) {
      preloadAhead(cfg.viewerPreload - 1);
    }
    // preload=false 时：next 不进 DOM（首屏只加载 current，保证打开速度），
    // 但后台低优先级预加载 next 原图，第一次切换时已缓存、不闪烁
    if (!preload) {
      preloadNextImage();
    }
  }

  // 预加载当前图后面的 count 张（从 next+1 开始），浏览器缓存后切换时秒开
  var preloadCache = {};
  function preloadAhead(count) {
    for (var i = 0; i < count; i++) {
      var idx = v.index + 2 + i; // index+1=next 已在 track，从 index+2 开始
      if (idx >= v.list.length) break;
      var item = v.list[idx];
      if (item.kind !== 'image') continue;
      var key = item.path;
      if (preloadCache[key]) continue;
      preloadCache[key] = true;
      var img = new Image();
      img.src = rawURL(item.path, pickMaxdim());
      if (img.decode) img.decode().catch(function () {}); // 提前解码，切换时秒开
    }
  }

  // 后台低优先级预加载 next 原图（不进 DOM，只让浏览器缓存）
  // 打开时调用：current 在 DOM 里优先加载，next 后台悄悄缓存，第一次切换不闪烁
  // 切换时调用：动画开始就缓存新 next，连续切换时每张都提前准备好
  function preloadNextImage() {
    // viewerPreload=0 时不预加载任何图（用户明确要求零预加载）
    if (cfg.viewerPreload < 1) return;
    if (v.index >= v.list.length - 1) return;
    var item = v.list[v.index + 1];
    if (item.kind !== 'image') return;
    var key = item.path;
    if (preloadCache[key]) return;
    preloadCache[key] = true;

    var doPreload = function () {
      var img = new Image();
      img.fetchPriority = 'low';
      img.src = rawURL(item.path, pickMaxdim());
      if (img.decode) img.decode().catch(function () {}); // 提前解码，切换时秒开
    };

    // O5：延后到首图（current）加载完成后再发起 next 预加载，
    // 避免两张图同时跑后端 ffmpeg 转码、在低功耗 NAS 上互相抢 CPU 导致首图更慢。
    // 首图的转码优先级最高，next 等首图就绪后再跑。fetchPriority='low' 也让
    // 浏览器在网络调度上优先首图。
    var m = v.media;
    if (m && m.complete && (m.naturalWidth > 0 || m.readyState >= 2)) {
      doPreload();
    } else if (m) {
      var fired = false;
      var onReady = function () {
        if (fired) return;
        fired = true;
        doPreload();
      };
      if (m.tagName === 'IMG') {
        m.addEventListener('load', onReady, { once: true });
        m.addEventListener('error', onReady, { once: true });
      } else {
        m.addEventListener('loadeddata', onReady, { once: true });
        m.addEventListener('error', onReady, { once: true });
      }
      // 超时兜底：首图 3 秒还没加载完也发起预加载
      // （避免首图异常/超大图时 next 永远不预加载，第一次切换仍要等转码）
      setTimeout(onReady, 3000);
    } else {
      doPreload();
    }
  }

  function emptyBox() {
    var d = document.createElement('div');
    d.className = 'v-vbox';
    return d;
  }

  // 把轨道结构调整成「中间那张 = v.list[v.index]」，并**复用 DOM 里已有的那一盒**。
  // 为什么必须复用：重建会新建 <img> 元素，而新元素在 decode 完成前是 visibility:hidden，
  // 于是画面会「消失一下再出现」。复用同一个元素时位图还在内存里，重新插入后立即可绘制。
  // 用于「动画期间再次切换」这类需要立刻跳到目标图的场景。
  function syncTrackToIndex(track) {
    var want = v.list[v.index];
    if (!want) return null;
    var wantPath = normPath(want.path);
    var keep = null;
    for (var i = 0; i < track.children.length; i++) {
      var b = track.children[i];
      if (b._itemPath && normPath(b._itemPath) === wantPath) { keep = b; break; }
    }
    // 先把要复用的盒子摘出来，再清空轨道，否则它会被一起清掉
    if (keep && keep.parentNode) keep.parentNode.removeChild(keep);
    while (track.firstChild) {
      var old = track.firstChild;
      if (old._media && old._media.tagName === 'VIDEO') old._media.pause();
      track.removeChild(old);
    }
    track.classList.remove('anim');
    track.style.transitionDuration = '';
    var curBox = keep || createMediaBox(want, true);
    var nextBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index < v.list.length - 1) {
      nextBox = createMediaBox(v.list[v.index + 1], false);
    }
    track.appendChild(emptyBox()); // prev 用空占位（左滑时才加载）
    track.appendChild(curBox);
    track.appendChild(nextBox);
    track.style.transform = 'translateX(-100%)';
    v.media = curBox._media;
    v.hiSrc = '';
    v.actual = false;
    if (v.media && v.media.tagName === 'VIDEO') v.media.play().catch(function () {});
    updateFitBtn();
    return curBox;
  }

  // list 返回后补建 next 盒（不重建 current）。
  // track 结构：[prev(empty), current, next]。next 位置可能是 emptyBox（首屏只加载 current 时），
  // 这里把它替换成真正的 next 媒体盒，确保第一次左/右切换时 next 已在 DOM、不闪屏。
  function ensureNextBox() {
    var track = vWrap.querySelector('.v-track');
    if (!track) return;
    if (cfg.viewerPreload < 1 || v.index >= v.list.length - 1) return;
    var nextItem = v.list[v.index + 1];
    if (!nextItem) return;
    // next 位置（第 3 个子元素，索引 2）已有媒体 → 不重复创建
    if (track.children.length >= 3 && track.children[2].querySelector('img,video')) return;
    var nextBox = createMediaBox(nextItem, false);
    if (track.children.length >= 3) {
      track.replaceChild(nextBox, track.children[2]);
    } else {
      track.appendChild(nextBox);
    }
  }

  // 大图加载指示与「露面」时机（用法见 createMediaBox）
  var VIEWER_SPIN_DELAY = 150;      // 延时多久才显示转圈：缓存命中时它根本不出现
  var VIEWER_SPIN_MIN = 300;        // 一旦显示就至少停这么久，避免刚出现就消失地抖一下
  var VIEWER_REVEAL_TIMEOUT = 2500; // 兜底：解码异常时也要让图片露面

  // 创建单个媒体容器（图片或视频），isCurrent 决定是否自动播放视频
  function createMediaBox(item, isCurrent) {
    var box = document.createElement('div');
    box.className = 'v-vbox';
    box._itemPath = item.path; // 供 syncTrackToIndex 复用「已经在 DOM 里的那一盒」
    if (item.kind === 'video') {
      var video = document.createElement('video');
      video.controls = true;
      video.playsInline = true;
      video.preload = 'metadata';
      video.src = rawURL(item.path);
      if (isCurrent) video.autoplay = true;
      video.onerror = function () { toast('该编码可能不被浏览器支持'); };
      box.appendChild(video);
      box._media = video;
    } else {
      // 加载指示：延时出现 + 最短停留，避免「秒开的图也闪一下转圈」。
      // 当前图等位图 decode 完成再露面，且是**瞬时**显示（不做淡入）：
      //   - 大 JPEG 会从上往下逐行绘制，那本身就是一种闪烁；
      //   - opacity 淡入会在滑动时叠加晃眼（1.8.34 特意去掉过，别改回去）。
      var spin = null, spinTimer = null, spinShownAt = 0;
      if (isCurrent) {
        spin = document.createElement('div');
        spin.className = 'v-spin-box';
        spin.innerHTML = '<div class="v-spin"></div>';
        box.appendChild(spin);
        spinTimer = setTimeout(function () {
          spinShownAt = Date.now();
          spin.classList.add('show');
        }, VIEWER_SPIN_DELAY);
      }
      var dropSpin = function () {
        if (!spin) return;
        clearTimeout(spinTimer);
        var wait = spinShownAt ? Math.max(0, VIEWER_SPIN_MIN - (Date.now() - spinShownAt)) : 0;
        setTimeout(function () {
          spin.classList.remove('show');
          setTimeout(function () { if (spin.parentNode) spin.parentNode.removeChild(spin); }, 220);
        }, wait);
      };

      var img = document.createElement('img');
      img.alt = item.name || '';
      img.decoding = 'async';
      img.fetchPriority = 'high'; // 主图优先，别被后台预加载挤到后面
      var revealed = false;
      var reveal = function () {
        if (revealed) return;
        revealed = true;
        img.style.visibility = '';
        img.classList.add('loaded');
        dropSpin();
      };
      if (isCurrent) img.style.visibility = 'hidden'; // visibility 不产生动画，只是"先不露面"
      img.onload = function () {
        if (img.decode) { img.decode().then(reveal, reveal); } else { reveal(); }
      };
      img.onerror = function () { reveal(); toast('无法加载图片'); };
      var md = pickMaxdim();
      img._maxdim = md; // 记住这张图按哪个预算取的，供视口变大后重取判断
      img.src = rawURL(item.path, md);
      if (isCurrent) setTimeout(reveal, VIEWER_REVEAL_TIMEOUT); // 解码卡死也不能永远不露面
      box.appendChild(img);
      box._media = img;
      attachImageGestures(img);
    }
    return box;
  }

  var counterTimer = null;
  function flashCounter() {
    vCounterFloat.textContent = (v.index + 1) + ' / ' + v.list.length;
    vCounterFloat.style.opacity = '1';
    clearTimeout(counterTimer);
    counterTimer = setTimeout(function () {
      vCounterFloat.style.opacity = '.6';
    }, 1500);
  }

  function resetView() {
    v.scale = 1; v.tx = 0; v.ty = 0; v.zoomed = false; v.actual = false;
    updateFitBtn();
  }

  // 计算「原始大小（1:1）」需要多少 scale。
  // 基准：1 个图像像素对应 1 个设备像素（物理像素），因此要除以 devicePixelRatio ——
  // 在 125%/150% 缩放的屏幕上，若按 CSS 像素算会显示成 1.25/1.5 倍，就不是"原始大小"了。
  // 布局宽度 clientWidth 受 max-width:100% 约束（与是否 transform 无关），可直接当基准。
  // w 可显式传入（换图瞬间主图的 naturalWidth 还是旧值，此时要用 probe 的尺寸）。
  function applyActualScale(w) {
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    var cw = m.clientWidth;
    var nw = w || m.naturalWidth;
    if (!cw || !nw) return;
    var dpr = window.devicePixelRatio || 1;
    v.scale = Math.max(1, nw / (cw * dpr));
    v.zoomed = v.scale > 1.01;
    if (!v.zoomed) { v.tx = 0; v.ty = 0; }
  }

  function applyTransform() {
    if (v.media && v.media.tagName === 'IMG') {
      // 缩放回到 1x 时强制居中，避免任何路径下图片偏移看不到
      if (v.scale <= 1.01) { v.tx = 0; v.ty = 0; }
      // 拖拽边界限制：图片不能被拖出窗口（放大后至少保留一条边在窗口内；
      // 图片比窗口小时居中不偏移）。
      //
      // ★ 基准必须是【布局显示尺寸】clientWidth/clientHeight，不是 naturalWidth：
      //   CSS `.v-media-wrap img { max-width:100%; max-height:100% }` 会把图片压到
      //   容器内（例如 4000px 的原图显示成 1200px），而 transform:scale() 恰恰作用在
      //   这个布局尺寸上。若按原始像素算，可拖范围会被放大约 (natural / display) 倍
      //   （4000/1200 ≈ 3.3 倍），钳位形同虚设 —— 图片能被拖到完全离开视口，
      //   且缩小时因阈值仍偏大而收不回来。
      else {
        var dispW = v.media.clientWidth;   // 布局宽度（不含 transform，正是 scale 的作用基）
        var dispH = v.media.clientHeight;
        if (dispW > 0 && dispH > 0) {
          var wrapW = vWrap.offsetWidth;
          var wrapH = vWrap.offsetHeight;
          // 图片缩放后比容器宽/高出的部分的一半 = 中心点可偏移的最大量。
          // 这样图片边缘永远不会越过容器边缘（不露背景），也就绝不会被拖出窗口。
          var maxTx = (dispW * v.scale - wrapW) / 2;
          var maxTy = (dispH * v.scale - wrapH) / 2;
          if (maxTx > 0) { v.tx = Math.max(-maxTx, Math.min(maxTx, v.tx)); }
          else { v.tx = 0; }
          if (maxTy > 0) { v.ty = Math.max(-maxTy, Math.min(maxTy, v.ty)); }
          else { v.ty = 0; }
        }
      }
      v.media.style.transform = 'translate(' + v.tx + 'px,' + v.ty + 'px) scale(' + v.scale + ')';
    }
  }

  // 放大升级：两级制 —— 浏览用 2048px 预缩版，放大超过 1.5x 直接换原图。
  // 去掉了旧版的 needPx 计算、512 阶梯、逐级升级，逻辑从 6 层判断减到 3 层。
  function maybeUpgradeToFull(force) {
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    // 正在加载中（v.hiSrc 与当前 src 不同）→ 阻止重复请求
    if (v.hiSrc && v.hiSrc !== m.getAttribute('src')) return;
    // 非 1:1 按钮时，放大不到 1.5x 不升级（2048px 预缩版在 1.5x 内仍 1:1 命中）
    if (!force && v.scale < 1.5) return;
    // 已经在用源文件了 → 不用再请求。看 src 带不带 maxdim：带 = 还是预缩版，不带 = 已是源文件。
    if (!/(^|[?&])maxdim=/.test(m.getAttribute('src') || '')) return;
    var item = v.list[v.index];
    if (!item || !item.path) return;
    var url = rawURL(item.path); // 直接要原图
    if (m.getAttribute('src') === url) return;
    v.hiSrc = url;
    var probe = new Image();
    var apply = function () {
      if (v.hiSrc !== url || v.media !== m) return;
      // 1:1 按钮时趁 probe 持有原图尺寸先摆正倍数
      if (v.actual && probe.naturalWidth > 0) {
        applyActualScale(probe.naturalWidth);
        applyTransform();
      }
      m._maxdim = Infinity; // 已是源文件，视口再变大也没有更高的了
      m.src = url;
    };
    // 先 decode() 再换 src，消除闪屏空白帧
    probe.onload = function () {
      if (probe.decode) { probe.decode().then(apply, apply); } else { apply(); }
    };
    probe.onerror = function () { if (v.hiSrc === url) v.hiSrc = ''; };
    probe.src = url;
  }

  // 视口变大后按新预算重取当前图。全屏按钮只是 requestFullscreen()，不换图，
  // 所以全屏后 2048 预缩版被拉伸到更大设备像素会糊。只升不降：视口变小保留高清版。
  function refreshMaxdimForViewport() {
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    var cur = m._maxdim || 0;
    if (!isFinite(cur)) return; // 已经在用源文件，没有更高的了
    var want = pickMaxdim();
    if (want <= cur) return; // 新预算没有变大
    var item = v.list[v.index];
    if (!item || !item.path) return;
    var url = rawURL(item.path, want);
    if (m.getAttribute('src') === url) return;
    if (v.hiSrc && v.hiSrc !== m.getAttribute('src')) return; // 已有一次切换在途
    v.hiSrc = url;
    var probe = new Image();
    var apply = function () {
      if (v.hiSrc !== url || v.media !== m) return;
      m._maxdim = want;
      m.src = url;
    };
    // 先 decode() 再换 src，避免闪一帧空白
    probe.onload = function () {
      if (probe.decode) { probe.decode().then(apply, apply); } else { apply(); }
    };
    probe.onerror = function () { if (v.hiSrc === url) v.hiSrc = ''; };
    probe.src = url;
  }

  // 全屏切换后视口尺寸要等过渡结束才更新，延后一拍再算
  document.addEventListener('fullscreenchange', function () {
    setTimeout(refreshMaxdimForViewport, 60);
  });
  document.addEventListener('webkitfullscreenchange', function () {
    setTimeout(refreshMaxdimForViewport, 60);
  });
  window.addEventListener('orientationchange', function () {
    setTimeout(refreshMaxdimForViewport, 200);
  });
  // 拖窗口改大小：防抖 300ms，避免拖动过程中反复发起请求
  var resizePickTimer = null;
  window.addEventListener('resize', function () {
    clearTimeout(resizePickTimer);
    resizePickTimer = setTimeout(refreshMaxdimForViewport, 300);
  });

  // 图片手势：只处理缩放（双击/滚轮），平移和切换由 vWrap 的滑动逻辑统一处理
  function attachImageGestures(img) {
    img.classList.add('zoomable');
    img.addEventListener('dblclick', function (e) {
      if (v.zoomed) { resetView(); }
      else { v.scale = 2; v.zoomed = true; v.actual = false; updateFitBtn(); }
      applyTransform();
      maybeUpgradeToFull();
    });
    img.addEventListener('wheel', function (e) {
      e.preventDefault();
      var factor = e.deltaY < 0 ? 1.15 : 0.87;
      v.actual = false; // 手动缩放后不再是「原始大小」
      updateFitBtn();
      // 放大上限 6x。超过 1.5x 后 maybeUpgradeToFull 会直接升级原图，
      // 之后继续放大是原图 1:1 或插值，不会糊。
      v.scale = Math.min(6, Math.max(1, v.scale * factor));
      v.zoomed = v.scale > 1.01;
      if (!v.zoomed) { v.tx = 0; v.ty = 0; }
      applyTransform();
      maybeUpgradeToFull();
    }, { passive: false });
  }

  // ---------- 跟手滑动切换（手机相册式） ----------
  var swipe = {
    active: false, startX: 0, startY: 0, dx: 0, dy: 0,
    locked: false, panStart: null, track: null,
    lastX: 0, lastTime: 0, velocity: 0
  };
  // 切换序号：每次发起切换自增，用来作废「过期的延迟回调」
  // （等目标图 decode 的 doIt、settleAfterSwipe 的 transitionend 与 400ms 兜底）。
  var swipeSeq = 0;

  vWrap.addEventListener('pointerdown', function (e) {
    if (e.target.closest('.v-controls')) return;
    // 视频也允许滑动切换（点击由 click 事件区分左右区域）
    swipe.active = true;
    swipe.startX = e.clientX;
    swipe.startY = e.clientY;
    swipe.lastX = e.clientX;
    swipe.lastTime = Date.now();
    swipe.velocity = 0;
    swipe.dx = 0;
    swipe.dy = 0;
    swipe.locked = false;
    if (v.zoomed) {
      swipe.panStart = { x: e.clientX - v.tx, y: e.clientY - v.ty };
      return;
    }
    var track = vWrap.querySelector('.v-track');
    if (track) {
      track.classList.remove('anim');
      track.style.transitionDuration = '';
      swipe.track = track;
    }
  });

  vWrap.addEventListener('pointermove', function (e) {
    if (!swipe.active) return;
    swipe.dx = e.clientX - swipe.startX;
    swipe.dy = e.clientY - swipe.startY;

    // 记录速度（px/ms）
    var now = Date.now();
    var dt = now - swipe.lastTime;
    if (dt > 0) {
      var instV = (e.clientX - swipe.lastX) / dt;
      // 平滑速度
      swipe.velocity = swipe.velocity * 0.6 + instV * 0.4;
    }
    swipe.lastX = e.clientX;
    swipe.lastTime = now;

    // 缩放状态：平移图片
    if (v.zoomed && swipe.panStart) {
      v.tx = e.clientX - swipe.panStart.x;
      v.ty = e.clientY - swipe.panStart.y;
      applyTransform();
      return;
    }
    if (v.zoomed) return;

    // 未缩放：锁定方向后水平跟手滑动
    if (!swipe.locked) {
      if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) {
        swipe.locked = true;
        swipe.horizontal = Math.abs(swipe.dx) > Math.abs(swipe.dy);
      } else {
        return;
      }
    }
    if (!swipe.horizontal || !swipe.track) return;
    e.preventDefault();
    var pct = (swipe.dx / vWrap.offsetWidth) * 100;
    // 边界阻尼
    if ((v.index === 0 && pct > 0) || (v.index === v.list.length - 1 && pct < 0)) {
      pct = pct * 0.3;
    }
    swipe.track.style.transform = 'translateX(calc(-100% + ' + pct + '%))';
  });

  function endSwipe() {
    if (!swipe.active) return;
    swipe.active = false;
    swipe.panStart = null;
    if (v.zoomed) return;
    if (!swipe.track || !swipe.horizontal) { swipe.track = null; return; }

    var track = swipe.track;
    var w = vWrap.offsetWidth;
    var threshold = w * 0.18;
    var fast = Math.abs(swipe.velocity) > 0.25; // 快速滑动阈值

    var dir = 0;
    if ((swipe.dx > threshold || (fast && swipe.dx > 15)) && v.index > 0) {
      dir = -1; // 向右滑 → 前一张
    } else if ((swipe.dx < -threshold || (fast && swipe.dx < -15)) && v.index < v.list.length - 1) {
      dir = 1;  // 向左滑 → 下一张
    }

    // 动画时长：快速滑动更短更跟手，慢速滑动稍长
    var dur = fast ? 180 : 260;
    track.style.transitionDuration = dur + 'ms';
    track.classList.add('anim');

    if (dir === -1) {
      track.style.transform = 'translateX(0)';
      v.index--;
      preloadNextImage(); // 动画开始就预加载新 next，不等动画结束
      settleAfterSwipe(track, -1);
    } else if (dir === 1) {
      track.style.transform = 'translateX(-200%)';
      v.index++;
      preloadNextImage(); // 动画开始就预加载新 next，不等动画结束
      settleAfterSwipe(track, 1);
    } else {
      track.style.transform = 'translateX(-100%)';
    }
    swipe.track = null;
  }
  vWrap.addEventListener('pointerup', endSwipe);
  vWrap.addEventListener('pointercancel', endSwipe);

  // 动画结束后静默调整 track：不销毁当前图，只移除旧的相邻图、添加新的相邻图
  function settleAfterSwipe(track, dir) {
    var settled = false;
    var mySeq = swipeSeq;
    var timer = null;
    var handler = function () {
      if (settled) return;
      // 本轮回调只对「自己那一轮切换」有效：动画被取消（连点直接跳转）时，
      // 旧 handler 与 400ms 兜底定时器仍然活着，撞上新一轮动画就会按旧 dir
      // 再重排一次轨道，与新 handler 叠加成错位/空白。
      if (mySeq !== swipeSeq) {
        track.removeEventListener('transitionend', handler);
        clearTimeout(timer);
        return;
      }
      // 动画被外部取消（快速连点直接跳转）时不执行 settle
      if (!track.classList.contains('anim')) return;
      settled = true;
      track.removeEventListener('transitionend', handler);
      clearTimeout(timer);
      track.classList.remove('anim');
      track.style.transitionDuration = '';

      if (dir === -1) {
        // 向右滑：移除最右的 old next，左边插入新 prev（无则空占位）
        if (track.children.length > 2) track.removeChild(track.children[track.children.length - 1]);
        if (v.index > 0) {
          track.insertBefore(createMediaBox(v.list[v.index - 1], false), track.firstChild);
        } else {
          track.insertBefore(emptyBox(), track.firstChild);
        }
      } else {
        // 向左滑：移除最左的 old prev，右边追加新 next（无则空占位）
        if (track.children.length > 0) track.removeChild(track.children[0]);
        if (v.index < v.list.length - 1) {
          track.appendChild(createMediaBox(v.list[v.index + 1], false));
        } else {
          track.appendChild(emptyBox());
        }
      }

      // 静默重置位置（无动画），当前图回到中间
      track.style.transform = 'translateX(-100%)';

      // 更新 v.media 指向中间的当前图
      var mid = track.children[Math.min(1, track.children.length - 1)];
      if (mid && mid._media) {
        v.media = mid._media;
        if (v.media.tagName === 'VIDEO') v.media.play().catch(function () {});
      }
      // 暂停非当前视频
      for (var i = 0; i < track.children.length; i++) {
        var m = track.children[i]._media;
        if (m && m.tagName === 'VIDEO' && m !== v.media) m.pause();
      }

      // 换了当前图：清掉上一张的「已升级原图」标记。
      // 不清的话，切走再切回同一张时 1:1 会被误判成"原图还在加载"而直接返回，
      // 用户以为在看原图细节，其实还是预缩版放大出来的。
      v.hiSrc = '';
      resetView();
      flashCounter();
      var item = v.list[v.index];
      updateWindowTitle(); // 独立窗口模式：切换图片时同步窗口标题栏
      var vBtnDisp = item && item.kind === 'video' ? 'none' : '';
    $('vFit').style.display = vBtnDisp;
    if ($('vActual')) $('vActual').style.display = vBtnDisp;
    if (cfg.viewerPreload > 1) preloadAhead(cfg.viewerPreload - 1);
    };
    track.addEventListener('transitionend', handler);
    // 兜底：transitionend 可能因属性变化不触发
    timer = setTimeout(handler, 400);
  }

  // 点击左右区域 / 键盘切换：用滑动动画
  function swipeTo(dir) {
    if (dir < 0 && v.index === 0) return;
    if (dir > 0 && v.index === v.list.length - 1) return;
    if (v.zoomed) { resetView(); applyTransform(); return; }
    var track = vWrap.querySelector('.v-track');
    if (!track) { v.index += dir; showCurrent(); return; }
    var mySeq = ++swipeSeq; // 本次切换的序号（见 doIt / settleAfterSwipe 的过期判定）

    // 方案三：动画期间再次切换 → 取消当前动画，直接静默跳到目标图（不叠加动画）。
    // 注意：这里**不能** rebuildTrackInPlace —— 那会新建 <img>，而新图在 decode 前是
    // visibility:hidden，用户会看到画面"消失一下再出现"。改用 syncTrackToIndex：
    // 目标图若已在 DOM 里就复用同一个元素，位图还在内存中，重新插入即可立即绘制。
    if (track.classList.contains('anim')) {
      track.classList.remove('anim');
      track.style.transitionDuration = '';
      v.index += dir;
      if (v.index < 0) v.index = 0;
      if (v.index > v.list.length - 1) v.index = v.list.length - 1;
      syncTrackToIndex(track);
      preloadNextImage();
      flashCounter();
      updateWindowTitle(); // 动画中连点切换同样要更新标题栏
      var item = v.list[v.index];
      var vBtnDisp = item && item.kind === 'video' ? 'none' : '';
      $('vFit').style.display = vBtnDisp;
      if ($('vActual')) $('vActual').style.display = vBtnDisp;
      return;
    }

    // 切换前确保目标方向的相邻图是真实媒体元素（打开时可能是空占位）
    if (dir > 0 && track.children[2] && !track.children[2]._media) {
      if (v.index < v.list.length - 1) {
        track.replaceChild(createMediaBox(v.list[v.index + 1], false), track.children[2]);
      }
    }
    if (dir < 0 && track.children[0] && !track.children[0]._media) {
      if (v.index > 0) {
        track.replaceChild(createMediaBox(v.list[v.index - 1], false), track.children[0]);
      }
    }

    // 目标图必须「位图已解码」才能开始动画。
    // complete 只代表数据下载完，不代表位图已解码——动画开始时浏览器边解码边显示，
    // 就是"闪一下然后加载出来"。竖图全屏时高度=屏幕高，解码量大更明显。
    // img.decode() resolve 时位图已就绪，可直接绘制；已解码的图会立即 resolve。
    var targetBox = dir > 0 ? track.children[2] : track.children[0];
    var targetImg = targetBox && targetBox._media && targetBox._media.tagName === 'IMG' ? targetBox._media : null;
    if (targetImg) {
      var fired = false;
      var doIt = function () {
        if (fired) return;
        fired = true;
        // 期间若又来了新的切换请求，本次已过期：直接作废。
        // 否则两个 doIt 各执行一次 doSwipe → v.index 走两格而轨道只前进一格，
        // 显示的图、计数、标题、1:1 升级目标全部错位。
        if (mySeq !== swipeSeq) return;
        doSwipe(track, dir);
      };
      if (targetImg.decode) {
        targetImg.decode().then(doIt, doIt); // 解码成功/失败都开始（失败兜底显示）
      } else {
        doIt(); // 浏览器不支持 decode()，直接开始
      }
      setTimeout(doIt, 1500); // 超时兜底：解码卡死时强制切换
      return;
    }

    doSwipe(track, dir);
  }

  // 实际执行滑动动画（目标图已确认加载完成）
  function doSwipe(track, dir) {
    // 边界守卫必须放在这里：本函数可能被「等图加载」的延迟回调调用，
    // 那时 swipeTo 入口的边界检查已经过期（index 可能已被改动）。
    if (dir < 0 && v.index === 0) return;
    if (dir > 0 && v.index === v.list.length - 1) return;
    track.style.transitionDuration = '220ms';
    track.classList.add('anim');
    if (dir < 0) { track.style.transform = 'translateX(0)'; v.index--; }
    else { track.style.transform = 'translateX(-200%)'; v.index++; }
    // 方案二：动画开始就预加载新 next，不等 transitionend
    preloadNextImage();
    settleAfterSwipe(track, dir);
  }
  function next() { swipeTo(1); }
  function prev() { swipeTo(-1); }

  function closeViewer() {
    viewer.classList.add('hidden');
    vWrap.innerHTML = '';
    resumeGridRequests(); // 恢复被挂起的网格缩略图请求
    if (singleMode) {
      // 单文件模式（独立窗口）：关闭整个窗口。
      // 先请宿主关（内嵌 iframe 里 window.close() 可能被忽略），失败再退回 window.close()。
      hostLog('single mode: ask host to close viewer window');
      hostCall('close').then(function () {
        window.close();
      }).catch(function () {
        window.close();
      });
      resumeThumbGen();
      return;
    }
    $('topbar').style.display = '';
    $('main').style.display = '';
    resumeThumbGen();
  }

  // ---------- Viewer buttons / keys ----------
  // 左上角序号：overlay 模式下点击关闭看图器（独立窗口模式有标题栏关闭按钮，不触发）
  vCounterFloat.addEventListener('click', function () {
    if (!singleMode) closeViewer();
  });
  // 右下角按钮：在「适应窗口 ⇄ 原始大小(1:1)」之间切换。
  // ⤢ = 浏览器全屏（F11 效果）。恢复原有行为：全屏后窗口变大，图片的布局尺寸
  // 随之变大、可见细节变多，这是"看得更清楚"最直接也最符合习惯的手段。
  $('vFit').onclick = function () {
    if (!document.fullscreenElement) {
      var el = document.documentElement;
      if (el.requestFullscreen) { el.requestFullscreen().catch(function () {}); }
      else if (el.webkitRequestFullscreen) { el.webkitRequestFullscreen(); }
    } else {
      if (document.exitFullscreen) { document.exitFullscreen().catch(function () {}); }
      else if (document.webkitExitFullscreen) { document.webkitExitFullscreen(); }
    }
  };

  // 1:1 = 原始大小。复用已有的 transform + 拖拽链路（scale = 原图像素 / 布局像素 / DPR），
  // 不新增布局模式，平移、边界钳位、手势全部沿用原有逻辑。
  // 与 ⤢ 分开成两个按钮：⤢ 是改变"窗口"（看更多内容），1:1 是改变"像素对应关系"（看真实细节）。
  function updateFitBtn() {
    var b = $('vActual');
    if (!b) return;
    var want = v.actual ? '适应' : '1:1';
    if (b.textContent === want) return; // 滚轮会高频调用，值没变就不动 DOM
    b.textContent = want;
    b.title = v.actual ? '适应窗口' : '原始大小（1:1）';
  }

  var vActualBtn = $('vActual');
  if (vActualBtn) vActualBtn.onclick = function () {
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    if (v.actual) {
      resetView();
      applyTransform();
    } else {
      v.actual = true;
      applyActualScale();
      applyTransform();
      // force=true：1:1 必须用真正的原图，否则"原始大小"只是预缩版的 1:1。
      maybeUpgradeToFull(true);
    }
    updateFitBtn();
  };
  updateFitBtn();

  // 点击左/右半区切换（滑动后不触发，避免误触）
  $('vStage').addEventListener('click', function (e) {
    if (e.target.closest('.v-controls')) return;
    if (v.zoomed) return;
    // 刚刚滑动过（位移 > 8px），不触发点击切换
    if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) return;
    var rect = this.getBoundingClientRect();
    var x = e.clientX - rect.left;
    var isVideo = e.target.tagName === 'VIDEO';
    if (isVideo) {
      // 视频：左右 25% 区域切换，中间 50% 留给播放/暂停和 controls
      if (x < rect.width * 0.25) prev();
      else if (x > rect.width * 0.75) next();
    } else {
      // 图片：左右半区切换
      if (x < rect.width / 2) prev(); else next();
    }
  });

  document.addEventListener('keydown', function (e) {
    if (viewer.classList.contains('hidden')) return;
    if (e.key === 'Escape') {
      // 独立窗口模式：Esc 只重置缩放，不关窗口（避免误关）
      if (singleMode) {
        resetView();
        applyTransform();
        return;
      }
      closeViewer();
    }
    else if (e.key === 'ArrowRight') next();
    else if (e.key === 'ArrowLeft') prev();
  });

  // ---------- Scroll / resize ----------
  var scrollTicking = false;
  scroller.addEventListener('scroll', function () {
    if (!scrollTicking) {
      scrollTicking = true;
      requestAnimationFrame(function () { renderVisible(); scrollTicking = false; });
    }
  }, { passive: true });

  var resizeT = null;
  window.addEventListener('resize', function () {
    clearTimeout(resizeT);
    // keepScroll=true：改窗口大小、移动端地址栏收起/展开都不该把滚动位置弹回顶部
    resizeT = setTimeout(function () { layout(true); }, 150);
  });

  // ---------- Sort / view ----------
  $('sortSel').onchange = function () {
    state.sort = this.value;
    if (state.path) loadDir(state.path);
  };
  $('sizeLarge').onclick = function () {
    if (state.cellMode === 'large') return;
    state.cellMode = 'large';
    try { localStorage.setItem('mediaview_cellMode', 'large'); } catch(e) {}
    this.classList.add('active'); $('sizeSmall').classList.remove('active');
    layout(true);
  };
  $('sizeSmall').onclick = function () {
    if (state.cellMode === 'small') return;
    state.cellMode = 'small';
    try { localStorage.setItem('mediaview_cellMode', 'small'); } catch(e) {}
    this.classList.add('active'); $('sizeLarge').classList.remove('active');
    layout(true);
  };
  // 初始化时按 state.cellMode 同步按钮高亮（从 localStorage 恢复后可能是 small），
  // 不能无条件给 sizeLarge 加 active——否则 cellMode=small 时高亮错误且点击无法纠正。
  if (state.cellMode === 'small') {
    $('sizeSmall').classList.add('active');
    $('sizeLarge').classList.remove('active');
  } else {
    $('sizeLarge').classList.add('active');
    $('sizeSmall').classList.remove('active');
  }

  // ================= SETTINGS =================
  var browsePath = null;

  function loadSettings() {
    return fetchJSON(API + '/settings').then(function (s) {
      applySettings(s);
      return cfg;
    }).catch(function () { return cfg; });
  }

  function applySettings(s) {
    if (!s) return;
    cfg.thumbEnabled = !!s.thumbEnabled;
    cfg.thumbDir = s.thumbDir || '';
    cfg.thumbSize = parseInt(s.thumbSize, 10) || 320;
    cfg.thumbQuality = parseInt(s.thumbQuality, 10) || 80;
    cfg.activeThumbDir = s.activeThumbDir || '';
    cfg.defaultThumbDir = s.defaultThumbDir || '';
    cfg.note = s.note || '';
    cfg.cacheFiles = s.cacheFiles || 0;
    cfg.cacheBytes = s.cacheBytes || 0;
    cfg.settingsFile = s.settingsFile || '';
    cfg.takeoverSystemThumb = !!s.takeoverSystemThumb;
    cfg.systemThumbAvail = !!s.systemThumbAvail;
    cfg.systemThumbActive = !!s.systemThumbActive;
    cfg.cpuCores = parseInt(s.cpuCores, 10) || 0;
    cfg.thumbConcurrency = parseInt(s.thumbConcurrency, 10) || 6;
    cfg.preloadConcurrency = parseInt(s.preloadConcurrency, 10) || 3;
    cfg.gpuDecode = s.gpuDecode !== false; // 默认 true
    cfg.gpuImageDecode = s.gpuImageDecode === true; // 默认 false（实验性）
    cfg.viewerPreload = parseInt(s.viewerPreload, 10);
    if (isNaN(cfg.viewerPreload) || cfg.viewerPreload < 0 || cfg.viewerPreload > 5) cfg.viewerPreload = 2;
    cfg.viewerAnimation = s.viewerAnimation || 'slide';
    cfg.viewerMode = s.viewerMode || 'window';
    applyViewerAnimation();
  }

  // 应用翻页动画效果到查看器容器
  function applyViewerAnimation() {
    var anim = cfg.viewerAnimation || 'slide';
    viewer.classList.remove('anim-none', 'anim-slide', 'anim-fade', 'anim-zoom');
    viewer.classList.add('anim-' + anim);
  }

  function openSettings() {
    $('settings').classList.remove('hidden');
    $('saveHint').textContent = '';
    $('saveHint').classList.remove('err');
    // 表单填好之前禁止保存：此刻 DOM 里全是 HTML 静态默认值（缩略图未勾选、尺寸 160、画质 60），
    // 而后端是**整份替换** —— 手快在 GET 返回前点一次保存，就会把用户配置静默重置掉。
    var btn = $('settingsSave');
    btn.disabled = true;
    fetchJSON(API + '/settings').then(function (s) {
      applySettings(s);
      fillSettingsForm();
      btn.disabled = false;
    }).catch(function () {
      // 读取失败就保持禁用：宁可不让保存，也不能用默认值覆盖用户配置
      $('saveHint').classList.add('err');
      $('saveHint').textContent = '读取设置失败，请关闭后重试';
    });
  }
  function closeSettings() {
    $('settings').classList.add('hidden');
    $('dirBrowser').classList.add('hidden');
  }

  function fillSettingsForm() {
    $('setThumbEnabled').checked = cfg.thumbEnabled;
    $('setThumbDir').value = cfg.thumbDir || '';
    setSelValue('setThumbSize', String(cfg.thumbSize));
    setSelValue('setThumbQuality', String(cfg.thumbQuality));
    $('setTakeover').checked = cfg.takeoverSystemThumb;
    $('setCPUCores').value = cfg.cpuCores || 0;
    $('setConcurrency').value = cfg.thumbConcurrency || 6;
    $('setPreloadConcurrency').value = cfg.preloadConcurrency || 3;
    $('setGPUDecode').checked = cfg.gpuDecode !== false;
    $('setGPUImageDecode').checked = cfg.gpuImageDecode === true;
    $('setViewerPreload').value = cfg.viewerPreload != null ? cfg.viewerPreload : 2;
    setSelValue('setViewerAnimation', cfg.viewerAnimation || 'slide');
    setSelValue('setViewerMode', cfg.viewerMode || 'window');
    // 主题
    var curTheme = 'light';
    try { curTheme = localStorage.getItem('mediaview_theme') || 'light'; } catch (e) {}
    setSelValue('setTheme', curTheme);
    // 系统不存在 auto_thumbnailer 服务时隐藏该行
    $('rowTakeover').style.display = cfg.systemThumbAvail ? '' : 'none';
    var hint = $('takeoverHint');
    if (cfg.systemThumbAvail) {
      hint.textContent = cfg.systemThumbActive
        ? '系统缩略图服务当前：运行中'
        : '系统缩略图服务当前：已停止';
      hint.className = 'dir-hint';
    } else {
      hint.textContent = '';
    }
    syncThumbOpts();
    renderCacheInfo();
  }

  function setSelValue(id, v) {
    var el = $(id);
    var found = false;
    for (var i = 0; i < el.options.length; i++) {
      if (el.options[i].value === v) { el.selectedIndex = i; found = true; break; }
    }
    if (!found && el.options.length) {
      // 配置值不在下拉项里（比如手工改过配置文件）：补一个选项，避免被静默改掉
      var o = document.createElement('option');
      o.value = v; o.textContent = v;
      el.appendChild(o);
      el.value = v;
    }
  }

  // syncThumbOpts 关闭缩略图时把下面的选项收起来（开关本身留可见）
  function syncThumbOpts() {
    var on = $('setThumbEnabled').checked;
    $('thumbOpts').style.display = on ? '' : 'none';
  }

  function renderCacheInfo() {
    var el = $('cacheInfo');
    el.innerHTML = '当前目录：<b>' + escapeHtml(cfg.activeThumbDir || '-') + '</b><br>' +
      '已缓存 ' + cfg.cacheFiles + ' 个文件，占用 ' + fmtB(cfg.cacheBytes);
    var hint = $('cfgPath');
    if (cfg.note) {
      hint.className = 'dir-hint warn';
      hint.textContent = '⚠ ' + cfg.note;
    } else {
      hint.className = 'dir-hint';
      hint.textContent = cfg.settingsFile ? ('配置文件：' + cfg.settingsFile) : '';
    }
    if (!$('setThumbDir').value && cfg.defaultThumbDir) {
      $('dirHint').textContent = '默认目录：' + cfg.defaultThumbDir;
    }
  }

  // ---- directory browser ----
  function openBrowser(startPath) {
    $('dirBrowser').classList.remove('hidden');
    browse(startPath || null);
  }

  function browse(path) {
    var list = $('dbList');
    list.innerHTML = '<div class="db-empty">加载中…</div>';
    var url = API + '/browse' + (path ? '?path=' + enc(path) : '');
    fetchJSON(url).then(function (d) {
      browsePath = d.path;
      $('dbPath').textContent = d.path;
      $('dbPath').title = d.path;
      $('dbUp').disabled = !d.parent;
      $('dbUp').onclick = function () { if (d.parent) browse(d.parent); };
      list.innerHTML = '';
      if (!d.dirs || !d.dirs.length) {
        list.innerHTML = '<div class="db-empty">该目录下没有子文件夹</div>';
        return;
      }
      d.dirs.forEach(function (x) {
        var row = document.createElement('div');
        row.className = 'db-item';
        row.innerHTML = '<span class="db-ico">📁</span>' +
          '<span class="db-name"></span><span class="db-go">进入 ›</span>';
        row.querySelector('.db-name').textContent = x.name;
        row.querySelector('.db-name').title = x.path;
        row.onclick = function () { browse(x.path); };
        list.appendChild(row);
      });
    }).catch(function (e) {
      list.innerHTML = '<div class="db-empty">无法读取：' + escapeHtml(e.message || '') + '</div>';
    });
  }

  // ---- save ----
  function saveSettings() {
    var btn = $('settingsSave');
    btn.disabled = true;
    $('saveHint').classList.remove('err');
    $('saveHint').textContent = '保存中…';

    var body = {
      thumbEnabled: $('setThumbEnabled').checked,
      thumbDir: $('setThumbDir').value.trim(),
      thumbSize: parseInt($('setThumbSize').value, 10) || 320,
      thumbQuality: parseInt($('setThumbQuality').value, 10) || 80,
      takeoverSystemThumb: $('setTakeover').checked,
      cpuCores: parseInt($('setCPUCores').value, 10) || 0,
      thumbConcurrency: parseInt($('setConcurrency').value, 10) || 6,
      preloadConcurrency: parseInt($('setPreloadConcurrency').value, 10) || 3,
      gpuDecode: $('setGPUDecode').checked,
      gpuImageDecode: $('setGPUImageDecode').checked,
      viewerPreload: parseInt($('setViewerPreload').value, 10) || 0,
      viewerAnimation: $('setViewerAnimation').value || 'slide',
      viewerMode: $('setViewerMode').value || 'window'
    };

    fetch(API + '/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error((t || '').trim()); });
      return r.json();
    }).then(function (res) {
      btn.disabled = false;
      applySettings(res.settings);
      fillSettingsForm();
      $('saveHint').textContent = '已保存';
      closeSettings();
      toast('设置已保存');
      // 画质/缩略图目录/尺寸都可能变了：换一批 URL，避免浏览器沿用旧缩略图
      bumpThumbCacheBust();
      // 目录或缩略图参数变化 → URL 可能已变，统一重绘当前视图
      redraw();
    }).catch(function (e) {
      btn.disabled = false;
      $('saveHint').classList.add('err');
      $('saveHint').textContent = e.message || '保存失败';
      toast('保存失败：' + (e.message || ''));
    });  }

  // redraw 重新渲染当前视图（缩略图参数/开关变化后调用）
  function redraw() {
    for (var k in cellPool) {
      var el = cellPool[k];
      el.parentNode && el.parentNode.removeChild(el);
      delete cellPool[k];
    }
    requestRender();
  }

  // ---------- 主题切换 ----------
  function applyTheme(theme) {
    if (theme === 'light') {
      document.documentElement.setAttribute('data-theme', 'light');
    } else {
      document.documentElement.removeAttribute('data-theme');
    }
    try { localStorage.setItem('mediaview_theme', theme); } catch (e) {}
  }
  function initTheme() {
    var t = 'light';
    try { t = localStorage.getItem('mediaview_theme') || 'light'; } catch (e) {}
    applyTheme(t);
  }
  initTheme();

  $('btnSettings').onclick = openSettings;
  $('btnRebuildThumbs').onclick = function () {
    if (!state.path) { toast('请先进入一个目录'); return; }
    if (!confirm('重建当前目录的缩略图缓存？\n\n' + state.path)) return;
    fetch(API + '/thumb/clear?path=' + encodeURIComponent(state.path), { method: 'POST' })
      .then(function (r) {
        return r.text().then(function (t) {
          try { return JSON.parse(t); } catch (e) { return { ok: false, error: '非JSON响应: ' + t.substring(0, 200), status: r.status }; }
        });
      })
      .then(function (d) {
        if (d.ok) {
          toast('已清除，正在重新生成…');
          bumpThumbCacheBust();
          setTimeout(function () { loadDir(state.path); }, 300);
        } else {
          toast('清除失败: ' + (d.error || d.status || 'unknown'));
        }
      })
      .catch(function (e) { toast('清除失败: ' + e.message); });
  };
  $('settingsClose').onclick = closeSettings;
  $('settingsCancel').onclick = closeSettings;
  $('settingsMask').onclick = closeSettings;
  $('settingsSave').onclick = saveSettings;
  $('setThumbEnabled').onchange = syncThumbOpts;
  $('setTheme').onchange = function () {
    applyTheme(this.value);
  };
  $('btnBrowse').onclick = function () {
    if (!$('dirBrowser').classList.contains('hidden')) {
      $('dirBrowser').classList.add('hidden');
      return;
    }
    // 起点策略：输入框里已有路径 → 从那里开始；
    // 否则从「存储空间聚合层」开始（而不是当前的缩略图目录）——
    // 当前目录多半是深度路径，从那儿出发根本挑不到别的位置。
    openBrowser($('setThumbDir').value.trim() || null);
  };
  $('btnUseDefault').onclick = function () {
    $('setThumbDir').value = '';
    $('dirHint').textContent = '将使用默认目录：' + (cfg.defaultThumbDir || '');
  };
  $('dbPick').onclick = function () {
    if (browsePath) {
      $('setThumbDir').value = browsePath;
      $('dirHint').textContent = '已选择：' + browsePath;
      $('dirBrowser').classList.add('hidden');
    }
  };
  $('btnPurge').onclick = function () {
    var b = this;
    b.disabled = true;
    fetch(API + '/settings/purge', { method: 'POST' }).then(function (r) {
      return r.json();
    }).then(function (d) {
      b.disabled = false;
      toast('已清空 ' + (d.removed || 0) + ' 个缓存文件');
      bumpThumbCacheBust(); // 换 URL，避免浏览器继续用旧缩略图
      return loadSettings();
    }).then(function () {
      fillSettingsForm();
      redraw();
    }).catch(function () {
      b.disabled = false;
      toast('清空缓存失败');
    });
  };

  // 清除所有数据：缩略图缓存 + 设置，恢复出厂默认（卸载前可用）
  $('btnFactoryReset').onclick = function () {
    if (!confirm('确定要清除所有数据吗？\n\n将删除：全部缩略图缓存、所有设置项\n并恢复系统缩略图服务。\n\n此操作不可撤销！')) return;
    var b = this;
    b.disabled = true;
    fetch(API + '/settings/factory_reset', { method: 'POST' }).then(function (r) {
      return r.json();
    }).then(function (d) {
      b.disabled = false;
      toast('已清除所有数据，设置已恢复默认');
      bumpThumbCacheBust();
      return loadSettings();
    }).then(function () {
      fillSettingsForm();
      redraw();
    }).catch(function () {
      b.disabled = false;
      toast('清除失败');
    });
  };

  // ---------- Boot ----------
  var singleMode = false;
  function boot() {
    var qs = new URLSearchParams(location.search);
    var single = qs.get('path');
    if (single) {
      singleMode = true;
      // 单文件模式（独立窗口）：立即隐藏工具栏和网格，只显示查看器，避免闪烁
      $('topbar').style.display = 'none';
      $('main').style.display = 'none';
      var sortParam = qs.get('sort') || 'name';
      settingsReady = loadSettings();
      // 首图不依赖 settings：图片 URL 只用到 pickMaxdim()，与 settings 无关。
      // 不等 settings 可以省 1 个 RTT（局域网 5~30ms，经网关/宿主代理时可能更多）。
      // settings 到达后 applySettings() 会自动补齐动画效果；viewerPreload 的默认值
      // 与大多数用户设置一致，晚到几百 ms 不影响首图体验。
      openSingle(single, sortParam);
      return;
    }
    var last = null;
    try { last = localStorage.getItem('mediaview_last'); } catch (e) {}
    settingsReady = loadSettings();
    settingsReady.then(function () {
      if (last) {
        fetchJSON(API + '/list?path=' + enc(last)).then(function () { loadDir(last); })
          .catch(buildRoot);
      } else {
        buildRoot();
      }
    });
  }
  boot();
})();
