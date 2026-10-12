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
  // A7：带原图版本号（mtime）→ 服务端才能用长缓存。
  // 没有版本号时只能用 no-cache（否则覆盖同名文件后浏览器 24h 内仍取旧图）；
  // 有了它，翻页回到同一张图可以完全走浏览器缓存（0 请求、0 解码）。
  function rawURL(p, maxdim, mtime) {
    // maxdim 是"这张图最终要被显示到多大"的预算（见 pickMaxdim），后端据此用
    // ffmpeg lanczos 降采样 —— 质量远好于把上万像素的原图丢给浏览器自己缩。
    // 不带 maxdim 只在"要原文件"时才用（目前只有大图升级原图那一处）。
    var url = API + '/raw?path=' + enc(p);
    if (maxdim) url += '&maxdim=' + maxdim;
    url += '&v=' + (mtime || 0);
    // 参与产物内容的设置项也要进 URL：后端在带 ?v= 时下发
    // `Cache-Control: immutable, max-age=1年`，若 URL 不变，用户改了
    // 「大图画质 / lowres 档位」后浏览器根本不会重取，改了等于没改。
    url += '&s=' + rawSettingsTag();
    return url;
  }
  // 影响 /api/raw 产物的设置指纹（值变了 URL 就变 → 浏览器重新取）
  function rawSettingsTag() {
    return (cfg.viewerQuality | 0) + '.' + (cfg.viewerLowres | 0) + '.' + (cfg.thumbQuality | 0);
  }
  function enc(s) { return encodeURIComponent(s); }

  function metaURL(p) { return API + '/meta?path=' + enc(p); }

  // 预取「原图尺寸」——这是 1:1 能否一次算到位的前提。
  //
  // 为什么需要它：1:1 要的 scale = 原图像素宽度 / 布局宽度 / DPR。
  //   · item.w 来自列表 JSON，但它是 `json:"w,omitempty"` —— 尺寸为 0 时**整个字段不出现**，
  //     所以经常拿不到；
  //   · 退而用 v.media.naturalWidth，那一刻它只是**预览档**（3072）的宽度，
  //     于是 scale 按预览图算 → **第一次点 1:1 放大到的不是原图尺寸**，
  //     等原图进来再重算一次 → 视觉上的「二次放大」。
  // 所以在**打开/切换图片时**就把尺寸问回来（后端只读文件头，很便宜），
  // 用户看图这几秒足够返回，点 1:1 时 item.w 已经在了 → 一次算到位、锁死、不再重算。
  function ensureItemSize(item) {
    if (!item || item.w || item._szPending) return;
    item._szPending = true;
    fetch(metaURL(item.path)).then(function (r) { return r.json(); }).then(function (mm) {
      var w = mm && (mm.w || mm.width);
      if (w) {
        item.w = w;
        item.h = mm.h || mm.height || 0;
        // 若此刻正停在 1:1 且 scale 还没锁死（说明点的时候没尺寸），立刻按真实宽度修正一次
        var m2 = v.media;
        if (v.actual && m2 && m2.tagName === 'IMG' && !v._actualScaleLocked) {
          v._actualScaleLocked = true;
          applyActualScale(item.w);
          applyTransform();
        }
      }
    }).catch(function () {}).then(function () { item._szPending = false; });
  }

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
  var PICK_STEP = 512; // A3：256→512。档位越少，缓存复用率越高 → 越少"换尺寸就重解码"
  // （成本几乎全在完整解码，实测 1.83/1.91/2.06s 三个档位几乎是同一条水平线）
  function pickMaxdim() {
    // 1.8.144：设置里指定了固定清晰度档 → 直接用它（不再按视口算）
    var fixed = cfg.viewerMaxdim || 0;
    if (fixed > 0) return fixed;
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
    // 注意：cfg 是本模块顶层的「默认值缓存」，在页面元素存在**之前**就求值 ——
    // 这里绝不能写 $('...').value，否则 $() 返回 null、.value 抛 TypeError，
    // 整个脚本崩在这里、界面白屏（1.8.129 就是这样坏的）。
    thumbEngine: 'auto',

    // 1.8.144：大图预览的画质 / 清晰度 / lowres 档位（0 = 自动）

    viewerQuality: 0, // 0=自动88, -1=跟随缩略图质量, 84/88/95=固定

    viewerMaxdim: 2048, // 1.8.146 起默认 2048（0=自动按视口）

    viewerLowres: 0,  // 0=自动, -1=关闭, 1/2/3=固定

    thumbLowresLevel: 0, // 同上（缩略图）
    thumbLowres: true,
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
    // ⚠ 这两个默认值只是"后端设置还没加载回来"那一瞬的占位，**真值以后端为准**：
    //   后端 thumbConcurrency 默认按 CPU 核数算，preloadConcurrency 默认 2。
    //   这里若写成与后端相同的数字，反而会掩盖"后端改默认值、前端忘了跟"的问题，
    //   所以保持"占位"语义并写明（历史上曾因这两处不一致而误排查过）。
    thumbConcurrency: 3,
    preloadConcurrency: 0,
    gpuDecode: true,
    viewerPreload: 2,
    viewerAnimation: 'slide',
    viewerMode: 'window', // window=独立桌面窗口 / overlay=窗口内遮罩
  };
  var settingsReady = null; // Promise

  // 后台预加载并发允许 0（= 关闭后台预生成）。这里绝不能用 `|| 默认值` 兜底 ——
  // 0 是 falsy，会被静默改成非 0，用户设的「关闭」既存不上也回填不出来。
  function numOr(v, dflt) {
    var n = parseInt(v, 10);
    return isNaN(n) ? dflt : n;
  }

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
    // 持久化：从文件管理器打开每张图都是一个**全新的 iframe 实例**，
    // 排序只存在内存里的话，用户在某个窗口里设好的排序换个窗口就丢了 ——
    // 表现就是「设好之后能对上，一换图/一切换又乱了」。
    // 注意必须补方向：历史版本可能存下过不带方向的裸字段（如 "name"），
    // 而后端 parseSort 对裸字段只给默认方向 —— 于是升/降都变成同一个方向。
    sort: (function () {
      try {
        var saved = localStorage.getItem('mediaview_sort') || 'mtim:desc';
        if (saved.indexOf(':') >= 0) return saved;
        // 与后端 parseSort 的规则一致：修改时间/大小默认倒序，其余默认升序
        var f = String(saved).toLowerCase();
        return f + ((f === 'mtime' || f === 'mtim' || f === 'size' || f === 'date') ? ':desc' : ':asc');
      } catch (e) { return 'mtim:desc'; }
    })(),
    sortDesc: true,      // 当前排序方向（仅 UI 显示用，与 sort 里的 :dir 保持同步）
    sortManual: false,   // 用户是否手动改过排序（改过后本次会话不再自动跟随文件管理器）
    cellMode: (function(){ try { return localStorage.getItem('mediaview_cellMode') || 'large'; } catch(e){ return 'large'; } })(), // large | small
    root: false,
  };
  var cellPool = {};   // data-idx -> element
  var rafPending = false;

  // ================= 输入能力分档（INPUT PROFILE）=================
  // 目的：手机上的手势增强**只对触屏生效**，桌面（鼠标）路径一行行为都不变。
  //
  // 判据用【能力探测】，不是 UA，也不是窗口宽度：
  //   · (pointer: coarse)  —— 主输入设备是触摸
  //   · (hover: none)      —— 没有悬停能力
  //   · maxTouchPoints > 0 —— 确实有触点
  // 三项同时成立才判触屏。用户在手机浏览器里开「请求桌面版网站」也照样命中
  // （物理输入就是手指）—— 这一点比 UA 判断可靠得多，UA 会被那个开关改掉。
  // 桌面（鼠标）三项都不命中 ⇒ PROFILE === 'mouse' ⇒ 下面所有触屏分支短路。
  var INPUT = (function () {
    function mq(q) {
      try { return !!(window.matchMedia && window.matchMedia(q).matches); } catch (e) { return false; }
    }
    var touch = mq('(pointer: coarse)') && mq('(hover: none)') &&
      (navigator.maxTouchPoints || 0) > 0;
    return { touchPrimary: touch, profile: touch ? 'touch' : 'mouse' };
  })();
  var PROFILE = INPUT.profile;

  // 触屏专属手势模块**按需注入**。
  // ⚠️ 必须是动态注入，不能在 index.html 里写静态 `<script src="touch.js">` ——
  //    静态标签会让桌面也下载并执行这一整块手势代码，那就"搅和"了。
  //    （zzz_2100 有一条测试专门禁止静态写法，防止以后被"顺手改回去"。）
  if (PROFILE === 'touch') {
    var touchScript = document.createElement('script');
    touchScript.src = 'touch.js?v=__MV_VERSION__';
    touchScript.defer = true;
    document.head.appendChild(touchScript);
  }

  // ---------- Sort helpers ----------
  // 排序统一用 'field:dir' 两段式字符串（如 'mtime:desc'），后端 parseSort 认这种
  // 格式；方向随字符串一起走，让列表顺序、大图切换顺序、预生成顺序共用同一来源。
  // 内部字段名统一为后端字段名：name / mtime / size / type / btime。
  function sortParts(s) {
    var raw = String(s || '').toLowerCase().trim();
    var field = 'mtime', desc = true;
    if (!raw) return { field: field, desc: desc };
    var i = raw.indexOf(':');
    var f = i >= 0 ? raw.slice(0, i) : raw;
    var d = i >= 0 ? raw.slice(i + 1) : '';
    switch (f) {
      case 'name': field = 'name'; break;
      case 'size': field = 'size'; break;
      case 'type': field = 'type'; break;
      case 'btim': case 'btime': case 'ctime': case 'created': field = 'btime'; break;
      case 'mtime': case 'mtim': case 'date': case 'modified': field = 'mtime'; break;
      default: field = 'mtime';
    }
    if (d === 'asc') desc = false;
    else if (d === 'desc') desc = true;
    else {
      // 只给了字段、没给方向：沿用旧语义的默认方向（date/size 默认倒序）。
      desc = (field === 'mtime' || field === 'size');
    }
    return { field: field, desc: desc };
  }
  function sortStr(field, desc) { return field + (desc ? ':desc' : ':asc'); }

  // ── 1.8.190/1.8.191：按飞牛规则 + **数字按值**重排 ──────────────────────
  //
  // 飞牛的排序（它后端 server.cjs 的 sortEntries）对**名称**用的是
  //     a.name.localeCompare(b.name, "zh")
  // ICU/CLDR 的字符层级实测为：
  //     标点(空格 _ - , ! . ( { @ & # % + = ~ $) < 数字 < **汉字（按拼音）**
  //       < 小写 < 大写 < 全角 < 希腊 < 西里尔 < 韩文 < 日文假名
  // 而后端的 naturalLess 是**字节序**，会把**所有汉字排到最后** —— 实测：
  //     NO.072（76 个）后端 ['DSC06619.jpg', …]（汉字在第 76 位）
  //                    飞牛 ['小吉的快乐野餐封面.jpg', 'DSC06619.jpg', …]（第 1 位）
  // 这就是"有中文和字母时按名称排序不对"的原因。
  //
  // 为什么在前端做：Go 的 golang.org/x/text/collate(zh) 与 ICU **不一致** ——
  //     Go  : "A" vs "中" → A 在前（拉丁 < 汉字）
  //     Node: "A" vs "中" → 中 在前（汉字 < 拉丁）
  // 汉字拼音序两边相同（阿<波<国<玛<小<中），但汉字与拉丁的先后相反；
  // ICU 层级细到标点 16 类、全角/希腊/西里尔/韩文/假名各有位置，手写复刻不现实。
  // 浏览器的 Intl.Collator("zh") 与飞牛后端同源（都是 ICU/CLDR），所以在前端重排。
  //
  // 1.8.191：数字段按**数值**排（numeric: true）—— 用户明确要求，见上。
  // 只对 name / type 重排：飞牛对这两项用 localeCompare，
  // size / mtime / birthtime 用数值比较（那些后端已经做对了）。
  var _zhCollator = null;
  function zhCollator() {
    if (_zhCollator === null) {
      try {
        // 1.8.191：加 numeric: true —— 数字段按**数值**比较。
        // 飞牛后端用的是 a.name.localeCompare(b.name, "zh")（**无** numeric），
        // 于是排成 "1 (1) 1 (10) 1 (100) … 1 (2)"（字符串序）。用户明确要求
        // 数字按值排（"1 (1) 1 (2) … 1 (10)"、"1 2 3 … 10 11 100"），
        // 因此这里在 zh 规则之上加 numeric。
        _zhCollator = (typeof Intl !== 'undefined' && Intl.Collator)
          ? new Intl.Collator('zh', { numeric: true }) : false;
      } catch (e) { _zhCollator = false; }
    }
    return _zhCollator || null;
  }

  // 按飞牛规则重排一份列表（元素需带 name 字段）。sort 形如 'name:asc'/'type:desc'。
  // 同键时保持传入顺序（后端给的就是飞牛在字段相等时用的物理顺序）。
  function resortLikeFnOs(list, sort) {
    if (!list || list.length < 2) return list;
    var p = sortParts(sort || '');
    if (p.field !== 'name' && p.field !== 'type') return list;
    var col = zhCollator();
    if (!col) return list; // 环境不支持 Intl → 保留后端顺序，别乱动
    var sign = p.desc ? -1 : 1;
    var keyOf;
    if (p.field === 'type') {
      keyOf = function (x) {
        var n = String((x && x.name) || '');
        var i = n.lastIndexOf('.');
        return i > 0 ? n.slice(i + 1).toLowerCase() : '';
      };
    } else {
      keyOf = function (x) { return String((x && x.name) || ''); };
    }
    var idx = new Array(list.length);
    for (var i = 0; i < list.length; i++) idx[i] = i;
    idx.sort(function (a, b) {
      var c = col.compare(keyOf(list[a]), keyOf(list[b]));
      if (c !== 0) return c * sign;
      return a - b; // 稳定：同键保持后端给的物理顺序
    });
    var out = new Array(list.length);
    for (var k = 0; k < list.length; k++) out[k] = list[idx[k]];
    return out;
  }
  function normalizeSortParam(s) {
    var p = sortParts(s);
    return sortStr(p.field, p.desc);
  }
  // 应用排序到 state 并同步 UI 控件。manual=true 表示用户手动改的。
  function applySort(s, manual) {
    var p = sortParts(s);
    state.sort = sortStr(p.field, p.desc);
    state.sortDesc = p.desc;
    if (manual) {
      // 手动选排序直接写 state.sort，并立即对网格与大图生效（1.8.178 起没有开关了）。
      state.sortManual = true;
    }
    // 记住排序：下次从文件管理器打开（新 iframe 实例）仍用它，否则会退回默认值
    try { localStorage.setItem('mediaview_sort', state.sort); } catch (e) {}
    // 下拉框里每个字段只有一个选项，方向由箭头按钮单独控制 —— 所以这里按**字段**匹配，
    // 并把该选项的 value 更新成当前 "字段:方向"。旧实现按完整 value（含方向）精确匹配，
    // 于是「点一下降序箭头」会因 name:asc != name:desc 匹配失败，凭空多出一个"自定义"。
    var sel = $('sortSel'), found = false;
    for (var i = 0; i < sel.options.length; i++) {
      var opt = sel.options[i];
      if (opt.textContent === '自定义') continue; // 跳过历史遗留项，别被它匹配走
      if (sortParts(opt.value).field === p.field) {
        opt.value = state.sort;
        sel.selectedIndex = i; found = true; break;
      }
    }
    if (!found) {
      // 确实没有该字段的选项（例如 URL 指定了未知字段）才新增；复用已有的自定义项避免堆积
      var o = null;
      for (var j = 0; j < sel.options.length; j++) {
        if (sel.options[j].textContent === '自定义') { o = sel.options[j]; break; }
      }
      if (!o) {
        o = document.createElement('option');
        o.textContent = '自定义';
        sel.appendChild(o);
      }
      o.value = state.sort;
      sel.value = state.sort;
    }
    $('sortDirBtn').textContent = p.desc ? '↓' : '↑';
    $('sortDirBtn').title = p.desc ? '当前降序，点击切换为升序' : '当前升序，点击切换为降序';
  }
  // 解析某目录该用哪个排序：跟随开关开 → 一律读文件管理器偏好。
  //
  // 关键：**不要**再用 state.sortManual 挡住。它会让「点过一次排序按钮」永久
  // 关掉跟随 —— 网格（loadDir 走非 force 分支）从此再也不读文件管理器排序，
  // 表现就是「网格排序没有照搬飞牛」。想用自己的排序请关掉设置里的跟随开关
  // （手动点排序按钮时会自动关，见 applySort）。
  //
  // force=true：从文件管理器打开单张图时用；读不到偏好时兜底用文件管理器默认
  // （名称升序），而不是本应用本地可能过时的 state.sort。

  // resolveSortForDir —— 决定"这个目录用哪个排序"。
  //
  // 【网格与大图**刻意分开**，别再合并】：
  //   · 网格（force 为空）  → **永远**用 state.sort（用户点右上角排序按钮选的）。
  //       1.8.181~1.8.182 这里**也**去读了飞牛偏好，于是只要 IndexedDB 里该目录有记录，
  //       就把用户刚选的值覆盖掉 —— 用户看到的正是"排序按钮全部失效"。
  //       1.8.165 之所以没这问题：那时那份 IndexedDB 读不到（getDirSort 恒 null），
  //       永远走回退；而现在飞牛确实在写它，就暴露了。
  //   · 大图（force=true，从文件管理器双击打开）→ 读飞牛写下的偏好，
  //       读不到才回退 name:asc（资源管理器默认视图）。
  //
  // 关于那份偏好（结论·别再回头）：
  //   它在**前端 IndexedDB**（trim-file-manager / file-view-preferences，
  //   keyPath [userId,scope,surface,location]，字段 sortField/sortType）。
  //   飞牛**没有**把目录排序写进它的 SQLite（folder_views 只在改视图模式/图标大小时
  //   被顺带写一次），所以"后端读它的 SQLite"和"连它的 WebSocket"两条路都是死的。
  function resolveSortForDir(path, force) {
    if (!force) return Promise.resolve(state.sort);
    var fb = 'name:asc';
    if (!window.FnOsSort || !window.FnOsSort.getDirSort) return Promise.resolve(fb);
    return window.FnOsSort.getDirSort(path).then(function (s) {
      return s ? normalizeSortParam(s) : fb;
    }).catch(function () { return fb; });
  }

  // 1.8.175：把 AbortError 的未处理 rejection 吞掉。
  //
  // 背景：悬停预取用 AbortController 主动取消在飞的 fetch（避免快速划过一排格子时
  // 留下一串无用的转码）。我们那里的 .catch() 已经吞掉了它，但页面上**注入的
  // inspector.js 包装了 window.fetch**，它在请求被 abort 时会自己打一条
  //   "Fetch request failed: AbortError: signal is aborted without reason"
  // 并指向发起 abort 的那一行（app.js 的 ctl.abort()）。
  // 那是"主动取消"，不是错误 —— 消音即可，不改任何业务逻辑。
  window.addEventListener('unhandledrejection', function (ev) {
    var r = ev && ev.reason;
    if (!r) return;
    var name = String(r.name || '');
    var msg = String(r.message || r || '');
    if (name === 'AbortError' || /abort/i.test(msg)) {
      try { ev.preventDefault(); } catch (e) {}
    }
  });

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

  // 2.0.0：把**按显示顺序重排之后**的有序文件列表回传后端，让后台预生成按
  // 「用户实际看到的顺序」入队。
  //
  // 取代了原先挂在 /api/list 上的自动预生成 —— 那个顺序是后端**字节序**（汉字排最后），
  // 与前端 ICU 显示顺序不一致（实测「X:\测试排序」129 个文件里 121 个位置不同），
  // 等于把绝大部分算力花在用户**最后**才会看到的文件上，中文目录首屏反而更慢。
  //
  // 上限 512 与后端队列容量一致（thumb.go: make(chan thumbReq, 512)），再多也装不下。
  // 返回 Promise（手动「立即生成」用它报入队数量）；自动路径可忽略返回值。
  function postPregen(files, manual) {
    if (!files || !files.length) return Promise.resolve(null);
    var paths = [];
    for (var i = 0; i < files.length && paths.length < 512; i++) {
      if (files[i] && files[i].path) paths.push(files[i].path);
    }
    if (!paths.length) return Promise.resolve(null);
    try {
      return fetch(API + '/pregen', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        // &preload=50
        body: JSON.stringify({ paths: paths, manual: !!manual })
      }).then(function (r) { return r.ok ? r.json() : null; })
        .catch(function () { return null; });
    } catch (e) { return Promise.resolve(null); }
  }
  function fmtB(n) {
    if (!n) return '0 B';
    if (n >= 1073741824) return (n / 1073741824).toFixed(2) + ' GB';
    if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
    if (n >= 1024) return (n / 1024).toFixed(0) + ' KB';
    return n + ' B';
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
        // 2.4.0：首页 = **聚合文件夹层**（所有存储空间用户目录下的文件夹合并成一层）。
    //   原来是 `/api/volumes`（列出 /vol1、/vol2 这些盘位），用户要先点进盘位才看到自己的文件夹；
    //   飞牛文件管理器不是这样，它直接列内容。现在两边一致。
    //   `?path=/` 是后端约定的"聚合根"，返回的每条 dirs 都带**真实绝对路径**，
    //   所以点进去走的还是原来的目录逻辑，前端无需额外状态。
    fetchJSON(API + '/list?path=' + enc('/') + '&sort=' + enc(state.sort)).then(function (d) {
      loadingEl.classList.add('hidden');
      // ⚠️ 必须**转成 {kind:'dir'}**：后端返回的是 DirEntry（只有 name/path/size/mtime），
      //   而渲染靠 `item.kind === 'dir'` 决定"画文件夹图标"还是"去取缩略图"。
      //   少了这一步，首页的文件夹会被当成文件渲染（格子空白、还白跑缩略图请求）。
      //   loadDir 里一直有这一步，首页聚合时漏了 —— 真 Edge 实测抓出来的。
      state.items = ((d && d.dirs) || []).map(function (x) {
        return { kind: 'dir', name: x.name, path: x.path };
      });
      state.files = [];
      renderCrumb();
      layout();
      syncExitTips();            // 2.4.0：到根层 ⇒ 挂上"是否退出"的确认
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
  // ── 2.3.1：触屏档的"换目录"走**真 URL 导航**（从 2.1.12 移植） ──────────────
  //
  // 为什么（2.1.x 线的实测结论）：
  //   · 宿主的**页面栈只有一层** —— 它看不见我们内部的目录切换，
  //     也**不感知我们的 pushState**（2.1.9-probe：hist 涨到 11，手感毫无改善）；
  //   · 于是网格里"回上一层"在宿主看来就是"离开应用" ⇒ 容易直接退出；
  //   · 让"进目录"变成**真的页面导航**，宿主才会跟着记账，返回手势就变成"退回上一页"。
  //
  // ⚠️ 参数名必须是 `dir`：`?path=` 已被"单文件模式"占用
  //    （boot 里 var single = qs.get('path') 会把目录当成文件去 openSingle）。
  // ⚠️ 代价：进目录会重新加载页面（约 0.5~2 秒）。所以**只对触屏档**启用；
  //    桌面档（PROFILE==='mouse'）保持原来的内部切换，一点变化都没有。
  function navigateTo(path) {
    if (PROFILE === 'touch') {
      try {
        // 2.4.1：用 **replace** 而不是 href —— href 每进一层都往历史里压一条真实 URL，
        //   而守卫条目删不掉已有条目，于是"退到聚合根后继续返回"会恢复旧的
        //   /vol{n}/1000/xxx 整页（用户实测："到聚合根还应该停止，现在还能退回去"）。
        //   replace 替换当前条目 ⇒ 整个会话始终只有「1 条页面 + 1 条守卫」，
        //   返回完全由守卫 + mvOnPop 接管，历史不再累积。
        location.replace(GW + '/?dir=' + encodeURIComponent(path));
        return;
      } catch (e) { /* 极端环境导航失败 ⇒ 退化成内部切换，不能卡住用户 */ }
    }
    loadDir(path);
  }

  // 回"存储空间列表"（最上层）。必须显式带 ?dir=/，
  // 否则 boot 会走 last 分支、从 localStorage 跳回上次访问的目录。
  function navigateRoot() {
    if (PROFILE === 'touch') {
      try {
        location.replace(GW + '/?dir=/');   // 2.4.1：同 navigateTo，不压历史
        return;
      } catch (e) {}
    }
    buildRoot();
  }

  function loadDir(path) {
    // 先释放浏览器连接池再发 /api/list：网格里在飞的缩略图会占满同源 6 条连接，
    // 不取消的话这个 list 请求要排队等好几个缩略图生成周期（详见函数注释）。
    cancelPendingGridRequests();
    state.root = false;
    state.path = path;
    syncExitTips();   // 2.4.2：离开聚合根 ⇒ 撤掉宿主的退出确认（此后返回是「退一层」）
    loadingEl.classList.remove('hidden');
    emptyEl.classList.add('hidden');
    var mySeq = ++loadSeq;
    // 先确定排序（可能要去读文件管理器的偏好），再发列表请求：
    // 列表顺序决定了大图切换顺序、大图预加载取「后面 N 张」、缩略图预生成取「前 N 张」，
    // 顺序错了这三样全跟着错。
    var usedSort = null;
    resolveSortForDir(path).then(function (sortStrParam) {
      if (mySeq !== loadSeq) return;
      usedSort = sortStrParam;
      // 记下「网格实际用的排序」：窗口内点开图片时（openViewerWindow）必须复用它，
      // 否则那边会退到 state.sort —— 两边取的排序来源不同（一个读文件管理器偏好、
      // 一个用本地值），顺序就会不一致：网格第 1 张在大图里变成第 10 张。
      state.currentSort = usedSort;
      return fetchJSON(API + '/list?path=' + enc(path) + '&sort=' + enc(sortStrParam));
    }).then(function (d) {
      if (mySeq !== loadSeq) return; // 已有更新的目录请求，丢弃这次结果
      loadingEl.classList.add('hidden');
      var dirs = d.dirs.map(function (x) { return { kind: 'dir', name: x.name, path: x.path }; });
      var files = d.files;
      // 1.8.190：按飞牛规则重排（后端是字节序，会把汉字排到最后）。
      // 必须用**本次实际请求的排序**（usedSort 带方向），否则降序会被排成升序。
      var effSort = usedSort || (d && d.sort) || state.sort;
      dirs = resortLikeFnOs(dirs, effSort);
      files = resortLikeFnOs(files, effSort);
      state.items = dirs.concat(files);
      state.files = files;
      renderCrumb();
      layout();
      // 2.0.0：用**重排后**的顺序驱动后台预生成（预生成顺序 == 显示顺序）。
      // 关闭「后台预生成」时不发请求（后端也会兜底返回 0）。
      if ((cfg.preloadConcurrency || 0) > 0) postPregen(state.files);
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

  // ---------- 返回上一级（触屏档位的返回按钮用）----------
  // 手机上飞牛桌面没有可见的返回入口，系统边缘手势又只会退出**整个应用** ——
  // 所以给一个页面内的、语义确定的入口：左上角箭头。
  //
  // 语义：还在子目录 → 回上级；**已在第一级（存储空间列表）→ 什么也不做**。
  //
  // ⚠️ 第一级为什么是"**无响应**"（用户明确要求，2.2.1 起）：
  //    第一级是最外面一层，再退就是**离开应用**。用户要的是"在应用里怎么点都不出去"，
  //    所以这里既**不退出**、也**不弹任何确认框** —— 纯空操作。
  //    （2.2.0 曾在第一级弹「退出媒体浏览？」的确认框，2.2.1 按用户要求撤掉。）
  //    要退出应用请用飞牛窗口自带的关闭入口 —— 那是宿主原生的，不经过网页。
  function goUpLevel() {
    // 2.4.2：聚合根层**什么都不做** —— "是否退出"交给**宿主原生弹窗**
    //   （syncExitTips 只在聚合根层挂 setExitPageTips）。
    //   系统返回手势是宿主原生行为，网页拿不到回调；自己画框反而会和宿主那个重复。
    if (state.root || !state.path) return;
    var up = dirName(state.path);
    // 2.3.1：触屏档走真导航（宿主记账 ⇒ 返回能逐层退）
    // 2.4.1：**到聚合根就停**。原来 `dirName('/vol1/1000')` 会得到 /vol1、再得到 /，
    //   于是能一路退到"存储空间"那一层（用户实测："到聚合文件夹目录就应该停止"）。
    //   现在判据换成"上一层是否还在某个存储空间用户目录之内"：出了就不再往上，
    //   直接回聚合根，且**不再产生中间页面**。
    if (!up || up === '/') { navigateRoot(); return; }
    if (!isInsideAnyUserDir(up)) { navigateRoot(); return; }
    navigateTo(up);
  }

  // 2.4.1：`/vol{n}/1000` 及其子目录才算"用户目录之内"。
  //   再往上（/vol1、/vol2、/）就是"存储空间"那一层 —— 用户要求到聚合根就停止，不再显示它。
  function isInsideAnyUserDir(p) {
    if (!p) return false;
    var seg = String(p).split('/').filter(function (s) { return s.length > 0; });
    if (seg.length < 2) return false;              // 至少要是 /vol{n}/1000 这一级
    if (!/^vol\d+$/.test(seg[0])) return false;   // 第一段必须是 volN
    return seg[1] === '1000';                      // 第二段必须是用户目录
  }

  // ---------- 单图 / 独立看图窗口模式 ----------
  // URL 带 ?path= 就是（飞牛给「看图子窗口」的 URL 就长这样）。
  //
  // ⚠️ 必须在**这里**（顶层，早于下面守卫接线）就定下来，不能等 boot()：
  //    守卫压不压要按它决定 —— 单图窗口本身就是"一层"，返回的正确语义是**关掉它**
  //（宿主在 canGoBack 为 false 时会 finish 那个窗口）；压了守卫反而让 canGoBack 恒 true，
  //    宿主就会去"退历史"、把窗口显示成上一条页面。主窗口才需要守卫（它内部有很多层）。
  // ⚠️ 判据只此一处：boot() 里不再重复赋值（同一条判据分两处写，迟早会分叉）。
  var singleMode = false;
  try { singleMode = !!new URLSearchParams(location.search).get('path'); } catch (e) {}

  // ---------- 宿主返回的「守卫条目」：让系统的滑动返回退不出去 ----------
  // 手机端的返回手势 / 返回键由**宿主原生层**处理，页面拿不到它的回调、也拦不住它 ——
  // 但它的落地形式是"让 WebView 退一格历史"。这恰好给了我们一个着力点：
  // **只要历史里始终有一条属于本应用的条目，它就永远退不到应用之外去。**
  //
  // 机制就两条：
  //   ① 启动时压一条守卫条目（pushState）—— 给"别人发起的返回"准备一个落点；
  //   ② popstate 里**立刻把守卫补回去**，除此之外什么都不做 —— 这就是"滑动无反应"。
  // 于是无论滑多少次，历史指针都只在"当前条目 ↔ 守卫条目"之间来回，页面不会被卸载。
  //
  // ⚠️ 三条纪律（前几轮实测换来的，别再改回去）：
  //   · **我们永远不主动 history.back()** —— 主动退历史会与宿主发起的那一次叠加，
  //     一次滑动退两层（2.1.3 的实测教训）。这里只有 pushState，一次都没有 back。
  //   · **只在触屏档压** —— 桌面有自己的关闭按钮，不该被改动（手机与桌面不搅和）。
  //   · 压不上（极端沙箱环境）就静默退化：那只是回到"宿主可能把页面退出去"的旧状态，
  //     绝不会让页面本身出错。
  //
  // ⚠️ 这里不再讨论"要不要挂宿主的 setExitPageTips" —— 2.4.2 起用户明确要求用它
  //   （手势返回只有宿主原生框能拦到），实现见 syncExitTips。当年的分析留档如下：
  //   它的语义是"**用户离开应用页面时**显示的确认提示"—— 只能"问一句"，不能"拦住"；
  //   而用户要的是**滑动无反应**，所有弹窗（含宿主的）都属于"有反应"，方向相反。
  //   何况实测那台设备上它并没有生效（否则滑动会弹框，而不是直接退出）。
  //   ⇒ 能"拦住"的只有这条守卫条目。
  // ⚠️ 不记"守卫压没压上"这种状态量：我们没有"敢不敢动历史"的分支（永不主动 back），
  //    记了就是一个只写不读的死变量。真需要核查守卫在不在，看 `history.state.mvGuard`
  //    就行 —— 那是**真实历史状态**，比我们自己的记录可信（探针就是这么验的）。
  function mvGuardArm() {
    try {
      history.pushState({ mvGuard: 1 }, '');
    } catch (e) {
      // 极端沙箱环境压不上：静默退化（回到"宿主可能把页面退出去"的旧状态），
      // 绝不因此让页面本身出错。
    }
  }

  // 系统 / 宿主的返回都落在这里。**只补守卫；唯一的例外是大图查看器**。
  //
  // 为什么只对大图破例（这是本节最需要想清楚的一处）：
  //   · 用户要的是**网格里滑动无反应**（"在文件夹滑动返回会直接退出"）—— 那里有左上角
  //     箭头可以逐级退，所以"吃掉返回"不会把人困住；
  //   · 但**大图里页面内没有任何返回出口**（2.1.1 起按用户要求删掉了大图的 ← 按钮，
  //     手势也在 2.2.0 删了）。若把返回也吃掉，大图就成了死胡同 —— 只能退出整个应用
  //     再重新进来。用户此前明确接受过"大图 → 网格"这个行为（他抱怨的是
  //     "到了网格继续滑就退出应用"）。所以这里保留它。
  //   其余（网格 / 子目录 / 第一级）一律**什么都不做**。
  //
  // ⚠️ 特意写成**具名函数**再注册，而不是内联匿名回调：匿名回调没法从测试里取出来
  //    真跑，测试就只能"手抄一份逻辑"，于是改源码它照样绿 —— 那是自证式测试。
  // 2.3.0：这次关查看器是不是"返回"触发的（是 ⇒ 历史已经退了，不要再补 back）
  var mvViewerFromPop = false;
  // 2.3.2：**我们自己**发起的 history.back()（closeViewer 收自己压的那条）。
  //   它的 popstate 会被 mvOnPop 收到，但那时查看器已经关了 —— 如果不打标记，
  //   mvOnPop 就会接着走"子目录回上级"，于是"点一次箭头 = 关查看器 + 退一层目录"
  //   （真 Edge 实测：URL 从 ?dir=/vol1 直接变成 ?dir=/）。
  var mvSelfBack = false;

  function mvOnPop() {
    mvGuardArm();
    // 2.3.2：这一次 popstate 是 closeViewer 主动退历史引起的 —— 它已经把关查看器这件事做完了，
    //   这里**只能补守卫、不能再退层**，否则一次点击会退两层（关查看器 + 回上级目录）。
    if (mvSelfBack) { mvSelfBack = false; return; }
    if (!viewer.classList.contains('hidden')) {
      mvViewerFromPop = true;
      try { closeViewer(); } finally { mvViewerFromPop = false; }
      return;
    }
    // 2.3.0：**网格里返回 = 回上一层目录**。
    //   2.2.x 这里曾是"什么都不做"（当时为了做到"滑动无反应"），但那让网格里的返回
    //   完全失效 —— 用户在子目录里按返回，页面不动，只能靠左上角箭头。
    //   现在与"查看器返回 = 关查看器"统一成**同一个出口**：
    //     查看器开着 → 关查看器
    //     子目录     → 回上一层目录
    //     已在最上层 → 什么都不做（保持"滑动无反应"，不退出应用）
    if (!state.root && state.path) { goUpLevel(); return; }
    // 2.4.1：**已在聚合根层** ⇒ 问一句"是否退出"（宿主闸门实测不生效，改为页面自己弹）。
    //   点「留下」会补回守卫，用户原地不动；点「退出」走 hostCall('close')。
    // 2.4.2：已在聚合根层 —— **什么都不做**，把"是否退出"交给宿主原生弹窗
    //   （syncExitTips 已在进根层时把它挂上）。这里若补守卫即可，不再自绘任何框。
    return;
  }

  // 2.4.0：**离开确认闸门** —— 只在"聚合根层"挂上（仅手机版）。
  //
  //   语义：在根层再返回一次就是"离开这个应用"，这时问一句是合理的；
  //   子目录/查看器里问就成了"把回上一层吞掉"，所以**必须跟状态走**。
  //   （bridge.js 里保留了 2.2.1 删它时的分析，可对照。）
  // ---------- 退出前的确认（照搬 2.2.0 的双入口方案）----------
  //
  // 两条入口共用同一句文案，但**用不同的框**：
  //   ② 系统返回（返回手势 / 返回键）：那是宿主原生行为，网页拿不到回调、也拦不住 ——
  //      唯一能施加影响的是宿主提供的 setExitPageTips（见 syncExitTips），弹的是**原生**框。
  //
  // 为什么不用 window.confirm()：宿主 WebView 里它可能被禁用、样式也不受控；
  // 自绘能保证「有取消键、且取消真的留在原地」。
  

  // 通用确认弹层：确定 → onOk()；取消 / 点遮罩 → 什么都不做（留在原地）。
  // 每次都重新绑定 handler 并在收起时清掉，避免"上一次的 onOk 被下一次触发"。
  

  // 退出整个应用：借宿主桥接的 close。
  // 不在宿主内（独立标签页/桌面浏览器）时 hostCall 会 reject —— 忽略即可。
  

  // 2.4.3：初始值必须是 **null**，不能是 false。
  //
  //   语义是"**我不知道宿主侧现在是挂着的还是清掉的**"。
  //   为什么不能用 false：手机版进目录走**真导航**（整页重载），新页面里这个变量会回到
  //   初始值；而宿主侧的 setExitPageTips 是**应用级**的，整页重载**不会**清掉它。
  //   若初始成 false，新页面在子目录算出的 want 也是 false ⇒ 判定"状态没变"⇒ **不下发**，
  //   于是上一个页面挂的闸门就一直留着 —— 用户实测到的"任何层返回都弹框"就是这么来的。
  //   用 null 则每个新页面**首次一定下发一次**，把宿主侧纠正到与当前层级一致；之后照旧去重。
  // 2.4.3：**聚合根层的判据**（内存状态 + URL 双保险）。
  //
  //   为什么还要看 URL：手机版进目录是真导航（整页重载），刚加载时内存里的 state
  //   可能还没就绪；而 **URL 就是层级的唯一真相**：
  //     · `?dir=/`            → 聚合根（navigateRoot 显式带的）
  //     · 没有任何 dir/path   → 首页（从桌面图标直接进来）
  //     · `?dir=/vol1/1000/x` → 用户目录里，不是聚合根
  //     · 有 `?path=`         → 单文件模式（独立看图窗口），不是聚合根
  //   聚合根页面的 URL 是**固定**的，所以这个判据比内存状态更稳。
  function isAggregateRoot() {
    if (state.root && !state.path) return true;              // 内存说在根层 ⇒ 根层
    if (state.path) return false;                            // 内存说在子目录/某个文件 ⇒ 不是根层
                                                             //   （此时 URL 可能还没跟上，不能只看它）
    // ⚠️ 必须**先解码**：真导航拼 URL 时用 encodeURIComponent，
    //    所以 `?dir=/` 在 location.search 里实际是 `?dir=%2F`。
    //    不解码的话"显式 ?dir=/"这一条永远不成立（行为探针抓出来的）。
    var q = location.search || '';
    try { q = decodeURIComponent(q); } catch (e) { /* 参数里有非法转义：用原串 */ }
    if (/[?&]path=/.test(q)) return false;                   // 单文件模式
    if (/[?&]dir=\/(?:&|$)/.test(q)) return true;             // 显式 ?dir=/
    if (!/[?&]dir=/.test(q)) return true;                    // 没有任何 dir ⇒ 首页
    return false;
  }

  var exitTipsArmed = null;
  function syncExitTips() {
    var want = (PROFILE === 'touch') && viewer.classList.contains('hidden') &&
      isAggregateRoot();
    if (want === exitTipsArmed) return;      // 状态没变就不重复打扰宿主
    exitTipsArmed = want;
    try {
      var b = window.fnosBridge;
      if (!b || typeof b.setExitPageTips !== 'function') return;   // 宿主没这能力：静默
      // ⚠️ 官方签名是 `{ title?, content? }`。
      //    2.4.0/2.4.1 我写成了 `{ tip: ... }` —— 宿主收不到有效参数，**根本不弹**。
      b.setExitPageTips(want
        ? { title: '退出急速媒体浏览？', content: '浏览位置与设置都会保留，下次进入可以接着看。' }
        : null);
    } catch (e) { /* 能力缺失 / 调用失败都不影响页面本身 */ }
  }

  // 启动接线：压守卫 + 接上 popstate。
  // ⚠️ 单独包成具名函数（而不是让下面的 if 直接写两行）是为了**可测**：
  //    测试能把这个函数抠出来真跑，验"压了几条、接没接上"；
  //    若写成 if 里的裸两行，测试就只能手抄——那就是自证式测试。
  function mvGuardInit() {
    mvGuardArm();
    window.addEventListener('popstate', mvOnPop);
  }

  // ⚠️ `!singleMode` 这一条不能少：独立看图窗口里压守卫，会把"返回=关掉这个窗口"
  //    变成"返回=退到历史里上一条"，宿主再按它自己的规则收尾 —— 这是不可控的一段。
  if (PROFILE === 'touch' && !singleMode) mvGuardInit();

  // ⛔ 这里曾有 syncExitTips()（2.2.0）：把宿主的「离开页面确认」**只在第一级**挂上，
  //    其余时刻清除。2.2.1 按用户要求整体撤掉，理由见上面「守卫条目」那段的最后一段：
  //    用户要的是**滑动无反应**，任何弹窗都属于"有反应"；而且它实测在那台设备上没生效。
  //    如果将来又想加回来，注意那条铁律：**必须跟状态走**（常挂的后果是每次返回都弹框、
  //    把"回上一层"整个吞掉 —— 这是 2.1.4 实测踩过的坑）。

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
    d.onclick = function () { if (path === null) navigateRoot(); else navigateTo(path); };
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
    // 可见行 = 用户此刻真正看得到的那几行；往外各留 2 行做滚动缓冲。
    // 缓冲行**只建元素、不请求缩略图**（eager=false，见 loadCellImage）：
    // 否则每打开一个目录都会白白多解码一整屏（实测 1200x800 下 24 张可见 + 12 张缓冲）。
    var viewStartRow = Math.max(0, Math.floor(top / cell));
    var viewEndRow = Math.ceil((top + h) / cell);
    var startRow = Math.max(0, viewStartRow - 2);
    var endRow = viewEndRow + 2;
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
      var r = Math.floor(idx / state.cols), c = idx % state.cols;
      var eager = r >= viewStartRow && r < viewEndRow;
      var x = c * (state.cell + state.gap) + 6;
      var y = r * (state.cell + state.gap);
      var el = cellPool[idx];
      if (!el) {
        el = createCell(item, idx, eager);
        cellPool[idx] = el;
        content.appendChild(el);
      } else {
        loadCellImage(el, eager); // 滚进可见区后才补发请求
      }
      el.style.transform = 'translate3d(' + x + 'px,' + y + 'px,0)';
      el.style.width = state.cell + 'px';
      el.style.height = state.cell + 'px';
    }
  }

  // loadCellImage 按需发起缩略图请求：只有「用户看得到的行」（eager=true）才真的去取。
  //
  // 为什么不用 <img loading="lazy">：实测在真实 Edge 里对它完全无效 ——
  // 格子用 position:absolute + translate3d 定位、且都挂在同一个滚动容器里，
  // 浏览器的 lazy 判定不会把它们当成"视口外"，缓冲行的图照样全部下载
  // （实测 1200x800 请求 36 张、1600x1000 请求 56 张，其中缓冲行占 12~16 张）。
  // 这里改成显式判定：URL 先存在 data-src 上，滚到可见才赋给 src。
  function loadCellImage(el, eager) {
    if (!eager) return;
    var img = el && el.querySelector ? el.querySelector('img.cell-thumb') : null;
    if (!img || img.dataset.mvReq) return; // 已经排过队，别重复
    var url = img.getAttribute('data-src');
    if (!url) return;
    img.dataset.mvReq = '1';
    enqueueGridThumb(img, url); // 走并发闸，不再直接赋 src
  }

  // ---------- 网格缩略图并发闸 ----------
  //
  // 同源只有 6 条 HTTP/1.1 连接。网格一屏有 20+ 张缩略图，若一次性全发出去，连接会被
  // 占满 —— 此时点开大图，/api/raw 只能排在队尾，用户看到的就是「要生成两页缩略图，
  // 大图才响应」。这里做两件事：
  //   ① 同时最多 GRID_THUMB_INFLIGHT 个在途 —— 永远给大图留出连接余量；
  //   ② 查看器打开期间整体停发，把连接完全让给大图。
  // 后端那侧也有让路机制（看大图期间缩略图请求直接返回占位图），但前提是请求能
  // 到得了后端 —— 堵在浏览器连接队列里时它无从生效，所以闸必须设在最前面。
  var GRID_THUMB_INFLIGHT = 4;
  var gridThumbInflight = 0;
  var gridThumbQueue = [];

  function viewerOpen() { return viewer && !viewer.classList.contains('hidden'); }

  function gridThumbPump() {
    while (gridThumbQueue.length && gridThumbInflight < GRID_THUMB_INFLIGHT) {
      if (viewerOpen()) return; // 大图正用连接 → 按住不发
      var job = gridThumbQueue.shift();
      if (!job.img.isConnected) continue; // 格子已被回收
      startGridThumb(job.img, job.src);
    }
  }

  function startGridThumb(img, src) {
    gridThumbInflight++;
    var done = function () {
      img.removeEventListener('load', done);
      img.removeEventListener('error', done);
      gridThumbInflight--;
      if (gridThumbInflight < 0) gridThumbInflight = 0;
      gridThumbPump();
    };
    img.addEventListener('load', done);
    img.addEventListener('error', done);
    // 打上「这次 src 替换是我们主动做的」标记：它会让上一次在途请求被 abort 并
    // 派发 error，onerror 里据此跳过降级（见 createCell 的 onerror）。
    // load 成功后清掉，避免掩盖后续真正的加载失败。
    img.dataset.mvAbort = '1';
    img.src = src;
  }

  function enqueueGridThumb(img, src) {
    if (viewerOpen()) return; // 大图在跑：这次不发（关闭查看器后 layout 会重建格子）
    if (gridThumbInflight < GRID_THUMB_INFLIGHT) { startGridThumb(img, src); return; }
    gridThumbQueue.push({ img: img, src: src });
  }

  // 打开查看器时调用：把排队的网格请求整批丢弃，只留大图用连接
  function dropGridThumbQueue() {
    gridThumbQueue = [];
  }

  // eager=true 表示这个格子落在「用户看得到的行」里，创建时立即请求缩略图；
  // eager=false 是滚动缓冲行，只建元素、URL 暂存 data-src，等滚进可见区再请求。
  function createCell(item, idx, eager) {
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
      inner.onclick = function () { navigateTo(item.path); };
    } else if (!cfg.thumbEnabled) {
      // 缩略图已关闭：降级为「图标 + 扩展名 + 文件名」，不产生任何缩略图请求
      inner.appendChild(fileCard(item));
      (function (it) { inner.onclick = function () { openViewer(it); }; })(item);
    } else {
      var img = new Image();
      img.className = 'cell-thumb'; img.decoding = 'async';
    // 网格缩略图标「低优先级」。
    // 同源只有 6 条 HTTP/1.1 连接：用户点开大图时，网格里排队的缩略图请求会把连接
    // 占满，/api/raw 只能排在后面 —— 这正是「要生成两页缩略图大图才响应」的直接原因。
    // 大图侧是 fetchPriority='high'，这里标 low，浏览器就会把连接优先让给大图。
    img.fetchPriority = 'low';
      // URL 先挂在 data-src 上，只有可见行才由 loadCellImage 真正发起请求。
      // 不用 loading="lazy"：实测对 transform 定位的格子无效（缓冲行照样全下载）。
      img.setAttribute('data-src', thumbURL(item.path, cfg.thumbSize || 320, item.mtime));
      img.alt = item.name;
      // 关键：<img> 在「有 alt、无 src」时，浏览器会把它原生渲染成
      // 「破图图标 + alt 文本」—— 这就是每个格子在缩略图出来之前看到的那一小块
      // 「损毁小图标 + 文件名」。先塞一张 1×1 透明图当 src 即可消除该渲染；
      // 真正要看的缩略图 URL 仍挂在 data-src 上，滚进可见区时再替换。
      img.src = BLANK_PIXEL;
      // 缩略图加载失败：只对当前图片降级为图标，不影响其他图片（避免一张图失败导致全局关闭缩略图）
      // 加载失败不要立刻降级。
      // 原因：请求被**我们主动取消**时（并发闸换 src、暂停让路、切目录清连接、
      // 关闭查看器后重发），浏览器同样会派发 error 事件 —— 那不是"图坏了"。
      // 立刻降级就会先闪一个「图标 + 文件名」的卡片，随后缩略图才刷出来。
      // 做法：等 1.2s，期间自动重试一次；仍未成功才真的降级。
      var failTimer = null;
      img.onerror = function () {
        if (img.dataset.mvFailed) return;
        if (img.dataset.mvAbort) { delete img.dataset.mvAbort; return; }
        clearTimeout(failTimer);
        failTimer = setTimeout(function () {
          if (img.dataset.mvFailed) return;
          if (!img.isConnected) return;                       // 格子已回收
          if (img.complete && img.naturalWidth > 0) return;   // 已经成功了
          if (!img.dataset.mvRetried) {
            img.dataset.mvRetried = '1';
            img.dataset.mvAbort = '1';
            var url = img.getAttribute('data-src');
            if (url) { enqueueGridThumb(img, url); return; }
          }
          img.dataset.mvFailed = '1';
          if (!cfg.thumbEnabled) return;
          inner.innerHTML = '';
          inner.appendChild(fileCard(item));
          img.onerror = null;
          inner.onclick = function () { openViewer(item); };
        }, 1200);
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
    if (eager) { loadCellImage(el, true); }
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
        var url = rawURL(item.path, pickMaxdim(), item.mtime);
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
    dropGridThumbQueue(); // 并发闸里排队的也一并丢弃：连接要全让给大图
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
      // 走并发闸重新发起：直接赋 src 会在关闭查看器的瞬间把 20+ 个请求同时放出去，
      // 又占满连接（下一次点开大图就再次卡住）。
      enqueueGridThumb(e.img, e.src);
    }
    gridSuspended = [];
  }

  // 关闭查看器后，把网格里可见的缩略图**重新请求**一遍。
  //
  // 为什么必须重发：查看器打开期间后端对缩略图请求返回的是占位图（no-store），
  // 但 <img> 因此已经"加载完成"，浏览器不会再自己发请求 —— 不主动重发的话，
  // 那几张就永远停在白图（用户报的「出现几张白色缩略图」）。
  // 后端在恢复时会补生成真图，重发即可拿到。
  function reloadVisibleThumbs() {
    for (var k in cellPool) {
      var el = cellPool[k];
      var im = el && el.querySelector ? el.querySelector("img.cell-thumb") : null;
      if (!im || !im.isConnected) continue;
      var url = im.getAttribute("data-src");
      if (!url) continue;
      im.dataset.mvReq = "1";
      im.dataset.mvAbort = "1";   // 断开同样会派发 error，别当失败降级
      im.src = BLANK_PIXEL;       // 先断开，否则同 URL 不会重新发请求
      enqueueGridThumb(im, url);
    }
  }

  // ---------- 切目录时释放浏览器连接池 ----------
  //
  // 浏览器对同源只有 6 条 HTTP/1.1 连接。网格里正在生成的缩略图会把它们占满，
  // 此时切目录发出的 /api/list 只能排到队尾 —— 用户看到的现象就是「点了目录
  // 没反应，要等几张缩略图冒出来界面才切换」。
  // 实测（真实 Chromium，40 张未缓存缩略图 / 单张 1.2s）：
  //   不取消        /api/list 等 1130 ~ 7140 ms（1~6 个缩略图生成周期）
  //   只清空 DOM    /api/list 等 7140 ms（清 DOM 不会 abort 请求）
  //   调用本函数    /api/list 等    15 ms（快约 100~500 倍）
  //
  // 原理：给 <img> 换 src 会让浏览器 abort 掉上一个还在飞的请求，立刻归还连接。
  // 与 suspendGridRequests 的区别：那个是「暂停、稍后恢复」（打开查看器用），
  // 这个是一次性丢弃 —— 旧格子马上会被 layout() 销毁，不需要恢复。
  function cancelPendingGridRequests() {
    for (var k in cellPool) {
      var el = cellPool[k];
      var im = el && el.querySelector ? el.querySelector('img.cell-thumb') : null;
      if (im && !im.complete) {
        im.onerror = null; // 占位不是「加载失败」，别触发降级成文件图标
        im.src = BLANK_PIXEL;
      }
    }
    gridSuspended = []; // 丢掉暂停记录，避免 resume 把已取消的请求又拉起来
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
  // 查看器内「控件」统一判定：指针事件落在这类元素上时不参与滑动/点击切图。
  // 集中成一个常量，避免以后新增控件时漏改 —— 漏改的症状很怪：
  // 「点控件，图片先切一张、然后才响应」。
  var VIEWER_CTL_SEL = '.v-controls';
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
    // ⚠️ **触屏档强制遮罩模式**（不看 viewerMode 设置）—— 2.2.2 按用户实测做的改动。
    //
    // 用户报的："大图浏览应该退到网格，可现在点击进去退出再点击大图退出就会退出软件。"
    // 根因不在我们这一层：`viewerMode` 的默认值是 'window'，也就是**请宿主另开一个独立的
    // 看图窗口**（那份文档的 URL 带 ?path=）。那个窗口的历史栈是它自己的，主窗口压的守卫
    // 管不到它；而它"返回"时怎么收尾（关子窗口 / 退历史 / 结束页面）完全由宿主决定 ——
    // 我们既拿不到回调、也拦不住。这正是"返回却退出软件"的来源。
    //
    // 遮罩模式下大图只是**叠在网格上的一层**：返回 → 我们的 popstate → 关查看器 → 退回网格，
    // 全程由本应用掌控。真浏览器里连做 3 轮「点开 → 返回」已实测：文档存活、
    // 每次都退回网格、守卫每次补回。
    //
    // 桌面（鼠标）**仍尊重设置** —— 多窗口在桌面上是有意义的，不该被手机的问题牵连。
    if (PROFILE === 'touch') { openViewerOverlay(target); return; }
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
    // 打开前先把本页可能残留的媒体停掉（防御：任何"隐藏而非销毁"的上一次会话）
    stopAllMediaIn(vWrap);
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
    // 2.3.0：**查看器 = 一层历史**（仅触屏档）。
    //
    // 为什么必须做：查看器此前只是"叠在网格上的遮罩"、**不产生任何历史条目**。
    //   历史里只有 mvGuardArm 压的那一条常驻守卫，于是：
    //     在查看器里按返回 → 退掉的是那条**守卫条目**（而不是"关掉查看器"）
    //     → 查看器仍开着、而守卫已经没了 ⇒ 用户再返回一次就**直接退出应用**。
    //   （这正是用户报的"点进大图 → 返回 → 退出软件"。）
    //   压上这一条之后，"返回"先退掉查看器这一层 ⇒ popstate → closeViewer ⇒ 回到网格。
    // ⚠️ 网格的"进目录"在本版**不是**真导航（goUpLevel → loadDir 内部切换），
    //    所以网格里的逐层返回靠的是"守卫每次补回 + mvOnPop 里回上级"，与查看器这条各管各的层级。
    // 桌面档不加：桌面查看器是独立窗口，语义不同。
    if (PROFILE === 'touch') {
      try { history.pushState({ mvViewer: 1 }, ''); } catch (e) {}
    }
    // 2.4.2：查看器打开 ⇒ 不是"最外层"了，撤掉宿主闸门（旧版 2.2.0 也是这么接的）
    try { syncExitTips(); } catch (e) {}
  }

  // 独立窗口模式：三层降级
  //   1) 宿主桌面子窗口（openApp —— 需在飞牛桌面 iframe 内，且宿主公开了该方法）
  //   2) 浏览器独立窗口（window.open）
  //   3) 窗口内遮罩
  // 每条失败分支都必须给 toast：历史 bug 里最常见的失败路径恰恰是静默的，
  // 用户以为「点了没反应」，开发者也拿不到任何线索。
  function openViewerWindow(target) {
    var path = target.path;
    // 必须用「网格当前实际用的排序」：它可能是从文件管理器偏好解析出来的，
    // 与 state.sort（本地值）不一定相同。用错会让大图的顺序与网格对不上 ——
    // 典型表现是「点网格第 1 张，大图却显示第 10 张」。
    // 这里是「在急速媒体浏览里点开」的场景：带上本应用网格**当前实际用的排序**，
    // 独立窗口直接用，不再去问文件管理器。两种入口因此天然分开：
    //   飞牛文件管理器双击   → URL 只有 path → 独立窗口读**文件管理器**的排序
    //   急速媒体浏览网格点击 → URL 带 sort   → 独立窗口用**本应用**的排序
    // state.currentSort 是 loadDir 解析出来的结果（开关开=飞牛的值，关=本应用的值），
    // 传下去的值与网格所见必然一致。
    // 旧实现把 state.currentSort（本应用解析出来的值）拼进 URL，等于替用户定了排序 ——
    // 独立窗口拿到 sort 参数就直接用（openSingle 的 if (sort) 分支），不再读文件管理器偏好。
    // 结果：「跟随文件管理器排序」对这个窗口完全失效 —— 在文件管理器里换排序，大图顺序不变
    // （用户报的「大图只走软件内的设置，文件管理无效」）。
    // 独立窗口重新解析一次才能真正跟随：它带 force=true，不会被本会话的手动排序挡住。
    // 桌面子窗口是 app/ui/config 里单独注册的入口 mediaview.viewer，
    // 参数走 URL 查询串（宿主会把 params 合并进入口 URL），
    // 所以这里传的是一个「锚点」字符串，而不是位置参数。
    var sort = state.currentSort || state.sort || 'name';
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
      // ★ 固定窗口名（原来带 Date.now()）。
      //   带时间戳 = 每次都是**新窗口**，旧窗口既不复用也不关闭 ——
      //   而旧窗口里的视频只要文档还活着就一直在播（实测：隐藏不停播），
      //   于是"连着看几个视频"就变成好几个视频同时出声、同时吃 CPU/GPU。
      //   固定名字后浏览器会复用同一个窗口：旧文档被卸载 → 里面的视频随之销毁。
      w = window.open(pageURL, 'mediaview_viewer', 'width=1280,height=880');
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
  // 1.8.175：**立即**恢复后台缩略图生成，并在后端确认恢复之后才执行 andThen。
  //
  // 为什么必须"等它返回"：
  //   closeViewer() 原本的顺序是 reloadVisibleThumbs() → … → resumeThumbGen()。
  //   而 reloadVisibleThumbs() 是**同步**把每个可见缩略图重发一遍，
  //   此时后端仍处在"暂停"状态 —— 它对缩略图请求返回的是**占位图（no-store）**，
  //   <img> 收到后认为"加载完成"，浏览器不会再自动重发，于是那一片**永远停在白图**。
  //   （后端在真正 resume 之后才会补生成真图，但前端已经不会再问了。）
  //   这就是用户报的「点序号返回网格后，缩略图全都变成空白」。
  //
  //   把重发放到 resume 的回调里，就保证"先恢复、再重发"，拿到的是真图。
  function resumeThumbGenNow(andThen) {
    if (resumeTimer) { clearTimeout(resumeTimer); resumeTimer = null; }
    var go = function () { try { if (andThen) andThen(); } catch (e) {} };
    try {
      fetch(API + '/thumb/resume', { method: 'POST' }).then(go, go);
    } catch (e) { go(); }
  }
  // 页面卸载兜底：立即发 resume（不等定时器），用 sendBeacon 确保请求发得出去。
  // 关闭查看器窗口时宿主会立刻销毁页面，之后的 setTimeout/fetch 都不会执行；
  // 残留的暂停状态会让缩略图一直停到 TTL 到期（最长 2 分钟）。这里抢发恢复信号。
  function flushResumeThumbGen() {
    if (resumeTimer) { clearTimeout(resumeTimer); resumeTimer = null; }
    try {
      if (navigator.sendBeacon) {
        // 显式带空 body：确定走 POST，且 Content-Length 为 0，服务端只认方法。
        navigator.sendBeacon(API + '/thumb/resume', new Blob([], { type: 'text/plain' }));
      } else {
        fetch(API + '/thumb/resume', { method: 'POST', keepalive: true }).catch(function () {});
      }
    } catch (e) {}
  }

  // Single-file open mode (fileTypes entry): ?path=...
  // 打开后异步加载同目录媒体，支持左右切换到相邻图片/视频
  // 2.0.0：大图同目录列表的加载提示（DOM 见 index.html #vListHint）。
  // 列表就绪前左右切换不可用，原来没有任何反馈 → 用户以为"打开后不能翻页/坏了"。
  function showListHint(on) {
    var el = $('vListHint');
    if (!el) return;
    if (on) el.classList.remove('hidden'); else el.classList.add('hidden');
  }
  function openSingle(path, sort) {
    // 打开前清掉本页可能残留的媒体（防御：任何"隐藏而非销毁"的上一次会话）
    stopAllMediaIn(vWrap);
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

    // 1.8.188：**删除了 `paths`（宿主有序列表）分支**。
    // 依据（全量 nginx 日志统计）：open 请求 2013 条，带 paths 的 **0 条**；
    // 飞牛前端里 `paths=` 只出现在"文件夹选择 / 分享 / 分享链接 / PDF"，与"打开大图"无关。
    // 它是早期版本遗留、实测从未出现；而且若真出现，它给的顺序与"用户当前在文件管理器
    // 里设的排序"可能冲突，反而把顺序带偏。来源因此收敛为：
    //   大图 → URL 的 sort（若有）→ IndexedDB 偏好 → name:asc
    //   网格 → state.sort

    // 异步拉取同目录列表，把当前文件定位进去，即可左右切换
    var dir = dirName(path);
    if (!dir) return;
    // 2.0.0：列表就绪前左右切换不可用 —— 给个提示，别让用户以为坏了。
    showListHint(true);
    // sort 为 null 时：等设置就绪后读文件管理器的排序偏好（「跟随」开关控制）。
    // 列表顺序决定左右切换顺序与大图预加载方向，这里必须和文件管理器一致。
    var listPromise;
    var usedSort = null; // 本次实际使用的排序（带方向），供前端重排与诊断
    if (sort) {
      // 必须记下这个带方向的串：独立窗口（/viewer?path=…&sort=…）走的就是这条分支，
      // 若不记，下面重排只能退到后端回传的**裸字段名**（如 "name"），方向随之丢失，
      // 表现为「一个方向正常、另一个方向被排成相反的顺序」。
      usedSort = sort;
      listPromise = fetchJSON(API + '/list?path=' + enc(dir) + '&sort=' + enc(sort));
    } else {
      // 1.8.173：**删掉了"先问飞牛文件管理器接口要顺序"这一步**。
      //
      // 那个接口（/app/fnos-file-manager-mcbbc/api/v1/file-info）飞牛已经**删掉**了：
      // server.cjs 里 "file-info" 出现 0 次（路由没注册）。console 里的 401 不是
      // "在但不授权"，而是该应用对**所有非公开 /api/** 的统一登录闸门 ——
      // 实测真实路径与乱写路径返回一模一样的 {"error":"请先登录"}。
      // 也就是**在当前飞牛版本上它百分百降级**，且不会因拿到授权而恢复：
      // 却每次打开目录都要付：控制台一条红字 + 白占一条同源 HTTP/1.1 连接
      // （同源只有 6 条，缩略图正在抢）+ 白等一个 RTT。
      //
      // 现在这条才是真正干活的：**清单一律走 /api/list**（元素带 size/mtime 等字段）。
      //
      // ⚠️ 顺序：后端 naturalLess 是**字节序**，对中文并不等于飞牛（它把汉字排到最后）。
      //    真正的飞牛顺序由**前端**用 Intl.Collator('zh',{numeric:true}) 重排得到
      //    （见 resortLikeFnOs）；1.8.180 曾误删前端重排、以为"后端才与飞牛逐字一致"，
      //    190/191 已改回。排序字段由 resolveSortForDir 决定：大图读偏好，网格用 state.sort。
      listPromise = resolveSortForDir(dir, true).then(function (s) {
        usedSort = s;
        return fetchJSON(API + '/list?path=' + enc(dir) + '&sort=' + enc(s));
      });
    }
    listPromise.then(function (d) {
      showListHint(false); // 列表已就绪：现在可以左右切换了
      var files = d.files || [];
      // 前端重排必须用「本次实际请求的排序」（带方向）。后端回传的 sort 字段只有
      // 字段名（如 "name"），拿它重排会退化成默认方向 —— 升序/降序就都变成同一个
      // 方向了（1.8.76 的 bug）。usedSort 为空时才退回响应字段/state.sort。
      usedSort = usedSort || (d && d.sort) || state.sort;
      // 1.8.190：与网格用同一套飞牛规则重排，两处顺序才会完全一致。
      files = resortLikeFnOs(files, usedSort);
      if (files.length <= 1) return;
      // 定位「正在显示的那张」，而不是打开时的 path：列表是异步到达的，
      // 若期间显示已经变过（快速切换、从网格进入等），按旧 path 定位会把用户
      // 弹回打开时那张图。先按当前显示定位，找不到再退回按 path 找一次。
      var shown0 = v.list[v.index];
      var want = normPath(shown0 ? shown0.path : path);
      var idx = -1;
      for (var i = 0; i < files.length; i++) {
        if (normPath(files[i].path) === want) { idx = i; break; }
      }
      if (idx < 0) {
        var orig = normPath(path);
        for (var j = 0; j < files.length; j++) {
          if (normPath(files[j].path) === orig) { idx = j; break; }
        }
      }
      if (idx < 0) {
        // 当前文件不在列表里（排序/过滤差异）：插到开头，仍可浏览目录其他文件
        files.unshift(shown0 || { kind: kind, path: path, name: name });
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
    }).catch(function () { showListHint(false); /* 列表加载失败不影响当前文件查看 */ });
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
    // 用**复用式**重排而不是 rebuildTrackInPlace：
    // 后者先清空轨道再重建 <img>，而新图在 decode 完成前是 visibility:hidden ——
    // 清空后到新图露面之间会露出旧图，用户看到的就是「翻页先显示错的图，然后闪到
    // 正确的图」。syncTrackToIndex 会复用轨道里**同路径**的元素（位图已在内存），
    // 重新插入即可立即绘制，中间没有任何空窗。
    var track = vWrap.querySelector('.v-track');
    if (track) {
      syncTrackToIndex(track);
    } else {
      rebuildTrackInPlace(preload);   // 首次（还没有 track）才需要新建
    }
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
      stopMedia(old._media);   // 统一出口：pause + 断源 + 释放解码器
      track.removeChild(old);
    }
    // current 始终渲染（优先创建，浏览器优先加载）
    var curBox = createMediaBox(v.list[v.index], true);
    // 2.3.0（P1）：prev **也预加载**。
    //   原来 prevBox 恒为空占位（为省内存），后果是"向左回看第一下必然白屏等待" ——
    //   而向右切因为有 next，是顺的。左右手感不对称，用户能直接感觉到。
    //   这里与 next 对称：viewerPreload >= 1 且存在上一张就渲染它。
    //   （DOM 仍是有限个盒子：prev + cur + next，换图时 stopMedia 会释放解码器。）
    var prevBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index > 0) {
      prevBox = createMediaBox(v.list[v.index - 1], false);
    }
    // next：始终渲染（只要 viewerPreload >= 1），确保第一次切换时 next 已在 DOM、不闪屏
    // current 在 DOM 中排前面，浏览器仍优先加载 current
    var nextBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index < v.list.length - 1) {
      nextBox = createMediaBox(v.list[v.index + 1], false);
    }

    track.appendChild(prevBox);
    track.appendChild(curBox);
    v.media = curBox._media;
    v.hiSrc = ''; // 换了源（或原地重建）→ 重新从缩放版开始，放大时再按需升级
    // 1.8.160：这里必须重置视图状态 —— **这是"关闭查看器后重新打开"路径上唯一的重置点**。
    //
    // ⚠️ 不要把它当成冗余删掉。它曾被认为是"误插"，因为本函数注释写着"原地重建"、
    // 看起来只在首次打开时跑。实际上：
    //   · closeViewer() 里是 `vWrap.innerHTML = ''` —— **轨道被整个销毁**，
    //     但 **v.scale / v.zoomed / v.tx / v.ty 不会被重置**；
    //   · 于是"放大到 8 倍 → 关闭查看器 → 重新打开一张图"时，
    //     `vWrap.querySelector('.v-track')` 找不到轨道 → 落到本函数（见 showCurrent），
    //     而 v.scale 还是 8 倍 —— 不重置的话，新图会**直接按 8 倍显示**。
    //   · 这正是历史上"切图后自己放大"那个 bug 的同一形态。
    // （换图路径另有 syncTrackToIndex 里的同样两行，两处都需要。）
    resetView(); // v.scale=1 / tx=ty=0 / zoomed=false / actual=false，并刷新按钮
    // 必须紧接着 applyTransform()：resetView() 只改 v.scale/v.tx/v.ty 这些**状态变量**，
    // 图片的实际缩放靠 CSS `transform: translate(...) scale(...)` 表达，那一步在这里。
    // 只调 resetView() 的话状态已归位、**DOM 上却还留着旧 transform**。
    applyTransform();
    updateFitBtn();
    track.appendChild(nextBox);

    // 重置位置（无动画），中间子元素 = current
    track.style.transform = 'translateX(-100%)';

    // 预加载后面的图片（next 已在 track 里，从 next+1 开始预加载 viewerPreload-1 张）
    // preload=false 时：next 不进 DOM（首屏只加载 current，保证打开速度），
    // 但后台低优先级预加载 next 原图，第一次切换时已缓存、不闪烁
    if (!preload) {
      warmTrackBoxes(typeof dir === 'number' ? dir : 1);
    }
  }

  // 预加载当前图后面的 count 张（从 next+1 开始），浏览器缓存后切换时秒开
  // 1.8.149：把轨道里**已有**的盒子提前 decode —— 零额外请求、零额外解码。
  //
  // 为什么替换掉原来的 preloadNextImage / preloadPrevImage / preloadAhead：
  //   它们用 `new Image()` 发的 URL 与轨道里相邻盒子的 `<img src>` **完全相同**，
  //   而那个盒子在 createMediaBox 里早就赋了同一个 src、并已在加载 ——
  //   所以这些"预取"不产生任何新信息（HTTP 缓存必然命中），净作用只有两个坏处：
  //     · 没有 X-MediaView-Prefetch 头 → 走阻塞抢槽，把**你正在等的那张**往后推
  //       （NAS 实测：单张冷 2.94s，被 4 个预取挤到 8.57s）；
  //     · 另开一个 Image 对象**重新解码一份大图**，与正在显示的那张抢 CPU。
  //   而且它对 swipeTo 真正等待的那个 decode() 毫无帮助 —— 预取热的是另一个对象。
  //
  // 现在只对轨道里已经存在的盒子调 decode()：纯收益、零额外开销。
  // 1.8.151：方向化预载 —— 按**序号**热"你正要去的那一侧"后面 N 张。
  //
  // 用户要的行为：点下一张之后，就开始按序号加载**第三张**（即永远按设置里的
  // 「大图预加载数量」预载后面 N 张），而且**分清左右** —— 向左切就预载左边。
  //
  // 为什么要区分"在不在轨道里"：
  //   · 轨道里已有的盒子（index±1）**自己就在加载**，对它调 decode() 只是让它提前解码，
  //     不会产生额外请求 —— 直接热；
  //   · 轨道外的（index±2 起）**没有对应元素**，必须新建 Image 才会真的发请求。
  //     这不是 1.8.149 删掉的那种"纯重复预取"：那些与轨道盒子的 src 完全相同，
  //     而这里的张数在轨道里根本不存在。
  function warmTrackBoxes(dir) {
    if (typeof dir !== 'number' || !dir) dir = 1; // 打开首屏时没有方向 → 默认向右预载
    var m = v.media;
    if (m && m.tagName === 'IMG' && m.decode) m.decode().catch(function () {});

    var n = cfg.viewerPreload || 0;
    if (n < 1) return;
    if (n > 4) n = 4; // 防御：再多就是无谓占带宽
    var step = (dir < 0) ? -1 : 1; // 分清左右：向左切就沿负方向预载

    var track = vWrap.querySelector('.v-track');
    var inTrack = {};
    if (track) {
      for (var i = 0; i < track.children.length; i++) {
        var p = track.children[i]._itemPath;
        if (p) inTrack[normPath(p)] = true;
      }
    }
    for (var k = 1; k <= n; k++) {
      var idx = v.index + step * k;
      if (idx < 0 || idx >= v.list.length) break;
      var it = v.list[idx];
      if (!it || it.kind !== 'image' || !it.path) continue;
      if (inTrack[normPath(it.path)]) continue; // 轨道里已有 → 它会自己加载
      var im = new Image();
      im.fetchPriority = 'low';
      // 1.8.153：带上后端的预取标记。
      //
      // 后端契约（media.go）明确把预取排除在"用户正在看大图"之外：
      // 不带头会被当成真实请求 → 触发 noteForegroundPreview() → **kill 掉正在跑的后台预生成**
      // （thumb.go 的 cancelBackgroundCtx），而且用阻塞方式抢 scaledSem 的槽，
      // 把你正在等的那张往后推。
      // 网格里的悬停预取一直是对的（fetch + X-MediaView-Prefetch 头），这里是同一条契约。
      im.src = rawURL(it.path, pickMaxdim(), it.mtime) + '&prefetch=1';
      if (im.decode) im.decode().catch(function () {});
    }
  }

  // 「暂停」（元素还要留着、还要能被复用/继续播）：只 pause，**绝不断源**。
  // 用于"非当前格仍留在轨道里"和"页面暂时不可见"这两类 —— 它们随时可能回到前台。
  function pauseMedia(el) {
    if (!el || el.tagName !== 'VIDEO' || el.paused) return;
    try { el.pause(); } catch (e) { /* 忽略 */ }
  }

  // 媒体「停机」的唯一出口 —— 凡是要让媒体离开视线或被丢弃，都必须走这里。
  //
  // ⚠️ 为什么必须显式停（真机实测，Chromium）：
  //    · 容器 `display:none` / `visibility:hidden` → 1.5s 后 paused **仍为 false**，
  //      音频与解码都在继续；
  //    · 只有「从 DOM 移除」才会被浏览器自动暂停（HTML 规范要求）。
  //    ⇒ 任何"隐藏而不销毁"的路径都是泄漏源，必须显式停。
  //
  // ⚠️ 顺序要紧：先解绑 onerror 再 load()。createMediaBox 给视频挂了 onerror → toast，
  //    而 removeAttribute('src') + load() 会触发一次 error 事件，不解绑就会误弹「该编码可能不被浏览器支持」。
  //
  // ⚠️ 会断源（元素变空壳）→ **只用于"马上要丢弃"的元素**。
  //    还要复用的元素请用 pauseMedia()，否则切回来的那一格会是空白。
  function stopMedia(el) {
    if (!el || el.tagName !== 'VIDEO') return;
    try {
      el.onerror = null;
      el.onloadedmetadata = null;
      el.pause();                  // ① 立即停止播放（音频同步停）
      el.removeAttribute('src');   // ② 断开数据源，阻止继续下载
      el.load();                   // ③ 重新初始化 → 释放解码器与已缓冲数据
    } catch (e) { /* 元素已失效等极端情况，忽略 */ }
  }

  // 停掉某棵子树里的全部媒体。所有"丢弃前收尾"都用它 ——
  // 历史上正是因为每处各写一遍 pause 循环，才漏掉 closeViewer 那条路径。
  function stopAllMediaIn(root) {
    if (!root || !root.querySelectorAll) return;
    var vids = root.querySelectorAll('video');
    for (var i = 0; i < vids.length; i++) stopMedia(vids[i]);
  }

  // 同上，但只暂停、不断源（元素还留在文档里，随时可能回到前台继续播）。
  function pauseAllMediaIn(root) {
    if (!root || !root.querySelectorAll) return;
    var vids = root.querySelectorAll('video');
    for (var i = 0; i < vids.length; i++) pauseMedia(vids[i]);
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
      stopMedia(old._media);   // 统一出口（原来这里手写 pause，容易漏）
      track.removeChild(old);
    }
    track.classList.remove('anim');
    track.style.transitionDuration = '';
    var curBox = keep || createMediaBox(want, true);
    var nextBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index < v.list.length - 1) {
      nextBox = createMediaBox(v.list[v.index + 1], false);
    }
    // prev 也必须预建真实媒体。
    // 一直用空占位时：向**后退**的目标格是空的，必须临时新建 <img> 并等它 decode，
    // 这中间就是空窗 —— 用户看到「点倒退会闪图」。而**前进不闪**正是因为 next 格
    // 一直是真实元素（位图已在内存，移进来即可立即绘制）。
    // 代价是多预载一张相邻图；由 viewerPreload 控制（>=1 时才做）。
    var prevBox = emptyBox();
    if (cfg.viewerPreload >= 1 && v.index > 0) {
      prevBox = createMediaBox(v.list[v.index - 1], false);
    }
    track.appendChild(prevBox);
    track.appendChild(curBox);
    track.appendChild(nextBox);
    track.style.transform = 'translateX(-100%)';
    v.media = curBox._media;
    v.hiSrc = '';
    // 1.8.152：换图必须**完整重置**视图状态，不能只清 v.actual。
    //
    // Bug：原来只写 `v.actual = false`，而 v.scale / v.zoomed / v.tx / v.ty 会**留到下一张**。
    // 点过 1:1 之后巨图的 scale 是好几倍（applyActualScale 里 v.zoomed = v.scale > 1.01），
    // 于是切到下一张时 transform 按旧 scale 应用 —— 看起来"它自己放大了一些"，
    // 而且 v.zoomed 仍为真、切图守卫会把你挡在那里 —— 表现就是"切不了图"。
    resetView(); // v.scale=1 / tx=ty=0 / zoomed=false / actual=false，并刷新按钮
    // 1.8.154：**必须紧接着 applyTransform()**。
    // resetView() 只改 v.scale/v.tx/v.ty 这些状态变量；图片实际的缩放
    // 靠 CSS transform 表达，那一步在 applyTransform() 里。只调 resetView()
    // 的话状态已归位、**DOM 上却还留着上一张的 transform** —— 于是新图看上去
    // "自己放大了一些"（旧 scale 仍在生效）。
    applyTransform();
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
  // 兜底：**只有「解码真的卡死」才走这条**，而且它从 **load（数据下载完）之后**才开始计时 ——
  // 它要防的是"解码卡住"，不是"下载慢"（下载慢本来就该继续转圈）。
  // 参考：NAS 上 1 亿像素冷转码实测 2.5~2.9s，加传输与浏览器解码约 4.5s。
  var VIEWER_REVEAL_TIMEOUT = 8000;

  // 图片「完整解码后才露面」的**唯一闸门**（所有图片盒共用）。
  //
  // 为什么需要（1.8.150 修）：渐进式 JPEG 的位图按 scan 逐层细化，浏览器每解出一层就绘一帧；
  // 1 亿像素的图会连绘十几帧 —— 肉眼看到的就是「图片从上往下刷出来」。
  // 本文件很早的注释就写明过这个现象（"大 JPEG 会从上往下逐行绘制，那本身就是一种闪烁"），
  // 但当时的闸门**只装在 `isCurrent` 那一格**；而每次切换要显示的那张恰恰是
  // `isCurrent:false` 的相邻盒 —— 它从 src 赋值那刻就是可见的，
  // 于是"切换必闪"，只有首屏那一张不闪。
  //
  //   · `img.decode()` resolve 时位图**已可立即绘制**（这是它的保证）；
  //   · `complete` 只代表**数据下载完**，不代表位图解码完 —— 绝不能拿它当"可以露面"的判据
  //     （ensureMediaVisible 原来就是这么把闸门旁路掉的）；
  //   · 真正让中间帧不可见的是 `visibility:hidden`，所以两者必须一起用。
  //
  // 收敛成唯一一处的意义：现在有三个地方曾能改 visibility
  // （createMediaBox / img.onload / ensureMediaVisible），只要一处不看 decode，闪就会回来。
  function revealImage(img) {
    if (!img || img._revealed) return;
    var show = function () {
      if (img._revealed) return;
      img._revealed = true;
      var doReveal = function () {
        img.style.visibility = '';          // visibility 无过渡，这一步就是"露面"本身
        img.classList.add('loaded');
        if (img._dropSpin) img._dropSpin(); // 露面与撤转圈同一处，中间不留空窗
      };
      // 1.8.151：若转圈**已经显示出来了**，就等它停够最短时长再一并结束。
      //
      // 为什么需要：转圈的出现有 150ms 延迟（秒开的图根本不显示圈），
      // 但"圈刚显示、图紧接着就好了"会让圈**瞬间冒一下又消失** —— 比不显示圈更刺眼。
      // 反过来，如果只是把圈的消失延后（1.8.150 之前那种做法），
      // 又会出现"图已经完整画出来、圈还压在图上转"。
      //
      // 所以正确的解法是**让图等圈**：拿到"圈还需停多久"，等够了一起结束。
      var wait = img._spinWait ? img._spinWait() : 0;
      if (wait > 0) setTimeout(doReveal, wait); else doReveal();
    };
    if (img.decode) {
      img.decode().then(show, show);      // 解码失败也要露面，否则永远空白
    } else if (img.complete && img.naturalWidth > 0) {
      show();
    } else {
      // 数据还没到：等 load；**兜底计时也从这一刻之后才起算**
      img.addEventListener('load', function () {
        if (img._revealTimer === undefined) {
          img._revealTimer = setTimeout(show, VIEWER_REVEAL_TIMEOUT);
        }
      }, { once: true });
    }
    // 数据已到（或已在缓存里）→ 立刻开始兜底计时，防止解码卡死时永远不露面
    if (img.complete && img._revealTimer === undefined) {
      img._revealTimer = setTimeout(show, VIEWER_REVEAL_TIMEOUT);
    }
  }

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
      // 1.8.149：转圈**不再只给 isCurrent**。
      //
      // 原因：切换时目标图是 `isCurrent:false`（它原本只是"相邻的预载盒"），
      // 所以它**没有转圈** —— 于是"位移过去之后等解码"的那段时间里画面是空的，
      // 只能靠"根本没移过去"来遮丑，那正是"不跟手"的来源。
      // 现在每个盒都带转圈：位移立即发生时，目标位置立刻有一个可见的指示，
      // 解码完成后 reveal 撤掉它。轨道外的盒子虽有转圈但看不见，无副作用。
      var spin = null, spinTimer = null, spinShownAt = 0;
      if (true) {
        spin = document.createElement('div');
        spin.className = 'v-spin-box';
        spin.innerHTML = '<div class="v-spin"></div>';
        box.appendChild(spin);
        spinTimer = setTimeout(function () {
          spinShownAt = Date.now();
          spin.classList.add('show');
        }, VIEWER_SPIN_DELAY);
      }
      // 1.8.151：恢复「转圈最短停留」，但**不是**让圈压在图上多转 ——
      // 而是把这个时长交给 revealImage，让它**等够了一起结束**（见那边的说明）。
      var spinWait = function () {
        if (!spinShownAt) return 0; // 圈还没显示过 → 不用等，立即露面（秒开的图零延迟）
        return Math.max(0, VIEWER_SPIN_MIN - (Date.now() - spinShownAt));
      };
      var dropSpin = function () {
        if (!spin) return;
        clearTimeout(spinTimer);
        spin.classList.remove('show');
        setTimeout(function () { if (spin.parentNode) spin.parentNode.removeChild(spin); }, 220);
      };

      var img = document.createElement('img');
      img.alt = item.name || '';
      img.decoding = 'async';
      img.fetchPriority = 'high'; // 主图优先，别被后台预加载挤到后面
      // 1.8.150：**所有**图片盒先不露面（原来只给 isCurrent —— 那正是"切换必闪"的根因）。
      // 切换要显示的那张是 isCurrent:false 的相邻盒，它若立即可见，
      // 浏览器就会边解码边逐层绘制，用户看到"从上往下刷出来"。
      img.style.visibility = 'hidden';
      img._dropSpin = dropSpin;   // 露面时由 revealImage 一并撤掉转圈
      img._spinWait = spinWait;   // 露面时机由它决定：圈已显示就等够最短时长，一起结束
      img.onload = function () { revealImage(img); };
      img.onerror = function () { revealImage(img); toast('无法加载图片'); };
      var md = pickMaxdim();
      img._maxdim = md; // 记住这张图按哪个预算取的，供视口变大后重取判断
      img.src = rawURL(item.path, md, item.mtime);
      // ⚡ 命中缓存时 onload 可能永远不会再来：同一个 URL 的位图已经在内存里，
      // 赋完 src 立即 complete 为真，浏览器不会再派发 load 事件 —— 那样 reveal()
      // 永远不执行，元素就一直停在 visibility:hidden（只有 2500ms 兜底能救）。
      // 表现正是「计数在加、图片却不切换」。所以这里赋值后立刻补一次检查。
      if (img.complete && img.naturalWidth > 0) { revealImage(img); }
      // 兜底计时由 revealImage 内部从「数据到位」之后起算，这里不再单独挂
      box.appendChild(img);
      box._media = img;
      attachImageGestures(img);
    }
    return box;
  }

  var counterTimer = null;
  // 兜底：确保「画面上这张」就是「计数指向的那张」。
  //
  // 为什么需要：连点时前一轮的延迟回调（等 decode 的 doIt、动画的 settle、400ms 兜底）
  // 可能在后面才落地，把已经跳好的轨道又改回旧图 —— 现象就是「计数一路走到 6，
  // 图却停在第二张」。这里不去猜是哪条回调，而是每次收尾都**校验并纠正**：
  //   ① 轨道中间那格不是当前图 → 重建轨道
  //   ② 当前图的 <img> 还停在 hidden（缓存命中时 onload 不再触发）→ 让它露面
  function ensureMediaVisible() {
    var track = vWrap.querySelector('.v-track');
    if (track && v.list.length) {
      var want = v.list[v.index];
      var mid = track.children.length > 1 ? track.children[1] : null;
      if (want && (!mid || !mid._itemPath || normPath(mid._itemPath) !== normPath(want.path))) {
        syncTrackToIndex(track); // 轨道错位 → 按当前索引重建
      }
    }
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    // 1.8.150：这里**只交给同一把闸门**放行，不再自己清 visibility。
    //
    // 原来写的是 `if (m.complete && m.naturalWidth > 0) { m.style.visibility=''; ... }` ——
    // `complete` 只代表数据下载完、不代表位图解码完，于是这条"收尾自检"把闸门整个旁路掉：
    // 切换后数据一到位（约 572KB，传得很快）它就把图放出来，而 1 亿像素解码还要约 1 秒 ——
    // 那 1 秒里用户看到的就是"图片从上往下刷出来"。
    if (!m._revealed) revealImage(m);
  }


  function flashCounter() {
    vCounterFloat.textContent = (v.index + 1) + ' / ' + v.list.length;
    vCounterFloat.style.opacity = '1';
    clearTimeout(counterTimer);
    counterTimer = setTimeout(function () {
      vCounterFloat.style.opacity = '.6';
    }, 1500);
  }

  // SWIPE_LOCK_SCALE：放大到这个倍数以上时，切换前先归位（见 swipeTo 的说明）。
  // 2 是"看得出在放大、但还没到逐像素看细节"的分界（1.8.153：1.5 → 2）。
  //
  // 抬到 2 的取舍：1.5~2x 这段里**第一次点切换就直接切走**，不再"先归位"，
  // 换来的是"刚放大一点点就被挡住、要点两次"的情况更少。
  var SWIPE_LOCK_SCALE = 2;

  // 放大上限。原来是散在滚轮处理器里的字面量 6，触屏捏合也要用同一个上限 ——
  // 抽成常量，避免"改一处漏一处"（两处上限不一致会出现滚轮能放大到 8、捏合只能到 6）。
  var MAX_SCALE = 6;

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

  // ③-3：只给**程序化**的 scale 变化加过渡；手势（拖拽/滚轮）期间必须关掉，
  // 否则拖拽会"发飘"（transform 追着指针跑，有 .22s 延迟）。
  function animateTransform(on) {
    if (!v.media) return;
    v.media.style.transition = on ? 'transform .22s cubic-bezier(.22,.61,.36,1)' : '';
    if (on) {
      clearTimeout(v._animT);
      v._animT = setTimeout(function () { animateTransform(false); }, 260);
    }
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
          // 1.8.156：**保证"放大过就一定拖得动"**。
          //
          // 上面按"图片是否真的溢出容器"算可拖范围 —— 逻辑本身没问题（图没溢出时拖动
          // 确实没意义）。但实测：**竖图在放大两倍附近经常恰好不溢出**
          // （高度刚好多出一点点，或受 .v-vbox 的尺寸约束而没有超出容器），
          // 于是可拖范围算成 0 → 拖动**完全没有响应** → 用户以为"拖动坏了"。
          //
          // 所以给一个下限：只要真的放大过，至少允许拖动"自身被放大出来的那一段"的一半。
          // 这样图没溢出时也不是纹丝不动，而是能轻微跟手，视觉上不会露背景。
          if (v.scale > 1.01) {
            if (maxTx <= 0) { maxTx = Math.max(8, (dispW * v.scale - dispW) / 2); }
            if (maxTy <= 0) { maxTy = Math.max(8, (dispH * v.scale - dispH) / 2); }
          }
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
    // A1a：判据改用 _maxdim 标记。
    // 原来靠"src 里没有 maxdim"判断"已是原图"，一旦升级目标带上 maxdim，
    // 这条会永远为真 → 每次放大都重复升级、重复转码。
    // Infinity = 已经是原图，没有更高的了。
    if (!isFinite(m._maxdim)) return;
    var item = v.list[v.index];
    if (!item || !item.path) return;
    // A1a 分级升级：
    //   第一级 → 最高预览档 PICK_MAX（4096，约 1MB，2 秒内可见）
    //   第二级 → 原图（54MB，后台自动换；此时已有清晰图在看，用户无感等待）
    // 这样"放大立刻清晰"与"最终看到原图画质"两者都保住。
    // P1-2：用**显式阶段标记**，不要拿档位比较。
    //   原来的 `!(m._maxdim > 0 && m._maxdim < PICK_MAX)` 在"基础档位恰好 = PICK_MAX"时
    //   （大屏 / 高 dpr / 全屏后 pickMaxdim() 返回 4096）会算出 true
    //   → **直接取 54MB 原图**，分级升级完全落空。
    //   _upgradeStage: 0 = 仅预览档，1 = 已切 PICK_MAX，2 = 已是原图
    if (m._upgradeStage === undefined) m._upgradeStage = (m._maxdim >= PICK_MAX) ? 1 : 0;
    // ③-2：**1:1 直接取原图**（按用户明确要求）。
    //
    //   为什么不做中间档：**每换一次源（3072 → 4096 → 原图）就是一次重绘**，
    //   而大图重绘会肉眼可见地闪一下。1:1 的目标本来就是原图，
    //   所以中间档在这里只贡献"多闪一次"，不贡献画质（4096 在 8.87x 放大下与 3072 同样糊）。
    //   → **换源次数最少的方案，就是最不闪的方案。**
    //
    //   scale 已由 `_actualScaleLocked`（一次算到位）固定，换源时尺寸不变，
    //   所以用户看到的是"同一位置变清晰"，而不是尺寸跳变。
    //
    //   ⚠ 1.8.138 曾写成 `m._upgradeStage = 2` 再接 `return` —— 那等于
    //   **既不取 4096、也不取原图**（stage 已 >= 2 直接返回），必须设为 1。
    if (force && v.actual) m._upgradeStage = 1;
    if (m._upgradeStage >= 2) return;
    var upgradingToFull = (m._upgradeStage === 1);
    var url = upgradingToFull
      ? rawURL(item.path, 0, item.mtime)              // 第二级：原图（画质）
      : rawURL(item.path, PICK_MAX, item.mtime);      // 第一级：4096 预览档（快速可见）
    m._upgradeStage = upgradingToFull ? 2 : 1;
    m._maxdim = upgradingToFull ? Infinity : PICK_MAX;
    if (m.getAttribute('src') === url) return;
    v.hiSrc = url;
    var probe = new Image();
    var apply = function () {
      if (v.hiSrc !== url || v.media !== m) return;
      // 1:1 按钮时趁 probe 持有原图尺寸先摆正倍数
      // ③-1：只有拿不到 item.w（列表没给尺寸）时才用真实宽度兜底重算；
      // 正常情况下 scale 在点击那一刻就已经算到位了，这里不再重算 → 不再跳变。
      if (v.actual && probe.naturalWidth > 0 && !v._actualScaleLocked) {
        animateTransform(true);
        applyActualScale(probe.naturalWidth);
        applyTransform();
      }
      m.src = url;
      // A1a：第一级（4096）已显示 → 后台自动继续换原图（用户此刻已有清晰图在看）
      if (!upgradingToFull && v.scale >= 1.5) maybeUpgradeToFull(true);
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
    var url = rawURL(item.path, want, item.mtime);
    if (m.getAttribute('src') === url) return;
    if (v.hiSrc && v.hiSrc !== m.getAttribute('src')) return; // 已有一次切换在途
    v.hiSrc = url;
    var probe = new Image();
    var apply = function () {
      if (v.hiSrc !== url || v.media !== m) return;
      m._maxdim = want;
    // ④F：两个字段表达同一件事（"当前档位"），必须同步 ——
    //   否则"3072 打开 → 全屏（重取到 4096）→ 放大"时 stage 还是 0，
    //   第一次放大算出 url == 当前 src，空转一次才升原图。
    if (want >= PICK_MAX && (m._upgradeStage === undefined || m._upgradeStage < 1)) {
      m._upgradeStage = 1;
    }
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
      // 2.3.0：**以双击点为中心**放大（原来只是居中放大到 2×）。
      //   做法：把"双击点相对图片中心"的偏移取负作为平移量，双击点在屏幕上保持不动。
      if (v.zoomed) {
        resetView();
        applyTransform();
        return;
      }
      var rect = this.getBoundingClientRect();
      var cx = e.clientX - (rect.left + rect.width / 2);
      var cy = e.clientY - (rect.top + rect.height / 2);
      // 2.4.0：双击放大倍数按用户要求 2 → **3 倍**（再双击还原）。
      v.scale = 3; v.zoomed = true; v.actual = false;
      v.tx = -cx; v.ty = -cy;
      updateFitBtn();
      applyTransform();
      maybeUpgradeToFull();
    });
    img.addEventListener('wheel', function (e) {
      e.preventDefault();
      var factor = e.deltaY < 0 ? 1.15 : 0.87;
      v.actual = false; // 手动缩放后不再是「原始大小」
      updateFitBtn();
      // 放大上限 6x（MAX_SCALE）。超过 1.5x 后 maybeUpgradeToFull 会直接升级原图，
      // 之后继续放大是原图 1:1 或插值，不会糊。
      v.scale = Math.min(MAX_SCALE, Math.max(1, v.scale * factor));
      v.zoomed = v.scale > 1.01;
      if (!v.zoomed) { v.tx = 0; v.ty = 0; }
      applyTransform();
      maybeUpgradeToFull();
    }, { passive: false });
  }

  // ---------- 跟手滑动切换（手机相册式） ----------
  // 2.3.3/2.3.4：边缘单击的延迟判定窗（毫秒）—— **仅手机版**使用（见使用处）。
  //   为什么必须有它：要做到"双击零闪烁"，就不能在第一下就切图 —— 只能先等一小会儿，
  //   若这段时间内来了第二下（双击），直接取消 ⇒ 从头到尾没切过。
  //   中间 1/3 不切图，所以不需要延迟、仍是零延迟。
  var MV_TAP_DELAY_MS = 220;

  // 2.3.2：双击判定窗（毫秒）。两次 tap 间隔小于它就认为是双击的第二下。
  //   用时间窗而不是"延时 250ms 再切图" —— 那样单击切图会慢一拍，用户明确不要延迟。
  var MV_DBL_TAP_MS = 300;
  // 2.3.2：上一次 tap 的状态 —— 用来判断"这是不是双击的第二下"，以及该把图换回哪一张。
  //   undo: 0 = 没切过；1 = 上一步切到了上一张；-1 = 切到了下一张。
  //   at  : 上一次 tap 的时间戳。
  //   刻意用**对象**而不是两个裸变量：它俩天生是一对，绑在一起语义更清楚。
  var mvTap = { undo: 0, at: 0, timer: null };

  var swipe = {
    active: false, startX: 0, startY: 0, dx: 0, dy: 0,
    locked: false, track: null,
    lastX: 0, lastTime: 0, velocity: 0
  };
  // 切换序号：每次发起切换自增，用来作废「过期的延迟回调」
  // （等目标图 decode 的 doIt、settleAfterSwipe 的 transitionend 与 400ms 兜底）。
  var swipeSeq = 0;

  // ---------- 触屏手势让位（多指 / 被 touch.js 接管）----------
  // 触屏上有些手势必须由 touch.js 独占（多指捏合、右边缘横滑返回），此时 app.js
  // 原有的单指逻辑（滑动切图 / 点击切图 / 单指平移）要**整体让位**。不让位会出两个真 bug：
  //   ① 两指捏合时 pointerup 是**逐指触发**的，第一根手指抬起时位移≈0
  //      → 走 endSwipe 的"点击"分支 → **捏合会顺手翻页**；
  //   ② 右边缘横滑的横向拖动会被当成"滑动切图"，松手就把图切走了。
  // 桌面：touch.js 从不加载，且鼠标只有一个 pointerId ⇒ mtCount() 恒为 1
  //      ⇒ tgesture.own 恒为 false ⇒ 下面每个判断都短路，行为与改动前逐字节一致。
  var mtDown = {};                            // pointerId -> 1：当前按在查看器上的触点集合
  var tgesture = { own: false, muteClickUntil: 0 };
  function mtCount() {
    var n = 0;
    for (var k in mtDown) { if (mtDown[k]) n++; }
    return n;
  }
  // 本次手势让位：丢弃 app.js 侧已开始的手势状态（否则轨道会停在半路）
  function tgestureClaim() {
    tgesture.own = true;
    swipe.active = false;
    swipe.track = null;
  }
  // 全部触点抬起：解除让位，并短暂抑制紧随其后的 click。
  // 用**时间窗**而不是布尔标记：布尔标记若没有 click 来清就会残留，
  // 表现为"下一次点击被吞掉"（1.8.160 在这上面踩过一次，见 pointerdown 里的注释）。
  function tgestureRelease() {
    tgesture.own = false;
    tgesture.muteClickUntil = Date.now() + 400;
  }

  animateTransform(false); vWrap.addEventListener('pointerdown', function (e) {
    if (e.target.closest(VIEWER_CTL_SEL)) return;
    // 触屏：登记触点。第二根手指落下 ⇒ 本次手势改判为多指，app.js 整体让位。
    mtDown[e.pointerId] = 1;
    if (mtCount() > 1) { tgestureClaim(); return; }
    if (tgesture.own) return; // 已被 touch.js 接管（右边缘返回起手）
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
    // 这里只记下"按下时的偏移量"作为平移基准，不定方向（平移在 pointermove 里按位移走）。
    swipe.panBase = { x: v.tx, y: v.ty };
    // 1.8.159：每次手势开始都把 horizontal 清掉。
    // swipe.horizontal 是"本次手势要不要走滑动切图"，而它**只在 pointermove 里被赋值**；
    // 若用户在未放大时滑动过（horizontal=true），随后放大再**只点击不移动**，
    // pointermove 可能一次都不进来 → 残留的 true 会让 endSwipe 走进切图收尾分支。
    // 清掉它，放大状态下的"点击切图"就只由 vStage 的 click 处理器负责，路径唯一。
    swipe.horizontal = false;
    // 1.8.160：**每次手势开始都把 suppressClick 清掉**。
    //
    // suppressClick 是"这次已按点击处理过、别再让 vStage 的 click 又切一次"的标记：
    // 由 endSwipe 在 pointerup 里置 true，只在 vStage 的 click 里清 false。
    // 而 click **只在"按下与松开落在同一元素"时才会触发** —— 若用户按在图上、
    // 拖到 vStage 外面再松手，就没有 click 来清它 → 标记残留 true →
    // **下一次真点击会在第一行被 return 掉**，表现为偶发"点一下没反应，再点一下才行"。
    // 在 pointerdown 里清是安全的：置 true 发生在 pointerup，一定晚于这里。
    swipe.suppressClick = false;
    if (v.scale > 1.01) return; // 已放大：一律平移，不需要准备轨道（见 pointermove 的说明）
    var track = vWrap.querySelector('.v-track');
    if (track) {
      track.classList.remove('anim');
      track.style.transitionDuration = '';
      swipe.track = track;
    }
  });

  vWrap.addEventListener('pointermove', function (e) {
    if (tgesture.own) return; // 多指 / 已被 touch.js 接管：本次手势不参与
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

    // 1.8.159：**放大后一律平移，且零死区**；放大状态下不存在"滑动切图"。
    //
    // 交互规范（2026-10-07 定稿，按倍数分三档）：
    //   · ≤ 1.01（未放大）：水平拖动 = 跟手切图（保持不变）
    //   · 1.01 ~ 2 倍    ：任意方向拖动 = 平移图片；**不允许滑动切图**，
    //                      切图只能靠"点击左/右半区"（见 vStage 的 click 处理器）
    //   · ≥ 2 倍         ：任意方向拖动 = 平移图片；点击也不切图
    //
    // 四个版本的正反教训（别再把某条路接回来）：
    //   · 1.8.154 按方向分流（垂直=平移 / 水平=切图）→ 拖动与点击搅和；
    //   · 1.8.155 为治搅和改成"放大后一律平移" → 搅和好了，但 pointermove 里
    //     "不足 8px 就 return" 造成起步死区（不跟手）；
    //   · 1.8.157/158 为消死区，把"水平 + 两倍以内"又接回滑动切图 → 死区没了，
    //     但**放大后又能左右滑动切图了**（用户不要这个）；
    //   · 本版：保留 158 的"第 1 个像素就平移"（零死区），**去掉方向分流与回滚** ——
    //     "拖动"与"点击"彻底分开：拖动只平移，切图只认点击。
    if (v.scale > 1.01) {
      var base = swipe.panBase || { x: 0, y: 0 };
      // 明确告诉 endSwipe："本次手势不是滑动切图"，别去跑切图收尾（它会据此提前返回）
      swipe.horizontal = false;
      // 零死区：方位未定也立即跟手（不再有"位移不足 8px 就 return"）
      v.tx = base.x + swipe.dx;
      v.ty = base.y + swipe.dy;
      applyTransform();
      return;
    }
    if (!swipe.locked) {
      // 未放大：锁定方向后水平跟手滑动（原逻辑，不变）
      if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) {
        swipe.locked = true;
        swipe.horizontal = Math.abs(swipe.dx) > Math.abs(swipe.dy);
      } else {
        return;
      }
    } else if (!swipe.horizontal) {
      return;
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
    // 1.8.159：滑动切图**只可能在未放大时发生** ——
    //   · 放大分支已在 pointermove 里置 `swipe.horizontal = false` 并 return；
    //   · 下面两道守卫都是兜底：到阈值以上不切；没有"水平滑动 + 轨道"也不切。
    // 放大状态下的切图走 vStage 的 click 处理器（点击左右半区，限 2 倍以内）。
    if (v.scale >= SWIPE_LOCK_SCALE) return;
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
      warmTrackBoxes(typeof dir === 'number' ? dir : 1);
      settleAfterSwipe(track, -1);
    } else if (dir === 1) {
      track.style.transform = 'translateX(-200%)';
      v.index++;
      warmTrackBoxes(typeof dir === 'number' ? dir : 1);
      settleAfterSwipe(track, 1);
    } else {
      track.style.transform = 'translateX(-100%)';
      // 位移没到切换阈值 —— 但用户很可能只是想「点一下」。
      // 问题在于 vStage 的 click 有一条守卫：
      //     if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) return;
      // 手抖超过 8px 时这次点击就被整个吞掉，而这里（dir === 0）又什么都不做 ——
      // 两边都不处理，点击就丢了，用户只能再点一次。
      // 所以在 pointerup 就按点击处理，用按下的位置决定上一张 / 下一张。
      // 1.8.155：判据加**距离守卫** —— 只有"几乎没动"才算点击。
      //
      // 原来这里没有任何距离判定，于是"拖着看图、松手"也会被当成点击把图切走；
      // 更糟的是它随后设了 suppressClick = true，**把 vStage click 里那道
      // `dx > 8 → return` 的守卫也一并抑制掉了** —— 两边都不拦，"拖动"就变成了"切图"。
      // 8px 与 vStage click 的守卫保持一致。
      // 2.3.3：**这里不再切图**。
      //   原来它在 pointerup 就按三份判定切图/撤销，于是「第一下已切、第二下再撤回」——
      //   用户能看到「切过去→瞬间切回→放大」的闪烁。
      //   现在切图**统一由 vStage 的 click 负责**（带 220ms 判定窗，双击直接取消），
      //   这里只做滑动收尾，不再碰索引。
    }
    swipe.track = null;
  }
  // 收尾守卫：让位期间**不跑** endSwipe（否则多指手势会被当成"点击"翻页/收尾动画）；
  // 触点全部抬起后才解除让位。鼠标只有一个 pointerId ⇒ 分支与原来完全等价。
  function endSwipeGuarded(e) {
    var yielding = tgesture.own;
    delete mtDown[e.pointerId];
    if (yielding) {
      if (mtCount() === 0) tgestureRelease();
      return;
    }
    endSwipe();
  }
  vWrap.addEventListener('pointerup', endSwipeGuarded);
  vWrap.addEventListener('pointercancel', endSwipeGuarded);

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

      // 不再手工增删子元素，改为按当前索引**幂等重排**轨道。
      // 手工增删隐含「dir 与 v.index 严格对应」这一假设，连点、动画被取消、
      // 旧回调晚到时都会错位（现象就是"计数在走、画面不动"或停在旧图）。
      // syncTrackToIndex 完全由 v.index 派生、且复用已在 DOM 里的元素，
      // 重复调用没有副作用 —— 于是任何一条延迟回调落地都只会把轨道校正回正确状态。
      syncTrackToIndex(track);

      // 更新 v.media 指向中间的当前图
      var mid = track.children[Math.min(1, track.children.length - 1)];
      if (mid && mid._media) {
        v.media = mid._media;
        if (v.media.tagName === 'VIDEO') v.media.play().catch(function () {});
      }
      // 非当前媒体一律暂停（它们仍留在轨道里，随时可能被切到 —— 所以只暂停、不断源）
      for (var i = 0; i < track.children.length; i++) {
        var m = track.children[i]._media;
        if (m && m !== v.media) pauseMedia(m);
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
    ensureMediaVisible();   // 收尾自检：轨道错位就重建，图没露面就显示
    };
    // 1.8.149：没有过渡时**立即收尾**，不再等 400ms。
    //
    // 背景：anim-none 下 .v-track 的 transition 被 !important 关掉，
    // transitionend **永远不会触发** → 每次切换都吃满这 400ms 兜底，
    // 期间 resetView / flashCounter / updateWindowTitle / ensureMediaVisible 都没跑，
    // 表现为「点了没反应」「滚轮作用在上一张」。
    var noAnim = viewer.classList.contains('anim-none') ||
                 track.style.transitionDuration === '0s' ||
                 track.style.transitionDuration === '';
    if (noAnim) {
      handler();
    } else {
      track.addEventListener('transitionend', handler);
      // 兜底：transitionend 可能因属性变化不触发
      timer = setTimeout(handler, 400);
    }
  }

  // 静默跳到目标图（无动画）：取消在跑的动画/待定的解码，直接换图。
  // 注意：这里**不再推进 v.index** —— 调用方（swipeTo / 手势）已经推进过了。
  // 早先两处都推进，快速路径上会「一次点击跳两张」。
  function jumpToIndex(track, dir) {
    track.classList.remove('anim');
    track.style.transitionDuration = '';
    if (v.index < 0) v.index = 0;
    if (v.index > v.list.length - 1) v.index = v.list.length - 1;
    syncTrackToIndex(track);
    warmTrackBoxes(typeof dir === 'number' ? dir : 1);
    flashCounter();
    updateWindowTitle(); // 连点切换同样要更新标题栏
    var item = v.list[v.index];
    var vBtnDisp = item && item.kind === 'video' ? 'none' : '';
    $('vFit').style.display = vBtnDisp;
    if ($('vActual')) $('vActual').style.display = vBtnDisp;
    // 兜底：把轨道校正成「中间 = 当前索引」，并确保当前图露面
    ensureMediaVisible();
  }

  // 点击左右区域 / 键盘切换：用滑动动画
  function swipeTo(dir) {
    if (dir < 0 && v.index === 0) return;
    if (dir > 0 && v.index === v.list.length - 1) return;
    // 1.8.152：判据从「是否放大过」改成「放大倍数」。
    //
    // 原来用 `if (v.zoomed)` —— 而 v.zoomed 只要 scale > 1.01 就为真
    // （applyActualScale 里就是这么算的），于是**点过一次 1:1 之后，
    // 这张图（以及后续继承了这个状态的图）就再也切不动了**。
    //
    // 现在按倍数判断，与用户的心理预期一致：
    //   · 放大不到 2 倍 → 直接切图（轻微缩放不该挡住切换）；
    //   · 放大到 2 倍及以上 → 先把视图归位（第一次点击只归位，第二次才切），
    //     避免"正在看细节时被切走、回来还要重新找位置"。
    // 2.4.0：**归位不再放在这里**。
    //
    // 原来这里有 `if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); ...; return; }`，
    // 于是"放大后点左/右 1/3"虽然已经从 click 里删掉了归位，走到这（prev/next → swipeTo）
    // **又被归位了一次** —— 用户看到的就是"点了左右侧还是返回原样、而且没切图"。
    // 现在归位只留在**键盘 ←/→** 那一处（点击直接切图，这是用户明确要求的）。
    var track = vWrap.querySelector('.v-track');
    if (!track) { v.index += dir; showCurrent(); return; }
    var mySeq = ++swipeSeq; // 本次切换的序号（见 doIt / settleAfterSwipe 的过期判定）

    // ★ 逻辑立即生效：索引与计数先走一步，画面稍后跟上。
    // 旧实现把 v.index 的推进放在 doSwipe（要等目标图 decode 完），等待期间
    // 计数/标题都不变 —— 用户以为点击没生效，于是再点一次，第二次才命中快速
    // 路径切过去。这就是「要点两次才能切下一张」的真正原因。
    // 现在：每次点击都立即推进索引（有反馈、绝不丢失），只把**画面更新**推迟到
    // 目标图解码完成（期间保持当前画面，所以不会闪屏）。
    v.index += dir;
    if (v.index < 0) v.index = 0;
    if (v.index > v.list.length - 1) v.index = v.list.length - 1;
    flashCounter();
    updateWindowTitle();

    // 动画期间，**或上一轮还在等目标图解码**时再次切换 → 直接静默跳到目标图。
    // 关键是不能只把旧轮次作废就算完：等解码期间 v.index 还没动，
    // 新轮次算出来的目标图与旧轮次是同一张，于是「点两次只前进一张」，
    // 连点几下切换顺序就全乱了（用户报的「一按切换就乱套」）。
    if (track.classList.contains('anim') || v.pendingSwipe) {
      v.pendingSwipe = false;
      jumpToIndex(track, dir);
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
      // 标记「有切换正在等解码」：这期间的点击由上面的 jumpToIndex 立即兑现，
      // 不会被静默吞掉。
      v.pendingSwipe = true;
      var doIt = function () {
        if (fired) return;
        fired = true;
        // 期间若又来了新的切换请求，本次已过期：直接作废（新轮次自己负责收尾，
        // 所以这里不要清 pendingSwipe）。
        // 否则两个 doIt 各执行一次 doSwipe → v.index 走两格而轨道只前进一格，
        // 显示的图、计数、标题、1:1 升级目标全部错位。
        if (mySeq !== swipeSeq) return;
        v.pendingSwipe = false;
        doSwipe(track, dir);
      };
      // 1.8.149：**位移立即发生**，不再等 decode。
      //
      // 为什么：原来这里等 decode（≤1500ms）才位移，那 1.5 秒里画面纹丝不动 ——
      // 那正是"不跟手"的真正来源（用户会觉得"点了没反应"）。
      //
      // 而"图还没解码好"这件事，改由**目标盒子自己的转圈**来表达：
      // 位移过去之后，中央是一个转圈（既不是空白、也不是上一张的残留），
      // 位图一旦就绪，img 的 reveal 会撤掉转圈、显示图片。
      // 于是三个目标同时满足：
      //   · 跟手  —— 点下去画面立刻朝目标方向移动；
      //   · 不闪  —— 位移后看到的是转圈，不会先出现半解码的糊图；
      //   · 有指示 —— 转圈在**你正看着的中央**，而不是屏幕外的盒子里。
      doIt();
      // decode() 只是让位图提前进入可绘制状态；成功失败都无需额外处理
      // （img 的 load/error 会走 reveal，撤掉转圈）。
      if (targetImg.decode) targetImg.decode().catch(function () {});
      return;
    }

    doSwipe(track, dir);
  }

  // 实际执行滑动动画（目标图已确认加载完成）
  function doSwipe(track, dir) {
    // 注意：v.index **不在这里**推进 —— 它已经在 swipeTo 入口推进过了
    // （那样点击才能立即生效）。这里若再改一次会导致「一次点击跳两张」。
    track.style.transitionDuration = '220ms';
    track.classList.add('anim');
    if (dir < 0) { track.style.transform = 'translateX(0)'; }
    else { track.style.transform = 'translateX(-200%)'; }
    // 方案二：动画开始就预加载新 next，不等 transitionend
    warmTrackBoxes(typeof dir === 'number' ? dir : 1);
    settleAfterSwipe(track, dir);
  }
  function next() { swipeTo(1); }
  function prev() { swipeTo(-1); }

  function closeViewer() {
    showListHint(false); // 2.0.0：关掉查看器时顺手收起可能还挂着的加载提示
    // 恢复网格：把查看器期间变成占位图的缩略图重新请求回来。
    //
    // 1.8.175：**必须先让后端恢复，再重发** —— 顺序反了会拿到占位图，
    // <img> 认为"加载完成"，那一片缩略图就永远白着（用户报的「返回网格全变空白」）。
    // 所以这里不再同步调用 reloadVisibleThumbs()，而是走 resumeThumbGenNow 的回调。
    resumeThumbGenNow(function () { reloadVisibleThumbs(); });
    viewer.classList.add('hidden');
    // ★ 丢弃之前先停机：轨道里可能有正在播放的视频。
    //   虽然 innerHTML='' 移除元素时浏览器会自动暂停，但这里显式停掉 ——
    //   不依赖浏览器实现细节，同时顺手释放解码器（关掉查看器后不该再占 CPU/GPU）。
    stopAllMediaIn(vWrap);
    vWrap.innerHTML = ''; // 轨道连同里面的 <img> 一起销毁
    v.media = null;       // 元素已销毁，v.media 不该再指向失效对象
    // 2.3.0（P2）：把解码引用一并清掉 —— 手机连开十几张大图后不容易被系统杀。
    v.hiSrc = '';
    // 2.3.0：**主动关闭**（点关闭按钮 / 点背景 / Esc）时补一次历史回退。
    //
    // 为什么：打开查看器时我们压了一条 {mvViewer:1}（见 openViewerOverlay）。
    //   · 若是"返回"触发的关闭（mvViewerFromPop=true）⇒ 历史已经退了，这里不能再动；
    //   · 若是**主动关闭** ⇒ 那条历史还在栈顶，不补一次 back 的话，
    //     用户下一次按返回会**直接退掉一层目录**（多退一层）。
    //   ⇒ 一句话：**我们压的条目，只有我们自己收得回来。**
    if (PROFILE === 'touch' && !mvViewerFromPop && !singleMode) {
      try {
        if (history.state && history.state.mvViewer) {
          mvSelfBack = true;   // 让紧接着的 mvOnPop 知道"这次是我们自己退的"
          history.back();
        }
      } catch (e) {}
    }
    // 1.8.160：轨道没了，但视图状态（v.scale / v.zoomed / v.tx / v.ty）是挂在 v 上的、
    // 不会被这一步清掉。下次打开若走 rebuildTrackInPlace 由它重置；**在这里也重置一次**，
    // 是为了不依赖"下次一定走哪条分支"——状态与 DOM 同生共死，语义更清楚。
    resetView();
    resumeGridRequests(); // 恢复被挂起的网格缩略图请求
    if (singleMode) {
      // 单文件模式（独立窗口）：关闭整个窗口。
      // 必须在 hostCall('close') **之前**立即发 resume —— hostCall 会立刻销毁窗口，
      // 写在它后面的延迟恢复根本不会执行。
      flushResumeThumbGen();
      hostLog('single mode: ask host to close viewer window');
      hostCall('close').then(function () {
        window.close();
      }).catch(function () {
        window.close();
      });
      return;
    }
    $('topbar').style.display = '';
    $('main').style.display = '';
    // 注意：后台恢复已经在函数开头（resumeThumbGenNow）做过了 ——
    // 这里不能再调 resumeThumbGen()，那会**取消并重排**一个 1 秒后的 resume，
    // 白白把"已恢复"拖成"1 秒后才恢复"。
  
    // 2.4.3：关掉查看器后**重新判定**宿主闸门 —— 若此时正停在聚合根层，
    //   闸门要重新挂上（打开查看器时清过一次）。旧版 2.2.0 也有这一处。
    try { syncExitTips(); } catch (e) {}
  }

  // ================= 触屏模块接口（仅触屏档位暴露）=================
  // touch.js 用它做**双指捏合缩放**，而不是自己去猜 DOM 结构或复制一份状态。
  // （2.2.0 起本接口只服务捏合：返回手势已整个删除，相关出口一并撤掉。）
  // 桌面（PROFILE !== 'touch'）**不创建这个对象** ⇒ 桌面连入口都没有
  //（zzz_2100 断言"桌面档 window.__mvMobile === undefined"）。
  // 这是"不与电脑搅和"的第三层隔离：契约收敛到这一个出口，不散落 isMobile 判断。
  if (PROFILE === 'touch') {
    window.__mvMobile = {
      version: '__MV_VERSION__',
      // --- 查询 ---
      viewerOpen: function () { return !viewer.classList.contains('hidden'); },
      stageRect: function () {
        var s = $('vStage');
        if (!s) return null;
        var r = s.getBoundingClientRect();
        return { left: r.left, top: r.top, width: r.width, height: r.height };
      },
      view: function () { return { scale: v.scale, tx: v.tx, ty: v.ty }; },
      maxScale: function () { return MAX_SCALE; },
      // --- 让位（多指捏合 / 边缘返回期间，app.js 的单指逻辑整体退出）---
      claimGesture: tgestureClaim,
      releaseGesture: tgestureRelease,
      // --- 动作 ---
      // 手势专用：**必须关掉过渡**，否则 transform 追着手指跑、手感发飘
      //（与滚轮/拖拽同一原因，见 animateTransform 的注释）。
      // 返回 false 表示"这张不能缩放"（视频），调用方据此不发手势效果。
      setView: function (scale, tx, ty) {
        if (!v.media || v.media.tagName !== 'IMG') return false;
        var s = Math.max(1, Math.min(MAX_SCALE, scale));
        v.actual = false;                       // 手动缩放后不再是「原始大小」
        v.scale = s;
        v.zoomed = s > 1.01;
        v.tx = tx || 0;
        v.ty = ty || 0;
        animateTransform(false);
        updateFitBtn();
        applyTransform();   // 内部负责 s<=1.01 时归中，以及平移边界钳位
        return true;
      },
      // 手势收尾：定稿一次（≥1.5x 触发原图升级，与滚轮/1:1 走同一条链路）
      commitView: function () {
        if (!v.media || v.media.tagName !== 'IMG') return;
        animateTransform(true);
        maybeUpgradeToFull();
      },
      // ⚠️ 这里**刻意没有** closeViewer / goUpLevel（2.2.0 删掉了）。
      //    它们是为"右边缘横滑返回"准备的出口；那条手势已按用户要求整块删除，
      //    返回改走页面内的显式入口（左上角箭头 / 退出确认框），不再需要对手势暴露。
      //    接口越窄，touch.js 能误碰的东西越少 —— 这就是隔离层的意义。
    };
  }

  // 页面被隐藏（切到别的标签/窗口、被宿主遮住、最小化）时，把正在播的视频暂停。
  //
  // 为什么必须单独有这一条：视频**不会因为看不见而停止** —— 实测容器
  // `display:none` / `visibility:hidden` 之后 paused 仍为 false，音频与解码都还在跑。
  // 而"查看器窗口被切到后台/被遮住/最小化"都属于「隐藏而不销毁」，
  // 于是后台一直在响、一直在吃 CPU/GPU。这条兜住这一整类场景。
  //
  // 用 pause（不是 stop）：元素还在轨道里，切回前台后用户点播放就能接着看，
  // 不能在这里把 src 断掉（那会让切回来的那一格变成空白）。
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) pauseAllMediaIn(vWrap);
  });

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

  // 2.0.0：手动执行一次后台预生成（针对当前浏览的目录）。
  // 改走 /api/pregen（manual=true），按当前目录的**显示顺序**入队；
  // 不再用 /api/list?preload=N —— 那条会把顺序绑在后端字节序上（见 postPregen 注释）。
  (function () {
    var btn = $('setPreloadRun');
    if (!btn) return;
    btn.onclick = function () {
      var hint = $('setPreloadRunHint');
      if (!state.path) {
        if (hint) hint.textContent = '请先进入一个目录';
        return;
      }
      var files = state.files || [];
      if (!files.length) {
        if (hint) hint.textContent = '当前目录没有可生成的媒体文件';
        return;
      }
      if (hint) hint.textContent = '正在请求…';
      postPregen(files, true).then(function (res) {
        if (!hint) return;
        if (res && typeof res.enqueued === 'number') {
          hint.textContent = '已请求后台生成（入队 ' + res.enqueued + ' 张，按当前显示顺序）';
        } else {
          hint.textContent = '请求失败';
        }
      });
    };
  })();

  var vActualBtn = $('vActual');
  if (vActualBtn) vActualBtn.onclick = function () {
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    if (v.actual) {
      resetView();
      applyTransform();
    } else {
      v.actual = true;
      // ③-1：1:1 需要多大 scale 只取决于**原图像素宽度**，而这个宽度列表里就有
      //   （item.w，scanner 列目录时已填）。原来退回用"当前预览图的宽度"，
      //   于是只能随着源升级一档一档往上补 → 视觉上的"一节一节"三跳
      //   （3072 → 4096 → 8736，每跳一次 scale）。
      //   用 item.w 一次算到位后，后续换源只是"同一位置变清晰"，不再跳。
      var _actIt = v.list[v.index];
      // ① 优先用 item.w（= 原图像素宽度）。
      //    ⚠ item.w 在 JSON 里是 `omitempty`：读头失败或尺寸为 0 时**整个字段不出现**，
      //    所以这里经常拿不到。此时**不能**用当前 naturalWidth 顶替后就"锁定" ——
      //    那一刻原图还没加载，naturalWidth 只是**预览档**（3072），
      //    算出来的 scale 是"预览档的 1:1"，而且一旦锁定，原图就绪后也不会再修正，
      //    表现就是「点 1:1 放大的不是原图尺寸，得手动放大再点一次才对」。
      var _actW = (_actIt && _actIt.w) ? _actIt.w : 0;
      if (!_actW) ensureItemSize(_actIt); // 万一预取还没回来，补问一次（回来后会在 then 里修正）
      applyActualScale(_actW || (v.media ? v.media.naturalWidth : 0));
      // ★ 只有真拿到「原图宽度」时才锁定；否则留待原图就绪后在 apply 里重算一次。
      v._actualScaleLocked = !!_actW;
      animateTransform(true);      // ③-3：给这一次 scale 变化加过渡
      applyTransform();
      // force=true：1:1 必须用真正的原图，否则"原始大小"只是预缩版的 1:1。
      maybeUpgradeToFull(true);
    }
    updateFitBtn();
  };
  updateFitBtn();

  // 点击左/右半区切换（滑动后不触发，避免误触）
  animateTransform(false); $('vStage').addEventListener('click', function (e) {
    // 刚结束的多指手势（捏合 / 边缘返回）浏览器仍可能补一个 click —— 抑制掉，
    // 否则"捏合松手"会顺手翻一页。时间窗 400ms，过期自动失效（不会残留吞掉下一次点击）。
    if (Date.now() < tgesture.muteClickUntil) return;
    // pointerup 已经按点击处理过（见 endSwipe 的 dir === 0 分支），别再切一次
    if (swipe.suppressClick) { swipe.suppressClick = false; return; }
    if (e.target.closest(VIEWER_CTL_SEL)) return;
    // ★ 顺序要紧：**先判"这是不是一次点击"，再决定 ≥2 倍时归位。**
    //
    // 1.8.161 修：1.8.160 把下面那条 ≥2 倍的 `return` 改成了 `resetView()`，
    // 但**留在了这条距离守卫之前** —— 原来 `return`（什么都不做）放在前面无害，
    // 换成 `resetView()`（有副作用）后就出事了：
    //   放大到 2 倍以上 → 拖动图片 → 松手时浏览器照样发一个 click
    //   → 命中 ≥2 倍分支 → **缩放被重置回"适应窗口"**。
    // 用户报的"图片放大后一拖动就回归窗内大小"就是这条。
    //
    // 所以距离守卫必须在前：拖动过（>8px）就**直接当作拖动**、不进入任何点击逻辑。
    if (Math.abs(swipe.dx) > 8 || Math.abs(swipe.dy) > 8) return;
    // ≥2 倍时**与键盘 ←/→ 保持一致 —— 先归位**（第二次点击才切图；此时已确认是点击）。
    //
    // 原来这里是 `return`（点图什么都不做），而键盘与界面 ❮/❯ 走 swipeTo，那里是
    // `if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }`
    // —— 同一个"切图"动作两个入口表现不同，而"点了一下完全没反应"最容易让人
    // 以为功能坏了，所以统一成"先归位"。
    // 2.3.1：**先分区、再决定要不要归位**。
    //
    // 原来这条 ≥2× 的归位放在分区**之前** ⇒ 放大后点**中间**也会"恢复原状"，
    // 而用户要的是"中间什么都不做"（中间只负责双击缩放）。
    // 现在：中间直接 return；只有左右 1/3 才有"先归位"这一步（与键盘 ←/→ 一致）。
    var rect = this.getBoundingClientRect();
    var x = e.clientX - rect.left;
    var _left = rect.width / 3;
    var _right = rect.width * 2 / 3;
    if (x >= _left && x <= _right) return;   // 中间 1/3：无论缩放，什么都不做
    // 2.3.2：**双击的第二下（detail >= 2）不再切图**，并把第一下切走的索引还原。
    //   否则"在左/右 1/3 双击"= 切一张 + 放大（用户实测报的"边缘双击还会切图然后放大"）。
    //   还原后由 dblclick 负责放大；单击切图仍是零延迟。
    // 2.3.3：双击的第二下 ⇒ **取消**待执行的单击切图（于是全程没有切过图，零闪烁）。
    if (e.detail >= 2) {
      if (mvTap.timer) { clearTimeout(mvTap.timer); mvTap.timer = null; }
      return;
    }
    // 2.3.3：**取消"放大到 2× 以上时单击先归位"**（用户明确要求）。
    //   现在放大后点击直接按分区切图；拖动仍然是平移（pointermove 的放大分支没动）。
    // 2.3.0：**屏幕分三份**（图片与视频统一）。
    //
    //   左 1/3 = 上一张；中间 1/3 = **什么都不做**；右 1/3 = 下一张。
    //
    // 2.3.2：**双击的第二下不切图，并把第一下切走的索引还原**。
    //   中间 1/3 本来就不切图，但左右 1/3 的第一下会立即切（这是"零延迟"的代价）。
    //   若紧接着来了第二下（e.detail >= 2）说明用户其实是在双击，
    //   于是把索引换回来、让 dblclick 去放大 —— 用户看到的就是"双击只放大"。
    //   （不做 250ms 延迟，单击切图仍是零延迟。）
    //
    // 为什么中间必须"什么都不做"：双击的两下 click 都会命中同一个区域。
    //   如果中间也切图，双击就会"先切一张/两张、再放大"（用户实测报的正是这个）。
    //   而把中间留空之后，双击的两下 click 都落在"不切图"区 ⇒
    //   **不需要用 300ms 延迟去区分单击与双击**，切图仍是零延迟。
    //   （这正是 touch.js 文件末尾"加双击必须延迟 300ms"那个死结的解法。）
    // （分区变量在函数开头已经算好：_left / _right。中间 1/3 已在上面 return，
    //   所以能走到这里必然是左右两侧 —— 不要再重复算一遍，那会让"分区"有两个来源。）
    // 2.3.3：**延迟 MV_TAP_DELAY_MS 再切图**。
    //   这段窗口内若来了双击的第二下，上面的 clearTimeout 会把它取消 ⇒ 一次都没切过。
    //   中间 1/3 已在上面 return，所以只有左右边缘会走到这里。
    // 2.3.4：**延迟只对手机版**。
    //   手机：手指双击误触率高，需要 220ms 判定窗来做到"双击零闪烁"。
    //   桌面：鼠标双击精准，而"单击切图"是高频操作，220ms 是能被感知的延迟 ⇒ 立即切。
    // 2.4.1：**手机版放大后不响应左右切图**（用户要求）。
    //   放大是为了看细节 —— 此时点左右应该什么都不做，只能拖动查看；
    //   切图必须先缩回原状（双击还原）再点。桌面版不受影响（鼠标精确，直接切更顺手）。
    if (PROFILE === 'touch' && v.scale > 1.01) return;
    if (PROFILE === 'touch') {
      if (mvTap.timer) { clearTimeout(mvTap.timer); mvTap.timer = null; }   // 连点多次只算最后一次
      mvTap.timer = setTimeout(function () {
        mvTap.timer = null;
        if (x < _left) prev();
        else if (x > _right) next();
      }, MV_TAP_DELAY_MS);
    } else {
      if (x < _left) prev();
      else if (x > _right) next();
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
    // 2.4.0：**归位只在这两个入口做**（键盘离得远、不易误触）。
    //   点击（左/右 1/3）不归位 —— 直接切图，这是用户明确要求的。
    else if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      if (v.scale >= SWIPE_LOCK_SCALE) { resetView(); applyTransform(); return; }
      if (e.key === 'ArrowRight') next(); else prev();
    }
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
    applySort(this.value, true);
    if (state.path) loadDir(state.path);
  };
  $('sortDirBtn').onclick = function () {
    var p = sortParts(state.sort);
    applySort(sortStr(p.field, !p.desc), true);
    if (state.path) loadDir(state.path);
  };
  // 初始化排序控件：让下拉框高亮与升降序按钮图标和 state.sort 保持一致。
  applySort(state.sort, false);
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
  cfg.thumbEngine = s.thumbEngine || 'auto';
  cfg.thumbLowres = s.thumbLowres !== false;

  cfg.thumbLowresLevel = s.thumbLowresLevel || 0;

  cfg.viewerQuality = (s.viewerQuality === 0 || s.viewerQuality) ? s.viewerQuality : 0;

  cfg.viewerMaxdim = s.viewerMaxdim || 0;

  cfg.viewerLowres = (s.viewerLowres === 0 || s.viewerLowres) ? s.viewerLowres : 0;
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
    cfg.thumbConcurrency = numOr(s.thumbConcurrency, 3);
    cfg.preloadConcurrency = numOr(s.preloadConcurrency, 0);
    cfg.gpuDecode = s.gpuDecode !== false; // 默认 true
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
  setSelValue('setThumbEngine', cfg.thumbEngine || 'auto');
  $('setThumbLowres').checked = cfg.thumbLowres !== false;

  $('setThumbLowresLevel').value = String(cfg.thumbLowresLevel || 0);

  $('setViewerQuality').value = String(cfg.viewerQuality || 0);

  $('setViewerMaxdim').value = String(cfg.viewerMaxdim || 0);

  $('setViewerLowres').value = String(cfg.viewerLowres || 0);
    $('setTakeover').checked = cfg.takeoverSystemThumb;
    $('setCPUCores').value = cfg.cpuCores || 0;
    $('setConcurrency').value = numOr(cfg.thumbConcurrency, 3);
    $('setPreloadEnabled').checked = (cfg.preloadConcurrency || 0) > 0;
    $('setGPUDecode').checked = cfg.gpuDecode !== false;
    $('setViewerPreload').value = cfg.viewerPreload != null ? cfg.viewerPreload : 2;
    setSelValue('setViewerAnimation', cfg.viewerAnimation || 'slide');
    setSelValue('setViewerMode', cfg.viewerMode || 'window');
    // 复选框变化时立即同步到 cfg。
    // 为什么要这样：saveSettings 以 cfg 为真源（不再读 DOM）—— 否则"面板没打开过就保存"
    // 会把 DOM 的初始值写回后端，而其中有些设置是**全局**的（影响所有窗口），写错就一起坏。
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
    thumbEngine: $('setThumbEngine').value || 'auto',
    thumbLowres: $('setThumbLowres').checked,

    thumbLowresLevel: parseInt($('setThumbLowresLevel').value, 10) || 0,

    viewerQuality: parseInt($('setViewerQuality').value, 10),

    viewerMaxdim: parseInt($('setViewerMaxdim').value, 10) || 0,

    viewerLowres: parseInt($('setViewerLowres').value, 10) || 0,
      takeoverSystemThumb: $('setTakeover').checked,
      cpuCores: parseInt($('setCPUCores').value, 10) || 0,
      thumbConcurrency: numOr($('setConcurrency').value, 3),
      preloadConcurrency: $('setPreloadEnabled').checked ? 2 : 0,
      gpuDecode: $('setGPUDecode').checked,
      viewerPreload: parseInt($('setViewerPreload').value, 10) || 0,
      viewerAnimation: $('setViewerAnimation').value || 'slide',
      viewerMode: $('setViewerMode').value || 'window',
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

  // 触屏档位的返回按钮（**显示**由 CSS 的 @media (pointer: coarse) 控制，
  // 桌面根本看不见它；这里只接行为）。加 null 判断是为了容忍
  // "旧 index.html + 新 app.js" 这种版本错配 —— 缺元素时只是没这个按钮，不该整页报错。
  // 注：大图查看器**不再**单独放返回按钮（按用户要求去掉，别再加回来）——
  //     手机飞牛窗口自带返回箭头，从文件管理器点开单图时它等价于"关闭看图窗口"。
  //     应用内"大图 → 网格"由窗口那个返回箭头负责（它会退一格历史，
  //     而历史里常驻着我们压的守卫条目，所以只会关掉查看器那一层，不会退出应用）。
  //     ⚠️ 2.2.0 起 touch.js **只做双指捏合**，右边缘横滑返回整块删除（用户要求），
  //        别再把"退回网格走右边缘横滑"这类说法写回去。
  var navBackBtn = $('navBack');
  if (navBackBtn) navBackBtn.onclick = function () {
    // 2.3.2：**按状态分流**。此前它直接绑 goUpLevel —— 于是"在大图里点箭头"会走
    //   goUpLevel → navigateTo(上级) → **真导航整页重载**，而不是"关掉查看器回网格"。
    //   查看器里只有**系统返回**才走 popstate → closeViewer，箭头这条路整个绕过了它。
    //   ⇒ 这正是"大图没加载完成时返回就退出软件"的来源：
    //      点箭头触发了真导航、页面正在加载，此时再返回，宿主的状态机就乱了。
    if (!viewer.classList.contains('hidden')) {
      // closeViewer 内部会在"主动关闭"时补一次 history.back()，把我们压的那条收回。
      closeViewer();
      return;
    }
    goUpLevel();
  };

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

  function boot() {
    var qs = new URLSearchParams(location.search);
    var single = qs.get('path');
    if (single) {
      // ⚠️ 这里**不再** `singleMode = true` —— 它已经在顶层按 URL 定好了
      //（见「单图 / 独立看图窗口模式」那段）。同一条判据分两处写，迟早会分叉。
      // 单文件模式（独立窗口）：立即隐藏工具栏和网格，只显示查看器，避免闪烁
      $('topbar').style.display = 'none';
      $('main').style.display = 'none';
      settingsReady = loadSettings();
      // 首图不依赖 settings：图片 URL 只用到 pickMaxdim()，与 settings 无关。
      // 不等 settings 可以省 1 个 RTT（局域网 5~30ms，经网关/宿主代理时可能更多）。
      // settings 到达后 applySettings() 会自动补齐动画效果；viewerPreload 的默认值
      // 与大多数用户设置一致，晚到几百 ms 不影响首图体验。
      var sortParam = qs.get('sort');
      // 1.8.188：去掉了 `paths`（宿主有序列表）分支 —— 实测 0/2013 从未出现（见 openSingle 注释）。
      if (sortParam) {
        // URL 显式带排序（从查看器入口跳转等）：用它，且本次会话不再自动跟随。
        state.sortManual = true;
        openSingle(single, normalizeSortParam(sortParam));
      } else {
        // 从文件管理器双击图片：URL 只有 path。排序等 settings 就绪后，
        // 在 openSingle 内部读文件管理器的排序偏好（「跟随」开关控制）。
        openSingle(single, null);
      }
      return;
    }
    // 2.3.1：**真导航落地**时 URL 带 `?dir=` —— 优先用它打开目标目录。
    //   ⚠️ 不能用 `?path=`：那个参数**已经被"单文件模式"占用**（见上面的 single 分支）。
    var dirParam = qs.get('dir');
    if (dirParam !== null && dirParam !== '') {
      if (dirParam === '/') {
        // 显式要求"最上层"：**不能**走下面的 last 分支，
        // 否则会从 localStorage 跳回上次目录（"回到最上层失败"就是这么来的）。
        settingsReady = loadSettings();
        settingsReady.then(function () { buildRoot(); });
        return;
      }
      state.root = false;
      state.path = dirParam;
      settingsReady = loadSettings();
      settingsReady.then(function () { loadDir(dirParam); });
      return;
    }
    // 2.4.0：**手机版不再"跳回上次目录"**（用户要求）——
    //   从飞牛桌面图标进来时必须落在新首页（聚合文件夹层），而不是上次看的深层目录。
    //   桌面版保持原行为（那边窗口常驻、恢复上次位置更自然）。
    var last = null;
    if (PROFILE !== 'touch') {
      try { last = localStorage.getItem('mediaview_last'); } catch (e) {}
    }
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
  // 关窗 / 刷新 / 宿主销毁页面时立即恢复缩略图生成，避免暂停状态残留到 TTL 到期。
  // beforeunload 与 pagehide 都挂：移动端与部分宿主里前者不触发，后者更标准；
  // pagehide 加了 persisted 判断，进 bfcache（页面没销毁、返回后查看器还开着）时不误恢复。
  window.addEventListener('beforeunload', flushResumeThumbGen);
  window.addEventListener('pagehide', function (e) { if (!e.persisted) flushResumeThumbGen(); });
  boot();
})();
