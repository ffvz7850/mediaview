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
    // 换 URL ⇒ 封面会重新合成（缝色/画质/尺寸都可能变），灰度结论也一并作废
    coverToneCache = {};
    // 2.4.35：跨会话的"加载过"记录同理作废 —— 否则清缓存后仍会直接给旧 URL 的 src
    coverSeen = {}; coverSeenOrder = [];
    try { localStorage.removeItem('mediaview_cover_seen'); } catch (e) {}
  }
  function thumbURL(p, size, mtime) {
    // 带上画质：服务端的缓存标识含画质（改画质会重新生成），URL 也必须跟着变，
    // 否则浏览器会继续用 immutable(一年) 的旧图，用户以为设置没生效。
    return API + '/thumb?path=' + enc(p) + '&size=' + (size || 320) +
      '&q=' + (cfg.thumbQuality || 80) +
      '&_t=' + (mtime || 0) + '&_v=' + thumbCacheBust + externalTag();
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
    return url + externalTag();
  }
  // 2.4.42：**外部打开**（飞牛文件管理器双击 → 打开方式 → 本应用）的标记。
  //
  // 「只显示我添加的文件夹」约束的是**本应用自己的浏览范围**（首页 / 网格 / 面包屑）。
  // 宿主把某个文件点名交给我们打开时，用户并没有在应用里浏览 —— 那是一次
  // **单文件级、明确的**指令。若仍按可见范围卡它，表现就是"文件管理器里除了
  // 我添加的那几个文件夹，别的目录双击图片/视频都打不开"。
  //
  // 服务端见到 external=1 只**少收那一层**：存储空间白名单、绝对路径、链接解析
  // 复检全都还在（见 config.go 的 resolveMediaPathReq）。
  // 只有单文件模式会带它 —— 网格/首页的请求永远不带，所以可见范围本身没变。
  function externalTag() {
    // 顶层 `var singleMode` 在 IIFE 执行时就已经定好（早于任何 URL 构造），
    // 这里的 typeof 只是为了在"函数被单独抠出去跑"的测试环境里不炸。
    return (typeof singleMode !== 'undefined' && singleMode) ? '&external=1' : '';
  }
  // 影响 /api/raw 产物的设置指纹（值变了 URL 就变 → 浏览器重新取）
  function rawSettingsTag() {
    return (cfg.viewerQuality | 0) + '.' + (cfg.viewerLowres | 0) + '.' + (cfg.thumbQuality | 0);
  }
  function enc(s) { return encodeURIComponent(s); }

  // 本次会话里「已经加载过封面」的目录（key = dir|mtime）。
  // 用途：再次进入同一目录时直接给 src（浏览器缓存必命中），跳过淡入 —— 否则
  // 每次进/退目录，封面都要从 📁 重新淡入一遍，视觉上就是用户说的"闪一下"。
  var coverDidLoad = {};
  // 2.4.35：跨会话记住"这个封面曾经正常加载过"（key = path|mtime，与 coverDidLoad 同一套）。
  //
  // 为什么必须持久化：冷启动（重开应用 / 宿主把页面重载）时 coverDidLoad 是空的，
  // 于是每个文件夹封面都要重新排一次并发队列（同时只放 4 个）—— 屏幕上就是
  // "📁 → 一张张慢慢冒出来"，也就是用户说的"封面滑动加载闪烁"。
  // 记住之后，首屏直接给 src：命中浏览器 HTTP 缓存时 1~2 帧就出图，
  // 既不排队、也不露 📁。key 里带 mtime ⇒ 目录内容一变（封面会重合成）自然失效。
  var coverSeen = {};        // ckey -> 1
  var coverSeenOrder = [];   // 插入顺序（超上限时从最老的丢）
  var COVER_SEEN_MAX = 600;
  try {
    var _cs = JSON.parse(localStorage.getItem('mediaview_cover_seen') || 'null');
    if (_cs && _cs.order && _cs.order.length) {
      coverSeenOrder = _cs.order.slice(-COVER_SEEN_MAX);
      for (var _i = 0; _i < coverSeenOrder.length; _i++) coverSeen[coverSeenOrder[_i]] = 1;
    }
  } catch (e) { coverSeen = {}; coverSeenOrder = []; }

  var coverSeenTimer = 0;
  function noteCoverSeen(ckey) {
    if (!ckey || coverSeen[ckey]) return;
    coverSeen[ckey] = 1;
    coverSeenOrder.push(ckey);
    while (coverSeenOrder.length > COVER_SEEN_MAX) {
      var drop = coverSeenOrder.shift();
      if (drop) delete coverSeen[drop];
    }
    // 合并写：一屏二十个封面只落一次盘，别每张图都碰 localStorage
    if (coverSeenTimer) return;
    coverSeenTimer = setTimeout(function () {
      coverSeenTimer = 0;
      try { localStorage.setItem('mediaview_cover_seen', JSON.stringify({ order: coverSeenOrder })); } catch (e) {}
    }, 800);
  }

  // ---------- 2.4.38：封面主色占位 ----------
  //
  // 第一次进某个目录、封面还没在 HTTP 缓存里时，格子只能露出「📁 + 名称」，
  // 一屏二十个 📁 然后挨个被封面替换 —— 这就是用户说的"打开文件夹闪封面"。
  // 解决不是让封面更快，而是**先给每个格子一个接近封面的颜色**：
  // 上次封面解码后取一张 12×8 的平均色存下来，下次进目录立刻铺在格子底下，
  // 封面到位时只做一次很短的淡入 —— 视觉上是"暗块慢慢变清晰"，而不是"📁 墙集体跳出"。
  //
  // 只存颜色字符串（一个格子约 40 字节），上限 500 个 ≈ 20KB，和 cover_seen 同一套 LRU。
  var coverColorCache = {};   // ckey -> 'rgb(r,g,b)'
  var coverColorOrder = [];
  var COVER_COLOR_MAX = 500;
  try {
    var _cc = JSON.parse(localStorage.getItem('mediaview_cover_color') || 'null');
    if (_cc && _cc.order && _cc.m) {
      coverColorOrder = _cc.order.slice(-COVER_COLOR_MAX);
      for (var _ci = 0; _ci < coverColorOrder.length; _ci++) {
        if (_cc.m[coverColorOrder[_ci]]) coverColorCache[coverColorOrder[_ci]] = _cc.m[coverColorOrder[_ci]];
      }
    }
  } catch (e) { coverColorCache = {}; coverColorOrder = []; }
  var coverColorTimer = 0;
  function noteCoverColor(ckey, color) {
    if (!ckey || !color) return;
    if (coverColorCache[ckey] === color) return;
    if (!coverColorCache[ckey]) coverColorOrder.push(ckey);
    coverColorCache[ckey] = color;
    while (coverColorOrder.length > COVER_COLOR_MAX) {
      var drop = coverColorOrder.shift();
      if (drop) delete coverColorCache[drop];
    }
    if (coverColorTimer) return;
    coverColorTimer = setTimeout(function () {
      coverColorTimer = 0;
      try {
        var m = {};
        for (var i = 0; i < coverColorOrder.length; i++) {
          var k = coverColorOrder[i];
          if (coverColorCache[k]) m[k] = coverColorCache[k];
        }
        localStorage.setItem('mediaview_cover_color', JSON.stringify({ order: coverColorOrder, m: m }));
      } catch (e) { /* 满/被禁：只是丢占位，不影响正确性 */ }
    }, 800);
  }
  // 取整张封面的平均色（不是底部 30% —— 占位块是整格铺底，要的是整体印象）。
  function sampleCoverColor(img) {
    try {
      var cv = document.createElement('canvas');
      cv.width = 12; cv.height = 8;
      var cx = cv.getContext('2d');
      if (!cx) return '';
      var iw = img.naturalWidth || img.width;
      var ih = img.naturalHeight || img.height;
      if (!iw || !ih) return '';
      cx.drawImage(img, 0, 0, iw, ih, 0, 0, 12, 8);
      var d = cx.getImageData(0, 0, 12, 8).data;
      var r = 0, g = 0, b = 0, n = 0;
      for (var i = 0; i < d.length; i += 4) {
        if (!d[i + 3]) continue;   // 透明像素不参与（否则会被拉黑）
        r += d[i]; g += d[i + 1]; b += d[i + 2]; n++;
      }
      if (!n) return '';
      return 'rgb(' + Math.round(r / n) + ',' + Math.round(g / n) + ',' + Math.round(b / n) + ')';
    } catch (e) { return ''; }
  }

  // ---------- 封面名称配色的灰度自适应（2.4.20）----------
  //
  // 名称压在封面底部的一条**半透明**蒙层上（图片会透出来），所以文字颜色不能写死：
  //   图偏暗 ⇒ 白字 + 深色蒙层；图偏亮 ⇒ 黑字 + 浅色蒙层。
  //
  // 判据取「文字背后那一块图」（底部 30%）的平均灰度，不是整张图：
  // "上半是天空、下半是剪影"这类构图，用整张的平均会把颜色选反。
  var coverToneCache = {};   // key 与 coverDidLoad 同一套（dir|mtime）

  // pickCoverTone 决定用哪套配色。
  //   'bright' ⇒ 图偏亮 ⇒ 黑字 + **不铺底**（只靠白色柔光托起来）
  //   'dark'   ⇒ 图偏暗 ⇒ 白字 + 深色半透明渐变
  //
  // 为什么门槛是 190 而不是 140：亮图那套没有底色、只靠柔光 —— 门槛必须"真的够亮"，
  // 否则中间偏亮的封面上黑字会发虚。140~190 这段继续走白字+深条（用户认可的观感）。
  //
  // 为什么还要看 minCol（底部 24 列里最暗那一列的均值）：黑字没有底色可依，
  // "整体很亮、但文字压着的某一段是深色细节"（浅色海滩上的礁石、白云缝里的山脊）
  // 会直接糊掉。取最暗那一列兜底 —— 只要有一小段不够亮，就退回白字+深条。
  // 单参数调用时把 minCol 视作 mean（等价于只看整体亮度）。
  function pickCoverTone(mean, minCol) {
    if (minCol == null) minCol = mean;
    // 取整再比：Rec.601 的三个权重相加在浮点里是 0.9999999999999999，
    // 纯灰 190 会算成 189.99999…，卡在阈值上被判成"暗" —— 边界值必须稳定。
    return (Math.round(mean) >= 190 && Math.round(minCol) >= 150) ? 'bright' : 'dark';
  }

  // sampleCoverTone 把封面底部 30% 画到一张 24×8 的小画布上取平均灰度。
  // 任何一步失败都返回 ''（调用方保持 CSS 默认的"深色蒙层 + 白字"），
  // 绝不能让"算不出来"变成"文字看不见"。
  function sampleCoverTone(img) {
    try {
      var cv = document.createElement('canvas');
      cv.width = 24; cv.height = 8;
      var cx = cv.getContext('2d');
      if (!cx) return '';                      // 没有 canvas（极老浏览器 / jsdom）
      var iw = img.naturalWidth || img.width;
      var ih = img.naturalHeight || img.height;
      if (!iw || !ih) return '';
      var sh = Math.max(1, Math.round(ih * 0.3));   // 只取底部 30%
      cx.drawImage(img, 0, ih - sh, iw, sh, 0, 0, 24, 8);
      // 画布是 24 列 × 8 行（getImageData 是**行优先**：offset = (y*24+x)*4）。
      // 既算整体均值，也算"每一列均值"里最暗的那一列 —— 名称横着铺满一行，
      // 最暗的那一列才是真正的短板（见 pickCoverTone 的说明）。
      var d = cx.getImageData(0, 0, 24, 8).data;
      var W = 24, H = 8, sum = 0, n = 0, minCol = 999;
      for (var x = 0; x < W; x++) {
        var colSum = 0;
        for (var y = 0; y < H; y++) {
          var o = (y * W + x) * 4;
          // Rec.601 亮度权重：直接用 RGB 算术平均会把纯红/纯蓝判反
          var lum = 0.299 * d[o] + 0.587 * d[o + 1] + 0.114 * d[o + 2];
          colSum += lum; sum += lum; n++;
        }
        if (colSum / H < minCol) minCol = colSum / H;
      }
      if (!n) return '';
      return pickCoverTone(sum / n, minCol);
    } catch (e) {
      return '';   // 画布被污染（跨域图）等：静默降级
    }
  }

  // applyCoverTone 给封面加 .mv-tone-bright / .mv-tone-dark，配色全交给 CSS。
  // img 传 null 表示"只查缓存"：封面本身已在浏览器缓存里，不必再画一次。
  function applyCoverTone(cover, ckey, img) {
    var tone = coverToneCache[ckey];
    if (!tone && img) {
      tone = sampleCoverTone(img);
      if (tone) coverToneCache[ckey] = tone;
    }
    if (!tone) return;
    cover.classList.remove('mv-tone-bright', 'mv-tone-dark');
    cover.classList.add('mv-tone-' + tone);
  }

  // 文件夹封面 URL：与 thumbURL 同构（含 _t/_v），清缓存/换目录都跟着失效。
  // 缝色是服务端固定色（2.4.26 起），与主题、请求参数无关 —— 缓存键只含 path/size。
  function coverURL(p, size, mtime) {
    return API + '/dircov?path=' + enc(p) + '&size=' + (size || 320) +
      '&_t=' + (mtime || 0) + '&_v=' + thumbCacheBust;
  }

  function metaURL(p) { return API + '/meta?path=' + enc(p) + externalTag(); }

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
  // 2.4.30：加可选回调（cb）—— 竖图流要拿这个尺寸当"不等图片到达就能算高度"的依据。
  // 之前只有大图查看器用（它自己轮询 item.w），所以没有回调需求；
  // 现在 flow 也走同一条 /api/meta，**不许**再像 2.4.29 那样拿缩略图当"比例探针"
  // （那是另起一套取图，用户明确否掉了）。
  // 同一个 item 上多个调用方：已在途就挂到 _szCbs 上，回来一起通知。
  function ensureItemSize(item, cb) {
    if (!item) return;
    if (item.w && item.h) { if (cb) { try { cb(); } catch (e) {} } return; }
    if (cb) { (item._szCbs || (item._szCbs = [])).push(cb); }
    if (item._szPending) return;
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
    }).catch(function () {}).then(function () {
      item._szPending = false;
      var cbs = item._szCbs || [];
      item._szCbs = null;
      for (var i = 0; i < cbs.length; i++) { try { cbs[i](); } catch (e) {} }
    });
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
    coverEnabled: true,
    // 2.4.29：手机端默认走「竖图连续浏览」（用户拍板）。桌面档不看这个开关。
    mobileFlowView: true,
    // 2.4.35：手机端换目录默认走**内部切换**（零重载）。关掉则退回整页导航。
    // 2.4.20：默认（关）⇒ 名称压在封面底部，默认按"图偏暗"配色，等 onload 再按灰度纠正
    coverNameBelow: false,
    // 2.4.16：只显示用户添加的文件夹（范围判定在服务端生效，这里只驱动界面）
    onlyRoots: false,
    roots: [],
    rootsInfo: [],
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
  var rafId = 0;       // 已排队的那一帧（滚动/缩放时的渲染合并）

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
    // 2.4.29：竖图连续浏览模块，同样只在触屏档加载（桌面连字节都不下）。
    var flowScript = document.createElement('script');
    flowScript.src = 'flow.js?v=__MV_VERSION__';
    flowScript.defer = true;
    document.head.appendChild(flowScript);

    // 2.4.29：flow 模块的 host 契约。
    // 只给"取值"和"回网格定位"两件事，不把 v / track / swipe 这些内部状态交出去 ——
    // flow.js 因此可以完全不依赖本文件的实现细节（另起炉灶的前提）。
    // URL 构造复用同一套函数 ⇒ 画质/清晰度/lowres/缩略图尺寸这些设置自动跟着走，
    // 不会出现"大图按设置、流按另一套"的分裂。
    window.__mvFlowHost = {
      rawURL: function (p, maxdim, mtime) { return rawURL(p, maxdim, mtime); },
      // thumbURL 只给**视频封面帧**用（视频没有静态大图可取）；
      // 图片一律不许走它 —— 见 flow.js 里"图片分支只用 rawURL"的说明。
      thumbURL: function (p, size, mtime) { return thumbURL(p, size, mtime); },
      pickMaxdim: function () { return pickMaxdim(); },
      thumbSize: function () { return cfg.thumbSize || 320; },
      // 2.4.30：宽高走 /api/meta，与大图查看器**同一个函数**（ensureItemSize）。
      // 这样"比例"不再依赖缩略图，画质/解码也全是大图那一条链路。
      ensureSize: function (item, cb) { ensureItemSize(item, cb); },
      toast: function (m) { try { toast(m); } catch (e) {} },
      gridFocus: function (idx) { try { scrollToItem(idx, true); } catch (e) {} },
      // 2.4.41：竖图流打开时**自己按当前规则排一遍** —— 它拿到的数组可能来自
      //   旧排序的 state.items 或旧快照（见 resortLikeFnOs 的说明）。
      //   比较器只此一份，流里不重复实现。
      sortList: function (list, sort) { return resortLikeFnOs(list, sort); },
      // "当前规则"直接取 state.sort —— 它就是工具栏上的值，也正是网格这次请求实际用的值
      //   （网格档 resolveSortForDir 恒返回 state.sort）。
      //   不用 state.currentSort：那是"上一次请求的产物"，语义上绕了一层，
      //   而且列表还在飞的时候它代表的是**旧**请求。
      currentSort: function () { return state.sort; }
    };
    try { window.dispatchEvent(new Event('mv-flow-host')); } catch (e) {}
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
  // 按飞牛规则重排 —— **全项目唯一的比较器**（5 个字段 × 升降序，2.4.41）。
  //
  // 为什么前端还要有一份（后端 /api/list 已经按同一规则排好了）：
  //   因为前端经常会拿到一份**顺序不可信**的数组 —— 竖图流打开时用的就是
  //   `state.items`，而它可能是"改排序之前的"（列表还没回来）或"快照里的旧顺序"
  //   （手机换目录是整页导航，快照先铺）。只有"能按规则再排一遍"才治得住这两种窗口。
  //
  // 语义必须与后端 sortKey.less() 对齐：
  //   · name / type 走 ICU（zh + numeric，见 zhCollator）；
  //   · size / mtime / btime 是数值差；
  //   · 同键**保持原顺序**（稳定），与后端的 sort.SliceStable 一致 ——
  //     这里**不加**名称兜底：加了会让网格与竖图流从同一个起点排出两个顺序。
  function resortLikeFnOs(list, sort) {
    if (!list || list.length < 2) return list;
    var p = sortParts(sort || '');
    var sign = p.desc ? -1 : 1;
    var field = p.field;
    var col = null, keyOf;
    if (field === 'name' || field === 'type') {
      col = zhCollator();
      if (!col) return list; // 环境不支持 Intl → 保留后端顺序，别乱动
      if (field === 'type') {
        keyOf = function (x) {
          var n = String((x && x.name) || '');
          var i = n.lastIndexOf('.');
          return i > 0 ? n.slice(i + 1).toLowerCase() : '';
        };
      } else {
        keyOf = function (x) { return String((x && x.name) || ''); };
      }
    } else if (field === 'size' || field === 'mtime' || field === 'btime') {
      // btime 是"按创建时间排序时才去 statx 取一次"的（后端 scanner.go 的 needBtime）。
      // 所以「刚改成按创建时间、列表还没回来」时，手上这份列表多数项没有 btime：
      // 全当 0 排会变成一坨乱序，比"暂时保持旧序"更难看 ⇒ 有效值不足一半就跳过，
      // 等真实的列表回来自然会排对（不为这一下多打一个请求）。
      if (field === 'btime') {
        var valid = 0;
        for (var q = 0; q < list.length; q++) if ((list[q] && list[q].btime) > 0) valid++;
        if (valid * 2 < list.length) return list;
      }
      keyOf = (function (f) { return function (x) { return Number((x && x[f]) || 0); }; })(field);
    } else {
      return list;
    }
    var idx = new Array(list.length);
    for (var i = 0; i < list.length; i++) idx[i] = i;
    idx.sort(function (a, b) {
      if (col) {
        var c = col.compare(keyOf(list[a]), keyOf(list[b]));
        if (c !== 0) return c * sign;
      } else {
        var d = keyOf(list[a]) - keyOf(list[b]);
        if (d !== 0) return d * sign;
      }
      return a - b; // 稳定：同键保持原顺序
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

  // ---------- 加载指示器（延迟显示，消除"转圈闪一下"）----------
  //
  // 2.4.15：进/退目录时转圈只闪一下 —— 因为本地 list 请求常在 100ms 内返回，
  // 指示器"显示→立即隐藏"在视觉上就是一次闪烁。改成**超过 180ms 才显示**：
  // 快请求根本看不到转圈；真卡住时仍然会显示，用户照样知道在等。
  var loadingTimer = null;
  function showLoadingSoon() {
    if (loadingTimer) clearTimeout(loadingTimer);
    // 2.4.31：180 → 400ms。NAS 上「进/退目录」常在 200~350ms 返回，180ms 的阈值
    //   会让转圈**闪一下又消失**（比不显示更烦）。400ms 之后才算"真的慢"。
    loadingTimer = setTimeout(function () {
      loadingTimer = null;
      loadingEl.classList.remove('hidden');
    }, 400);
  }
  function hideLoadingNow() {
    if (loadingTimer) { clearTimeout(loadingTimer); loadingTimer = null; }
    loadingEl.classList.add('hidden');
  }

  // 首页空状态要说清"为什么空"：开了「只显示我添加的文件夹」却一个都没加（或都不可用）时，
  // 如果只显示通用的"此文件夹没有图片或视频"，用户根本不知道该去设置里处理 ——
  // 会以为应用坏了。
  function applyRootEmptyHint() {
    if (!state.root || !cfg.onlyRoots || state.items.length > 0) return;
    if (!(cfg.roots || []).length) {
      emptyText.innerHTML = '还没有添加任何文件夹。<br>请到设置 →「只显示我添加的文件夹」里添加。';
    } else {
      emptyText.innerHTML = '已添加的文件夹当前都不可用（磁盘未挂载或被删除）。<br>请到设置里检查这份列表。';
    }
  }

  // ---------- Root (storage spaces) ----------
  function buildRoot() {
    state.root = true; state.path = null;
    showLoadingSoon();
    // 2.4.40：根层也走**快照先行**。
    //   从第一层文件夹按返回，落到的就是这一页 —— 它是返回路径上最常出现的页面，
    //   "等 /api/list 才出内容"就是用户看到的那一下加载。快照命中就直接铺，
    //   接口回来一致则一个字节都不改。
    var snap = loadSnap('/', state.sort);
    var snapApplied = false;
    if (snap && !sameItems(state.items, snap.items)) {
      state.items = snap.items;
      state.files = snap.files || [];
      renderCrumb();
      applyItems(null, snap.top || 0);
      snapApplied = true;
      hideLoadingNow();
    } else if (snap) {
      hideLoadingNow();
    }
    // 一次拿到真实盘位列表：原来是硬编码 /vol1..10（盘位更多的机器看不到后面的存储空间），
    // 而且每一轮还先发一个返回值完全没用的 /api/health —— 首屏白白多打 10 个请求。
        // 2.4.0：首页 = **聚合文件夹层**（所有存储空间用户目录下的文件夹合并成一层）。
    //   原来是 `/api/volumes`（列出 /vol1、/vol2 这些盘位），用户要先点进盘位才看到自己的文件夹；
    //   飞牛文件管理器不是这样，它直接列内容。现在两边一致。
    //   `?path=/` 是后端约定的"聚合根"，返回的每条 dirs 都带**真实绝对路径**，
    //   所以点进去走的还是原来的目录逻辑，前端无需额外状态。
    fetchJSON(API + '/list?path=' + enc('/') + '&sort=' + enc(state.sort)).then(function (d) {
      hideLoadingNow();
      // ⚠️ 必须**转成 {kind:'dir'}**：后端返回的是 DirEntry（只有 name/path/size/mtime），
      //   而渲染靠 `item.kind === 'dir'` 决定"画文件夹图标"还是"去取缩略图"。
      //   少了这一步，首页的文件夹会被当成文件渲染（格子空白、还白跑缩略图请求）。
      //   loadDir 里一直有这一步，首页聚合时漏了 —— 真 Edge 实测抓出来的。
      var rootDirs = ((d && d.dirs) || []).map(function (x) {
        return { kind: 'dir', name: x.name, path: x.path };
      });
      // 2.4.18：首页也要接住 files，并且和 loadDir 一样用同一套排序。
      //   展开一层后，已添加文件夹里的**散图**会随 dirs 一起返回：
      //     · 显示由 state.items 负责（dirs + files 都在里面）；
      //     · state.files 是"这个目录的文件集合"，供后台预生成与
      //       设置页的「立即生成本目录缩略图」使用。
      //   两处都要填，否则首页的散图不会被预生成（第一次翻到时才现做，慢一拍）。
      var rootFiles = (d && d.files) || [];
      var rootSort = (d && d.sort) || state.sort;
      rootDirs = resortLikeFnOs(rootDirs, rootSort);
      rootFiles = resortLikeFnOs(rootFiles, rootSort);
      var nextItems = rootDirs.concat(rootFiles);
      var unchanged = sameItems(state.items, nextItems);
      var keepTop = snapApplied ? scroller.scrollTop : 0;
      state.items = nextItems;
      state.files = rootFiles;
      saveSnap('/', nextItems, rootFiles, keepTop, rootSort);
      renderCrumb();
      if (!unchanged) applyItems(null, keepTop);
      applyRootEmptyHint();      // 2.4.16：首页空状态要说清"为什么空"
      syncExitTips();            // 2.4.0：到根层 ⇒ 挂上"是否退出"的确认
    }).catch(function () {
      hideLoadingNow();
      state.items = [];
      state.files = [];
      renderCrumb();
      layout();
      applyRootEmptyHint();
    });
  }

  // ---------- Load directory ----------
  // 目录加载序号：快速连点两个目录时先发的请求可能后返回 ——
  // 没有这个守卫就会出现「面包屑是后点的目录、网格却是先点目录的内容」。
  var loadSeq = 0;
  // ---------- 换目录：真页面导航 ----------
  //
  // 每一层目录都是一个**真实的页面条目**（`?dir=<绝对路径>`），由浏览器/宿主自己记账。
  // 于是"返回"只有一种语义：回到上一层那个页面 —— 不需要在历史里布置守卫，
  // 也不需要猜宿主一次手势到底退了几格。这是最朴素、也最难出错的方案。
  //
  // 代价只有一条：每次换目录都是一次文档加载，首屏本来要等 /api/list。
  // 对策是**目录快照**（见下面 2.4.31 那段）：离开前把 items 与滚动位置写进
  // sessionStorage，新页面 boot 时**不等任何接口**先把画面铺出来，真实列表回来
  // 一致就一个字节都不改 —— 这就是"返回不闪"的全部机制。
  //
  // ⚠️ 参数名必须是 `dir`：`?path=` 已被"单文件模式"占用。
  function navigateTo(path) {
    rememberScroll();   // 离开前记住"看到第几屏"，返回时停在原处
    location.href = GW + '/?dir=' + encodeURIComponent(path);
  }

  // 回"存储空间列表"（最上层）。显式带 ?dir=/ —— 否则 boot 会把它当成
  // "没有指定目录"而走到首页逻辑（那里可能又落回上次目录）。
  function navigateRoot() {
    rememberScroll();
    location.href = GW + '/?dir=/';
  }

  // ---------- 2.4.31：目录快照（跨页秒开 / 来回切目录不闪）----------
  //
  // 问题：换目录是**整页导航**（见 navigateTo 的说明 —— 那是让宿主/浏览器自己记账、
  //   返回能逐层退的代价）。整页重载后首屏必然要等 /api/list 才有内容，
  //   这段时间就是用户看到的"进/退文件夹闪一下"。
  //
  // 做法：把最近几个目录的 items 存进 sessionStorage（同标签页、关掉即清）。
  //   新页面 loadDir 时**先**用快照画出来（通常 <1 帧就到位），真实列表回来后再比对：
  //   完全一致 ⇒ 一个字都不改（DOM/缩略图/滚动位置全都不动）→ 真正的零闪烁。
  //
  // 为什么用 sessionStorage 而不是 localStorage：这是"这次浏览的来回切换"，
  //   换设备/关标签页就该忘掉；也避免长期占着几 MB。
  var SNAP_MAX_AGE = 10 * 60 * 1000;   // 10 分钟前的快照不用（文件可能已变，代价是退化成正常加载）
  var SNAP_MAX_DIRS = 6;               // 最多记几个目录
  var SNAP_MAX_ITEMS = 800;            // 单个目录超过这么多项就不存（体积/序列化成本）
  var SNAP_MAX_BYTES = 700000;         // 单条上限（sessionStorage 总共约 5MB）

  // sort 参数 = "现在要用的排序"。快照里记的不是这个排序 ⇒ **当没有**。
  //   代价（明确接受）：改过排序后第一次回到旧目录不再秒开，要等 /api/list；
  //   换来的是"顺序永远不会错"，以及竖图流不会拿到旧顺序的数组。
  function loadSnap(p, sort) {
    try {
      var raw = sessionStorage.getItem('mv_snap:' + p);
      if (!raw) return null;
      var o = JSON.parse(raw);
      if (!o || !o.at || (Date.now() - o.at) > SNAP_MAX_AGE) return null;
      if (!o.items || !o.items.length) return null;
      if (sort && o.sort && o.sort !== sort) return null;
      return o;
    } catch (e) { return null; }
  }

  function saveSnap(p, items, files, top, sort) {
    try {
      var key = 'mv_snap:' + p;
      if (!items || !items.length || items.length > SNAP_MAX_ITEMS) {
        sessionStorage.removeItem(key);
        return;
      }
      // 2.4.38：连**滚动位置**一起记 —— 返回上一级时停在原处，是"顺滑"的一半。
      //   （只记 items 的话，回去永远从顶部开始，"我明明看到第 40 张"那种感觉就没了。）
      // 2.4.41：连**排序规则**一起记 —— 否则改了排序后，旧快照会把旧顺序先铺上来
      //   （竖图流再从它里面取列表，就成了"永远按修改时间降序"的观感）。
      var s = JSON.stringify({
        at: Date.now(), items: items, files: files || [], top: top || 0,
        sort: sort || ''
      });
      if (s.length > SNAP_MAX_BYTES) { sessionStorage.removeItem(key); return; }
      sessionStorage.setItem(key, s);
      // 维护一个"最近目录"索引，超出就删最老的（避免 sessionStorage 被撑爆）
      var idx = [];
      try { idx = JSON.parse(sessionStorage.getItem('mv_snap_index') || '[]') || []; } catch (e) { idx = []; }
      idx = idx.filter(function (x) { return x !== p; });
      idx.unshift(p);
      while (idx.length > SNAP_MAX_DIRS) {
        var drop = idx.pop();
        try { sessionStorage.removeItem('mv_snap:' + drop); } catch (e) {}
      }
      sessionStorage.setItem('mv_snap_index', JSON.stringify(idx));
    } catch (e) { /* 隐私模式/配额满：不影响正确性，只是退化成正常加载 */ }
  }

  // 离开目录前把滚动位置写回它自己的快照。
  // 调用时机在 loadDir 开头 —— 那一刻 state.path 还是**旧目录**，正好把"我刚看到第几张"记下来。
  function rememberScroll() {
    // 根层用 '/' 当快照键（buildRoot 也走快照，见那里的说明）
    var p = state.root ? '/' : state.path;
    if (!p) return;
    try {
      var key = 'mv_snap:' + p;
      var raw = sessionStorage.getItem(key);
      if (!raw) return;
      var o = JSON.parse(raw);
      if (!o || !o.items) return;
      o.top = scroller.scrollTop || 0;
      sessionStorage.setItem(key, JSON.stringify(o));
    } catch (e) { /* 快照不可用：只是回到顶部，不影响正确性 */ }
  }

  // sameItems 两份列表是否"看起来完全一样"（路径 + mtime + size 挨个比）。
  // 一致 ⇒ 不需要重建格子（这是"快照先行"能变成零闪烁的关键：不一致才重建）。
  function sameItems(a, b) {
    if (!a || !b || a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) {
      if (a[i].path !== b[i].path) return false;
      if ((a[i].mtime || 0) !== (b[i].mtime || 0)) return false;
      if ((a[i].size || 0) !== (b[i].size || 0)) return false;
    }
    return true;
  }

  function loadDir(path) {
    var mySeq = ++loadSeq;   // 快速连点时先发的请求可能后返回 —— 只认最新的那一次
    state.root = false;
    state.path = path;
    syncExitTips();          // 离开聚合根 ⇒ 撤掉宿主的退出确认（此后返回是「退一层」）
    showLoadingSoon();
    emptyEl.classList.add('hidden');
    // 2.4.31：**不等 settings、不等 /api/list**，先把快照铺上。
    //   快照里只有 items（网格内容），布局只用到 cellMode（localStorage）和容器宽度 ——
    //   都不依赖接口。所以"回到刚看过的目录"可以做到接口还没回、画面已经在。
    var snap = loadSnap(path, state.sort);
    var snapApplied = false;
    if (snap && !sameItems(state.items, snap.items)) {
      state.items = snap.items;
      state.files = snap.files || [];
      renderCrumb();
      applyItems(null, snap.top || 0);   // 连"看到第几屏"一起还原
      snapApplied = true;
      hideLoadingNow();
    } else if (snap) {
      hideLoadingNow();   // boot 已经铺过同一份内容，不再重复布局（重复布局本身就是闪一下）
    }
    // 先确定排序（可能要读文件管理器的偏好），再发列表请求：列表顺序决定了大图切换
    // 顺序、预加载取哪几张、后台预生成的顺序，顺序错了这三样全跟着错。
    resolveSortForDir(path).then(function (sortStrParam) {
      if (mySeq !== loadSeq) return;
      // 记下「网格实际用的排序」：窗口内点开图片时必须复用它，否则两边顺序会不一致。
      state.currentSort = sortStrParam;
      return fetchJSON(API + '/list?path=' + enc(path) + '&sort=' + enc(sortStrParam));
    }).then(function (d) {
      if (!d || mySeq !== loadSeq) return;
      hideLoadingNow();
      var dirs = d.dirs.map(function (x) { return { kind: 'dir', name: x.name, path: x.path }; });
      var files = d.files;
      // 1.8.190：按飞牛规则重排（后端是字节序，会把汉字排到最后）。必须用**本次实际
      // 请求的排序**（带方向），否则降序会被排成升序。
      var effSort = state.currentSort || (d && d.sort) || state.sort;
      dirs = resortLikeFnOs(dirs, effSort);
      files = resortLikeFnOs(files, effSort);
      var nextItems = dirs.concat(files);
      // 与屏上的完全一致 ⇒ **一个字都不改**（快照命中走的就是这条路：格子、缩略图、
      // 滚动位置全部原样保留 —— 这才是"不闪"）。
      var unchanged = sameItems(state.items, nextItems);
      // 快照先铺过 ⇒ 用户可能已经在看、甚至滚了几屏。真实列表回来必须停在原地，
      // 否则"刚滑到第 30 张，列表一回来被弹回顶部"比闪烁更难受。
      var keepTop = snapApplied ? scroller.scrollTop : 0;
      state.items = nextItems;
      state.files = files;
      saveSnap(path, nextItems, files, keepTop, effSort);
      renderCrumb();
      // 变了也只做**增量**替换（见 applyItems）：只有真换了内容的格子才动，
      // 其余 DOM 一个字节都不改，浏览器只 paint 最终状态。
      if (!unchanged) applyItems(null, keepTop);
      if ((cfg.preloadConcurrency || 0) > 0) postPregen(state.files);
      try { localStorage.setItem('mediaview_last', path); } catch (e) {}
    }).catch(function (e) {
      if (mySeq !== loadSeq) return;
      hideLoadingNow();
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
    // 2.4.18：上一层正好是「已添加的文件夹」⇒ 直接回首页。
    //   否则会退到那个文件夹本身（它的原始列表，含 #recycle 等），而首页已经是
    //   它的内容层 —— 用户会觉得"怎么又冒出这个文件夹"。
    if (isCustomRoot(up)) { navigateRoot(); return; }
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

  // ---------- 查看器自己的一层历史 ----------
  //
  // 网格层与层之间**不需要**任何历史操作 —— 每层都是一个真实页面（见 navigateTo）。
  // 唯一需要记账的是**大图查看器**：它只是叠在网格上的一层遮罩、不是页面，
  // 所以打开时压一条历史，让"返回"先落在这里（= 关掉查看器），而不是退到上一层目录。
  //
  // 规则只有两条：
  //   · 打开时 pushState({ mvViewer: 1 }) —— 全项目唯一一处 pushState；
  //   · popstate 里若查看器还开着 ⇒ 关掉它（历史已经退了，这里不用再动历史）。
  // 主动关闭（✕ / 点背景 / Esc）时由 closeViewer 把这条历史收回去，见那里的说明。
  var mvViewerFromPop = false;

  function mvOnPop() {
    if (viewer.classList.contains('hidden')) return;   // 不是查看器这一层：什么都不做
    mvViewerFromPop = true;
    try { closeViewer(); } finally { mvViewerFromPop = false; }
  }

  if (PROFILE === 'touch' && !singleMode) window.addEventListener('popstate', mvOnPop);

  // ⛔ 这里曾有整条「守卫条目」体系（2.3.0 ~ 2.4.39）：启动时压 1~3 条 pushState /
  //    hash 条目，popstate 里补满，用来把宿主的滑动返回"吃掉"。它要解决的问题
  //    （网格里滑动就退出应用）**只存在于内部切换（SPA）**：那时整个会话只有一条
  //    历史，返回必然落到应用之外。改成真页面导航之后，每一层目录本身就是一条历史，
  //    宿主怎么数都不会数到"没有可退的了" —— 前提不存在了，机制也就整块删掉了。
  //    （留档：2.4.33 ~ 2.4.39 七版都在这一层加补丁，无一奏效，原因见交接报告。）

  // ---------- Breadcrumb ----------
  // 当前路径在某个「已添加的文件夹」之下时，返回它的相对段。
  // 返回 null 表示"不在任何已添加文件夹之下"（走完整分段）。
  //
  // 为什么需要：展开模式下进了 root 的子目录，若仍按绝对路径分段，面包屑会变成
  // 「我的文件夹 / vol1 / 1000 / 照片库 / 2024」—— 前几段纯噪音。
  function relSegsUnderRoot(p) {
    if (!cfg.onlyRoots || !cfg.roots || !cfg.roots.length) return null;
    var np = normPath(p);
    if (!np) return null;
    for (var i = 0; i < cfg.roots.length; i++) {
      var r = normPath(cfg.roots[i]);
      if (!r) continue;
      if (np === r) return { base: r, segs: [] };          // 就在根上
      if (np.indexOf(r + '/') === 0) {
        return { base: r, segs: np.slice(r.length + 1).split('/').filter(Boolean) };
      }
    }
    return null;
  }

  function isCustomRoot(p) {
    if (!cfg.onlyRoots || !cfg.roots || !cfg.roots.length) return false;
    var np = normPath(p);
    for (var i = 0; i < cfg.roots.length; i++) {
      if (normPath(cfg.roots[i]) === np) return true;
    }
    return false;
  }

  function renderCrumb() {
    crumbEl.innerHTML = '';
    // 开启「只显示我添加的文件夹」后，首段的语义已经不是"存储空间"了
    var home = makeCrumb(cfg.onlyRoots ? '我的文件夹' : '存储空间', null, false);
    crumbEl.appendChild(home);
    if (!state.path) return;

    var rel = relSegsUnderRoot(state.path);
    var acc, segs;
    if (rel) {
      acc = rel.base;
      segs = rel.segs;
      // 正好停在已添加文件夹本身（首页已展开，通常到不了这里；真到了就只显示首段）
      if (!segs.length) return;
    } else {
      // 普通模式：按绝对路径完整分段。用 normPath 再逐段拼，避免手工加 '/'
      // 把 Windows 盘符（D://a）拼成 /D:/a。
      segs = normPath(state.path).split('/').filter(Boolean);
      acc = '';
      if (normPath(state.path).charAt(0) === '/') acc = '/';
    }
    segs.forEach(function (seg, i) {
      crumbEl.appendChild(makeSep());
      acc = (acc === '/' ? '/' : acc + '/') + seg;
      var cur = i === segs.length - 1;
      crumbEl.appendChild(makeCrumb(seg, acc, cur));
    });
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
    applyItems(state.items, keepScroll ? scroller.scrollTop : 0);
  }

  // 单元格的"内容指纹"。同一格（同一 idx）里指纹不变 ⇒ **一个 DOM 字节都不动**。
  //
  // ⚠️ 刻意**不含 mtime**：/api/list 每次回来的 mtime 精度可能不同（秒 vs 毫秒），
  //   而且"文件被覆盖但大小不变"极少见。把 mtime 算进来会让"同一个目录、内容没变"
  //   被判成"全变了"，于是每次刷新都整屏重建 —— 那正是 2.4.37 之前"打开闪一下"的来源。
  //   代价：原地覆盖成同尺寸新内容时缩略图会晚一步更新（可接受，清缓存可解）。
  function cellKey(it) {
    return (it.kind || '') + '|' + (it.name || '') + '|' + (it.size || 0);
  }

  // 把 state.items 应用到网格上。**不再清空 cellPool** —— renderVisible 会按
  // `data-mv-path` / `data-mv-key` 判断"复用 / 就地刷新 / 单格替换"。
  //
  // 这是"文件夹打开、返回不闪"的核心：以前 layout() 第一件事是把所有格子 removeChild，
  // 然后同步重建 20+ 个 <img>（src 从 1×1 占位重新开始）—— 即使 HTTP 缓存命中，
  // 也要重新解码、重新 paint。整屏内容在同一帧里"全部消失再全部出现"，
  // 用户看到的就是闪一下。现在只有**真的换了一张图**的那个格子才会被替换。
  function applyItems(next, keepTop) {
    if (next) state.items = next;
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
    scroller.scrollTop = keepTop || 0;
    // ⚠️ 这里必须**同步**重建，不能排到 rAF（2.4.31 实测的"换目录闪一下"）：
    //    applyItems 通常在 fetch 的 .then（微任务）里跑，删完 DOM 后如果只 requestRender()，
    //    重建要等**下一帧**的 rAF —— 中间这一帧就是"格子全没了"的空画面。
    //    同步跑完，删除与重建落在同一个任务里，浏览器只 paint 最终状态。
    //    （扫描/滚动那种高频路径仍走 requestRender 合并，别改成同步。）
    renderNow();
  }

  function requestRender() {
    if (rafId) return;
    rafId = requestAnimationFrame(function () { rafId = 0; renderVisible(); });
  }
  // renderNow 立刻重建（并撤掉已排队的那一帧，避免重复跑一遍）
  function renderNow() {
    if (rafId) { cancelAnimationFrame(rafId); rafId = 0; }
    renderVisible();
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
      } else if (el.dataset.mvPath !== item.path || el.dataset.mvKind !== (item.kind || '')) {
        // 这一格换了**另一张图 / 另一个文件夹**：只替换这一格，其余格子原样不动。
        //   换目录时几乎所有格都会走到这里（内容确实全变了），但因为是**同步逐格替换**，
        //   浏览器只 paint 最终状态 —— 不会出现"先全空、再一张张冒"的空帧。
        var fresh = createCell(item, idx, eager);
        cellPool[idx] = fresh;
        if (el.parentNode) el.parentNode.replaceChild(fresh, el);
        else content.appendChild(fresh);
        el = fresh;
      } else {
        // 同一张图：指纹变了就只更新缩略图 URL（DOM 结构不动）
        var key = cellKey(item);
        if (el.dataset.mvKey !== key) {
          el.dataset.mvKey = key;
          refreshCellThumb(el, item);
        }
        loadCellImage(el, eager); // 滚进可见区后才补发请求
      }
      el.style.transform = 'translate3d(' + x + 'px,' + y + 'px,0)';
      el.style.width = state.cell + 'px';
      el.style.height = state.cell + 'px';
    }
  }

  // 就地更新某一格的缩略图地址（不重建 DOM）。只在"同一 path、但内容指纹变了"时调用。
  // 目的：文件被替换时缩略图能跟上，同时**不牺牲 DOM 复用**（重建会让整格闪一下）。
  function refreshCellThumb(el, item) {
    var img = el.querySelector && el.querySelector('img.cell-thumb');
    if (!img) return;
    var want = item.kind === 'dir'
      ? coverURL(item.path, cfg.thumbSize || 320, item.mtime)
      : thumbURL(item.path, cfg.thumbSize || 320, item.mtime);
    if (img.getAttribute('data-src') === want) return;
    img.setAttribute('data-src', want);
    delete img.dataset.mvReq;   // 让 loadCellImage 重新排队
    delete img.dataset.mvFailed;
    delete img.dataset.mvRetried;
  }

  // scrollToItem 把网格滚到第 idx 项并闪一下（2.4.29：从竖图流退出时"回到你看的那张"）。
  //
  // 为什么不用 element.scrollIntoView：格子是 position:absolute + translate3d 虚拟滚动，
  // 目标格子可能**还没被创建**（在窗口外），scrollIntoView 找不到元素就什么也不做。
  // 这里按行号直接算 scrollTop，再让 renderVisible 把那一行建出来。
  //
  // 2.4.43：**已经在屏上的，一格都不要动。**
  //   原来无论目标在哪，都把那一行摆到"视口上方 1/4 处"（本意是滑了很远回来还能看到上下文）。
  //   但目标本来就在屏幕里时，这个"1/4"会把整屏内容硬推一下 —— 手机一屏两列八张（四行），
  //   点任意一张回来，点的那张都会挪到"第二排那个位置"，等于把用户刚看过的画面搬走。
  //   现在按"离得多远"分三档，越近动得越少：
  //     ① 完整可见（含一圈 pad）→ 完全不动，只闪一下；
  //     ② 只露出一部分（被上下边裁掉）→ 最小滚动，刚好整行露出来；
  //     ③ 完全不在视口里（真的滑了很远）→ 仍放到 1/4 处，保留上下文（原行为）。
  function scrollToItem(idx, flash) {
    var n = state.items.length;
    if (!n || idx < 0) return;
    if (idx >= n) idx = n - 1;
    var cols = state.cols || 1;
    var cell = state.cell + state.gap;
    var row = Math.floor(idx / cols);
    var rowTop = row * cell;
    var rowH = state.cell;
    var vh = scroller.clientHeight;
    var top = scroller.scrollTop;
    // 判"是否完整可见"必须**不带 pad**：只要这一整行在屏上就一格都不动。
    // pad 只用于"已经确定要滚"时留一点呼吸空间（太小会贴边，太大又变成一动就跳）。
    var pad = Math.min(24, Math.floor(rowH / 8));
    // ① 完整可见 ⇒ 什么都不做（连 scrollTop 都不赋值：赋值本身在某些内核里也会触发 scroll 事件）
    var visible = rowTop >= top && (rowTop + rowH) <= (top + vh);
    if (!visible) {
      var want;
      if ((rowTop + rowH) > top && rowTop < (top + vh)) {
        // ② 露了一部分：最小滚动（上方被裁 ⇒ 顶到 pad；下方被裁 ⇒ 底到 pad）
        want = rowTop < top ? (rowTop - pad) : (rowTop + rowH + pad - vh);
      } else {
        // ③ 完全在屏外：放到 1/4 处
        want = rowTop - Math.floor(vh / 4);
      }
      scroller.scrollTop = Math.max(0, want);
      requestRender();
    }
    if (!flash) return;
    // 等一帧让 renderVisible 把格子建出来，再挂高亮
    requestAnimationFrame(function () {
      var el = cellPool[idx];
      if (!el) {
        requestRender();
        requestAnimationFrame(function () { flashCell(idx); });
        return;
      }
      flashCell(idx);
    });
    function flashCell(i) {
      var cellEl = cellPool[i];
      if (!cellEl) return;
      cellEl.classList.add('mv-flash');
      setTimeout(function () { cellEl.classList.remove('mv-flash'); }, 1400);
    }
  }

  // flowModule 取竖图流模块（可能还没加载完 / 桌面档根本不存在）
  // 用 try 包住：个别单测会把调用方抠到没有 window 的环境里 eval，不能让这里炸。
  function flowModule() {
    try { return (typeof window !== 'undefined' && window.MVFlow) ? window.MVFlow : null; }
    catch (e) { return null; }
  }

  // flowOpen 竖图流是否正在显示（只在触屏档可能为真；桌面恒 false）
  function flowOpen() {
    var m = flowModule();
    return !!(m && m.isOpen && m.isOpen());
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
      // 文件夹封面：底层是「📁 + 名称」的兜底，封面图成功加载后盖住它。
      //
      // 名称位置有两条路（cfg.coverNameBelow，**默认关**）：
      //   关（默认）⇒ 压在封面底部的一条**半透明**蒙层上，图片隐约透出来；
      //                文字颜色按封面底部那块的灰度自动选黑/白（见 pickCoverTone）。
      //   开         ⇒ 格子内部纵向 flex，封面让出 20px 独立一行放名字（2.4.19 的样式）。
      // 两个元素都建出来（.cell-label / .cell-name-below），由 .mv-name-below 只显示其一，
      // 这样切设置只需改一个 class，不必重建 DOM 结构。
      var nameBelow = cfg.coverNameBelow === true;
      if (nameBelow) inner.classList.add('mv-name-below');
      var cover = document.createElement('div');
      cover.className = 'cell-cover';
      var fb = document.createElement('div');
      fb.className = 'cell-fallback';
      fb.innerHTML = '<div class="cell-fallback-ico">📁</div>' +
        '<div class="cell-fallback-name">' + escapeHtml(item.name) + '</div>';
      cover.appendChild(fb);
      var label = document.createElement('div');
      label.className = 'cell-label';
      label.innerHTML = '<span class="cell-label-name">' + escapeHtml(item.name) + '</span>';
      cover.appendChild(label);
      if (cfg.coverEnabled) {
        var cimg = new Image();
        // 复用 .cell-thumb：loadCellImage 按这个 class 找图，封面因此天然走
        // 现有的 eager / 并发闸 / 大图打开时挂起 那一整套资源纪律，不用另造。
        cimg.className = 'cell-thumb cell-cover-img';
        cimg.decoding = 'async';
        cimg.fetchPriority = 'low';
        var dsrc = coverURL(item.path, cfg.thumbSize || 320, item.mtime);
        cimg.setAttribute('data-src', dsrc);
        cimg.alt = item.name;
        // 已加载过同一版本（dir|mtime）⇒ 直接给 src，跳过并发队列与淡入。
        // 缓存命中时几乎是同步显示，于是"进目录→封面已在"而不是"📁 → 淡入"。
        var ckey = item.path + '|' + (item.mtime || 0);
        // 2.4.38：有主色占位就先铺上，并把 📁 兜底藏起来 —— 📁 墙正是"闪封面"的来源。
        //   封面真正到位时只在这块颜色上做一次短淡入，视觉上是"变清晰"而不是"跳出来"。
        var phc = coverColorCache[ckey];
        if (phc) {
          cover.style.background = phc;
          cover.classList.add('mv-cover-ph');
        }
        if (coverDidLoad[ckey] || coverSeen[ckey]) {
          cimg.dataset.mvReq = '1';
          cimg.src = dsrc;
          cimg.classList.add('mv-cover-ok');
          cover.classList.add('mv-has-cover');
          // 灰度结论上次已经算过：先按缓存上色，避免"盖上一层默认配色再闪一下"
          applyCoverTone(cover, ckey, null);
        } else {
          cimg.src = BLANK_PIXEL;
        }
        cimg.onload = function () {
          if (cimg.src.indexOf('data:image/gif') === 0) return; // BLANK_PIXEL 的 load 不算封面成功
          if (!cimg.isConnected) return;
          coverDidLoad[ckey] = true;
          noteCoverSeen(ckey);
          cimg.classList.add('mv-cover-ok');
          cover.classList.add('mv-has-cover');
          // 图真的到位了才算灰度：BLANK_PIXEL 是 1×1 透明图，算出来是纯黑，会把白字用错地方
          applyCoverTone(cover, ckey, cimg);
          // 2.4.38：顺手记下主色，供下次进这个目录时当占位（见 sampleCoverColor）
          var ph = sampleCoverColor(cimg);
          if (ph) {
            noteCoverColor(ckey, ph);
            cover.style.background = ph;
            cover.classList.add('mv-cover-ph');
          }
        };
        cimg.onerror = function () {
          // 主动断开（并发闸换 src / 暂停让路）不是"没封面"；真失败也无需额外降级 ——
          // fallback 📁 一直在底层兜底，封面 opacity 0 时看到的就是文件夹图标。
          if (cimg.dataset.mvAbort) { delete cimg.dataset.mvAbort; return; }
          // 真失败（空目录 404 / 图坏了）：把主色占位撤掉，否则会是一块纯色没有 📁
          cover.classList.remove('mv-cover-ph');
          cover.style.background = '';
        };
        cover.appendChild(cimg);
      }
      inner.appendChild(cover);
      if (nameBelow) {
        // 封面下方独立一行的名称。放在 .cell-cover **之外**是关键：
        // 放进封面里就还是"压在图上"，深色封面上依旧看不清。
        var nb = document.createElement('div');
        nb.className = 'cell-name-below';
        nb.textContent = item.name;
        nb.title = item.name;       // 窄格子下名字会被省略号截断，悬停能看全
        inner.appendChild(nb);
      }
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
    // 内容身份证：renderVisible 靠它判断"这一格能不能直接复用"（见 cellKey / applyItems）
    el.dataset.mvPath = item.path;
    el.dataset.mvKind = item.kind || '';
    el.dataset.mvKey = cellKey(item);
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

    // ---- 2.4.29：手机档 + 开关打开 ⇒ 竖图连续流（独立模块，不建轨道）----
    //
    // 走这条分支时：不创建 .v-track、不加载 track 里的 prev/next、不装缩放状态 ——
    // 一切都归 flow.js 自己管。返回/关闭仍由本文件的 closeViewer + history 守卫负责
    //（这是历史上"返回退出软件"踩出来的机制，绝不能因为换了浏览形态就绕开）。
    // 2.4.29 分流条件。
    // ⚠️ 用 typeof 守卫读模块级变量：zzz_2230 会把本函数**抠到独立环境**里 eval
    //    （那里没有 cfg / singleMode），裸引用会 ReferenceError 直接让历史回归测试红。
    //    守卫之后两个环境语义一致：真实页面里它们一定存在，测试环境里按默认值走。
    var single = (typeof singleMode !== 'undefined') && singleMode;
    var flowWanted = (typeof cfg !== 'undefined') && cfg.mobileFlowView !== false;
    var mf = (typeof flowModule === 'function') ? flowModule() : null;
    var flowOpened = false;
    if (PROFILE === 'touch' && !single && flowWanted &&
        mf && typeof mf.open === 'function') {
      // 先让查看器可见再 open：flow 要量 scrollEl.clientWidth 来算每张的高度，
      // 在 display:none 的祖先里量出来是 0（会退回 window.innerWidth，虽然也能用，
      // 但"先显示再量"才是确定的顺序）。open 失败就把查看器重新藏回去。
      viewer.classList.remove('hidden');
      applyViewerAnimation();
      // 2.4.41：传 **path** 而不是下标 —— 流内部会按当前排序重排，下标在重排后没有意义。
      try { flowOpened = mf.open(v.list, target.path); } catch (e) { flowOpened = false; }
      if (flowOpened) {
        v.mode = 'flow';
        // 与轨道模式做同样两件事：撤下网格在途请求、让后台缩略图让路
        suspendGridRequests();
        pauseThumbGen();
      } else {
        viewer.classList.add('hidden');
      }
      // 打不开（host 没注入 / 列表为空）⇒ 落到下面走原来的左右滑，不能"点了没反应"
    }

    if (!flowOpened) {
      v.mode = 'slide';
      viewer.classList.remove('hidden');
      applyViewerAnimation();
      // 先把网格在途的缩略图请求撤下来，再请求大图 —— 否则大图要排在它们后面
      suspendGridRequests();
      showCurrent(false); // 打开时只加载当前图，不预加载
      pauseThumbGen();
    }
    // **查看器 = 一层历史**（仅触屏档）。
    //
    // 网格里每一层目录都是真实页面（见 navigateTo），返回天然就是"回上一层页面"。
    // 但查看器只是叠在网格上的一层遮罩、不产生任何页面 —— 不给它压一条历史的话，
    // 在查看器里按返回会**直接退到上一层目录**（查看器还开着，网格已经换了）。
    // 压上这一条之后，"返回"先落在这里 ⇒ popstate → mvOnPop → closeViewer ⇒ 回网格。
    // 桌面档不加：桌面没有返回手势，查看器由 ✕ / Esc 关。
    //
    // 2.4.29：竖图流与轨道模式**共用这一处压栈**（不是各写一遍）——
    // 压栈点必须只有一处，否则"历史里只该有一条查看器条目"这个前提就不成立了
    //（多一条会让返回要按两次）。
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
      listPromise = fetchJSON(API + '/list?path=' + enc(dir) + '&sort=' + enc(sort) + externalTag());
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
        return fetchJSON(API + '/list?path=' + enc(dir) + '&sort=' + enc(s) + externalTag());
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

  // ---------- 屏幕水平三份（大图查看器唯一的"分区"来源） ----------
  // 返回：-1 = 左 1/3（上一张）；0 = 中间 1/3（只响应双击放大）；1 = 右 1/3（下一张）。
  //
  // 语义是"屏幕"，所以基准必须是 vStage 的矩形，**不能**用图片自身的矩形：
  // 一张竖图在宽窗口里只占中间一条，若按图片宽度分三份，"点屏幕左侧黑边"会被算成
  // "点在图片中间" —— 于是切图与双击放大两个判定各说各话（一处切、一处放大）。
  // 点击切图与 dblclick 放大必须共用这一个函数，别再各算一套（2.4.13 收敛成唯一来源）。
  function stageZoneX(clientX) {
    var r = $('vStage').getBoundingClientRect();
    var x = clientX - r.left;
    if (x < r.width / 3) return -1;
    if (x > r.width * 2 / 3) return 1;
    return 0;
  }

  // 图片手势：只处理滚轮缩放；平移与切换由 vWrap 的滑动逻辑统一处理。
  //
  // 2.4.13：**双击不再挂在这里**，改挂到 vWrap 上（一次注册，见下面）。
  //   原来挂在每个 <img> 上 ⇒ 双击只在"图片像素上"才生效：长图上下留黑边、宽图左右留黑边
  //   的地方双击完全没反应，用户会以为双击坏了；而"中间 1/3 = 缩放区"是屏幕语义，
  //   不该跟着图片边界走。
  function attachImageGestures(img) {
    img.classList.add('zoomable');
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

  // ---------- 双击放大：**只有中间 1/3**（2.4.13 按用户要求） ----------
  //
  // 三处一起改了，别只改一半：
  //   ① 监听点从 <img> 移到 .v-media-wrap：vWrap 只包着滑动轨道（右下角 1:1 / 全屏按钮是
  //      它的**兄弟节点**，天然不会误触），于是"中间 1/3 的黑边上双击"也能放大 ——
  //      这正是原先挂在 img 上时漏掉的那块区域。
  //   ② 分区用 stageZoneX（与点击切图**同一几何来源**），不再各算一套。
  //   ③ 左/右 1/3 直接 return ⇒ **不放大**。那两下 click 由 vStage 的 click 处理（各切一张）。
  //
  // 守卫按 v.media 而不是 e.target：双击可能落在 .v-vbox / 黑边上，而 transform 永远作用在
  // 当前媒体元素上；视频不支持缩放（app.js 只对 <img> 做 transform），按 tagName 拦掉。
  vWrap.addEventListener('dblclick', function (e) {
    if (e.target.closest && e.target.closest(VIEWER_CTL_SEL)) return;
    var m = v.media;
    if (!m || m.tagName !== 'IMG') return;
    if (stageZoneX(e.clientX) !== 0) return;   // ★ 左/右 1/3：不响应双击放大
    // 已放大 → 还原（这一步不需要 rect，先判掉）
    if (v.zoomed) {
      resetView();
      applyTransform();
      return;
    }
    // 2.3.0：**以双击点为中心**放大（原来只是居中放大到 2×）。
    //   做法：把"双击点相对图片中心"的偏移取负作为平移量，双击点在屏幕上保持不动。
    var rect = m.getBoundingClientRect();
    var cx = e.clientX - (rect.left + rect.width / 2);
    var cy = e.clientY - (rect.top + rect.height / 2);
    // 2.4.0：双击放大倍数按用户要求 2 → **3 倍**（再双击还原）。
    v.scale = 3; v.zoomed = true; v.actual = false;
    v.tx = -cx; v.ty = -cy;
    updateFitBtn();
    applyTransform();
    maybeUpgradeToFull();
  });

  // ---------- 跟手滑动切换（手机相册式） ----------
  // 2.4.13：**220ms 的"单击/双击判定窗"已整笔删除**（原来是 220ms 那个判定窗常量）。
  //
  //   它当初（2.3.3）是为"双击不闪"付的赎金：单击若立刻切图，双击就会
  //   "切过去 → 瞬间切回 → 放大"。但 2.3.0 起中间 1/3 **本来就不切图**，
  //   2.4.13 起双击放大又**只认中间 1/3** ⇒ 左右两侧再不需要区分单击与双击，
  //   判定窗失去作用对象，留着只会让每一次单击都慢 220ms。
  //
  //   ⇒ 现在触屏与鼠标一致：点左/右 1/3 **当帧切图**，没有任何等待。
  //   ⇒ 代价（已知并接受）：左右快速双击 = 连切两张 —— "每一下点击都算数"，
  //      这也正是本项目 1.8.x 花几个版本治好的"连点被吃掉"的反面。
  //   ⚠️ 别因为"双击会切两张"把它加回来：zzz_2413 有闸门测试盯着
  //      （Test2413TapSwitchIsSynchronous：click 返回时 prev/next 必须已调用且零定时器）。
  //
  //   顺带删掉的死代码：那个早没人引用的双击判定窗常量，以及 tap 状态对象
  //   （它的 undo/at 字段自 2.3.3 起就没人读了）。

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
  // 2.4.29：flow 打开时箭头键/左右点击都不该切图（轨道根本没建，切了也没意义）
  function next() { if (flowOpen()) return; swipeTo(1); }
  function prev() { if (flowOpen()) return; swipeTo(-1); }

  function closeViewer() {
    showListHint(false); // 2.0.0：关掉查看器时顺手收起可能还挂着的加载提示
    // 2.4.29：若当前是竖图连续流，先把它收掉并记下"看到第几张"。
    // 关闭动作本身仍走下面这套（resume 缩略图 / 历史回退 / 恢复网格），一行不跳 ——
    // 换浏览形态不该改变"返回"的语义，这正是 2.3.0 那条历史守卫的意义。
    var flowPath = '';
    if (flowOpen()) {
      try { flowPath = window.MVFlow.close() || ''; } catch (e) { flowPath = ''; }
    }
    v.mode = 'slide';
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
    //   · 若是**主动关闭** ⇒ 那条历史还在栈顶，不补一次 back 的话，用户下一次按返回
    //     会**直接退掉一层目录**（多退一层）。
    //   ⇒ 一句话：**我们压的条目，只有我们自己收得回来。**
    //   退完那次 popstate 会进 mvOnPop，那时查看器已经隐藏 ⇒ 它什么都不会做。
    if (PROFILE === 'touch' && !mvViewerFromPop && !singleMode) {
      try {
        if (history.state && history.state.mvViewer) history.back();
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

    // 2.4.29：从竖图流退出来 ⇒ 把网格滚回"你看的那一张"（并闪一下）。
    // 必须在 resumeGridRequests 之后：滚动会触发 renderVisible → 建格子 → 发缩略图请求，
    // 顺序反了会在"请求被挂起"的状态下建一批格子，它们永远等不到图。
    // 2.4.41：流内部会按当前排序重排，所以**不能**再用"下标对下标"换算（重排后下标全变）。
    //   用 path 反查网格里的位置：点哪张就回到哪张。
    if (flowPath) {
      var gi = -1;
      for (var fi = 0; fi < state.items.length; fi++) {
        if (state.items[fi] && state.items[fi].path === flowPath) { gi = fi; break; }
      }
      if (gi >= 0) scrollToItem(gi, true);
    }
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
      // flow 打开时对 touch.js 报"查看器没开"：捏合只服务轨道里的单张图，
      // 竖图流里既没有可缩放的 v.media，也不该被捏合抢走上下滚动。
      viewerOpen: function () { return !viewer.classList.contains('hidden') && !flowOpen(); },
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

  // 点击左/右 1/3 切换（滑动后不触发，避免误触）
  //
  // 2.4.13：**零延迟** —— 这里不再有任何 setTimeout。
  //   原来触屏档要走 220ms 判定窗（怕把双击的第一下切走），现在中间 1/3 既不切图、
  //   又独占双击放大，左右两侧的双击就按"两次点击"处理，判定窗整笔删掉。
  animateTransform(false); $('vStage').addEventListener('click', function (e) {
    // 2.4.29：竖图流打开时，点击归 flow 自己（切 HUD），这里一律不处理 ——
    // 否则"点一下想收起悬浮件"会顺手切换一张图。
    //（写成 typeof 守卫而不是裸调 flowOpen()：zzz_2230/2310/2413 会把这段抠出来单独 eval，
    //  独立环境里既没有那个局部函数、也没有 window —— typeof 对未声明标识符是安全的。）
    if (typeof flowOpen === 'function' && flowOpen()) return;
    // 刚结束的多指手势（捏合 / 边缘返回）浏览器仍可能补一个 click —— 抑制掉，
    // 否则"捏合松手"会顺手翻一页。时间窗 400ms，过期自动失效（不会残留吞掉下一次点击）。
    if (Date.now() < tgesture.muteClickUntil) return;
    // pointerup 已经按点击处理过（见 endSwipe 的 dir === 0 分支），别再切一次
    if (swipe.suppressClick) { swipe.suppressClick = false; return; }
    if (e.target.closest(VIEWER_CTL_SEL)) return;
    // ★ 顺序要紧：**先判"这是不是一次点击"，再决定要不要归位。**
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
    // 2.3.0：**屏幕分三份**（图片与视频统一）：左 1/3 = 上一张；中间 = 什么都不做；右 1/3 = 下一张。
    //
    // 中间为什么必须留空：双击的两下 click 都命中同一个区域，中间若也切图，
    //   双击就会"先切一张/两张、再放大"（用户实测报过）。留空之后，
    //   **不需要判定窗也能区分单击与双击** —— 这正是 2.4.13 能删掉 220ms 的前提。
    // 分区收敛到 stageZoneX()：它是"屏幕三份"的唯一来源，dblclick 放大也用它，
    //   别再在这里就地重算 _left/_right（两个来源必然漂移）。
    var zx = stageZoneX(e.clientX);
    if (zx === 0) return;   // 中间 1/3：无论缩放，什么都不做（双击放大由 vWrap 的 dblclick 负责）
    // 2.3.3：**没有"放大到 2× 以上时单击先归位"**（用户明确要求：放大后点击直接切图）。
    // 2.4.1：**手机版放大后不响应左右切图**（用户要求）。
    //   放大是为了看细节 —— 此时点左右应该什么都不做，只能拖动查看；
    //   切图必须先缩回原状（双击中间还原）再点。桌面版不受影响（鼠标精确，直接切更顺手）。
    if (PROFILE === 'touch' && v.scale > 1.01) return;
    // 2.4.13：**当帧切图**。触屏与鼠标走同一条路 —— 每一下点击都算数，
    //   所以左右快速双击 = 连切两张（见上面「跟手滑动切换」一段的取舍说明）。
    if (zx < 0) prev(); else next();
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
  // 小视图（格子 96px）要让名称字号收一档。
  // 大视图格子 180px，15px 的名称条占 20% 很合适；但同一个 15px 放到 96px 的格子里，
  // 名称条会占到近四成，封面被挤成一条 —— 名称不该抢封面的戏。
  // 用一个 body class 让 CSS 跟着切（不改 DOM 结构，切换时也无需重建格子）。
  function syncCellModeClass() {
    if (!document.body) return;
    if (state.cellMode === 'small') document.body.classList.add('mv-small');
    else document.body.classList.remove('mv-small');
  }
  $('sizeLarge').onclick = function () {
    if (state.cellMode === 'large') return;
    state.cellMode = 'large';
    try { localStorage.setItem('mediaview_cellMode', 'large'); } catch(e) {}
    this.classList.add('active'); $('sizeSmall').classList.remove('active');
    syncCellModeClass();
    layout(true);
  };
  $('sizeSmall').onclick = function () {
    if (state.cellMode === 'small') return;
    state.cellMode = 'small';
    try { localStorage.setItem('mediaview_cellMode', 'small'); } catch(e) {}
    this.classList.add('active'); $('sizeLarge').classList.remove('active');
    syncCellModeClass();
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
  syncCellModeClass();

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
    cfg.coverEnabled = s.coverEnabled !== false;
    cfg.coverNameBelow = s.coverNameBelow === true;
    // 2.4.29：缺字段时按"开"处理（与后端默认一致；老版本保存回来的 JSON 里没有这一项）
    cfg.mobileFlowView = s.mobileFlowView !== false;
    cfg.onlyRoots = !!s.onlyRoots;
    cfg.roots = Array.isArray(s.roots) ? s.roots.slice() : [];
    cfg.rootsInfo = Array.isArray(s.rootsInfo) ? s.rootsInfo : [];
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
    setRootHint('');
    $('dirBrowser').classList.add('hidden');
    // 表单填好之前禁止保存：此刻 DOM 里全是 HTML 静态默认值（缩略图未勾选、尺寸 160、画质 60），
    // 而后端是**整份替换** —— 手快在 GET 返回前点一次保存，就会把用户配置静默重置掉。
    var btn = $('settingsSave');
    btn.disabled = true;
    fetchJSON(API + '/settings').then(function (s) {
      applySettings(s);
      fillSettingsForm();
      btn.disabled = false;
      coverPregenRestore();   // 后台任务的进度与按钮状态跟着面板一起恢复
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
    $('setCover').checked = cfg.coverEnabled;
    $('setCoverName').checked = cfg.coverNameBelow;
    // 容错：旧 index.html 配新 app.js 时这个开关不存在，不能因此让整个设置面板打不开
    var mfEl = $('setMobileFlow');
    if (mfEl) mfEl.checked = cfg.mobileFlowView !== false;
    $('setOnlyRoots').checked = cfg.onlyRoots;
    pendingRoots = (cfg.roots || []).slice();
    renderRootsList();
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

  // ---- 已添加的文件夹（「只显示我添加的文件夹」）----
  //
  // pendingRoots 是设置面板里的**工作副本**：增删只改它，点「保存」才写回服务端。
  // 这样点错了还能取消 —— 直连保存的话一次误触就改掉了整个可见范围。
  var pendingRoots = [];
  var dbPickTarget = 'thumbDir'; // 目录选择器当前用途：'thumbDir' | 'root'

  // 目录是否还在：状态来自服务端 rootsInfo（前端拿不到文件系统）。
  // 刚添加、还没保存过的条目不在 rootsInfo 里 —— 先按"可访问"显示。
  function rootExists(path) {
    for (var i = 0; i < (cfg.rootsInfo || []).length; i++) {
      var r = cfg.rootsInfo[i];
      if (r && r.path === path) return !!r.exists;
    }
    return true;
  }

  function setRootHint(t) {
    var el = $('rootHint');
    if (el) el.textContent = t || '';
  }

  function renderRootsList() {
    var box = $('rootList');
    if (!box) return;
    box.innerHTML = '';
    if (!pendingRoots.length) {
      var e = document.createElement('div');
      e.className = 'root-empty';
      e.textContent = '还没有添加文件夹。开关关闭时这份列表不生效。';
      box.appendChild(e);
      return;
    }
    pendingRoots.forEach(function (p, idx) {
      var row = document.createElement('div');
      row.className = 'root-item' + (rootExists(p) ? '' : ' missing');
      var ico = document.createElement('span');
      ico.className = 'root-ico';
      ico.textContent = rootExists(p) ? '📁' : '⚠️';
      var txt = document.createElement('span');
      txt.className = 'root-text';
      var name = document.createElement('span');
      name.className = 'root-name';
      name.textContent = baseName(p);
      var path = document.createElement('span');
      path.className = 'root-path';
      path.textContent = p;
      path.title = p;
      txt.appendChild(name);
      txt.appendChild(path);
      var del = document.createElement('button');
      del.className = 'btn ghost mini root-del';
      del.textContent = '移除';
      del.onclick = function () {
        pendingRoots.splice(idx, 1);
        renderRootsList();
        setRootHint('已移除，点「保存」生效');
      };
      row.appendChild(ico);
      row.appendChild(txt);
      row.appendChild(del);
      box.appendChild(row);
    });
  }

  // ---- directory browser ----
  // 面板的宿主：点哪个按钮，就在那个区块里展开。
  //
  // 早期版本把面板固定放在设置主体末尾，结果点「添加文件夹」会滚到「缩略图缓存」
  // 那块（清空缓存 / 清除所有数据）的下面 —— 用户看到的是"缩略图区域"，以为跳错了地方。
  // 就地展开没有这个歧义：按钮下面紧接就是目录列表。
  function browserHostFor(target) {
    return $(target === 'root' ? 'rootBrowserHost' : 'thumbBrowserHost');
  }

  function openBrowser(startPath) {
    var box = $('dirBrowser');
    var host = browserHostFor(dbPickTarget);
    if (host && box.parentNode !== host) {
      // appendChild 会把节点**移过来**（不是复制）：只有一份 DOM/状态，
      // 浏览到哪一层、列表内容都跟着走，不用维护两套选择器。
      host.appendChild(box);
    }
    box.classList.remove('hidden');
    // 按钮文案跟着用途走：两处入口共用同一个面板，不标出来很容易点错
    // （在缩略图入口点"添加此文件夹"会以为加进了列表，其实写进了输入框）。
    $('dbPick').textContent = dbPickTarget === 'root' ? '添加此目录' : '用此目录';
    // 就地展开后可能贴着视口底部：nearest 只在需要时滚动，已可见时不动。
    try { box.scrollIntoView({ block: 'nearest' }); } catch (e) { box.scrollIntoView(); }
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
      coverEnabled: $('setCover').checked,
      coverNameBelow: $('setCoverName').checked,
      // 2.4.29：手机端竖图连续流（桌面不生效，但值照样存 —— 换设备登录时状态一致）
      // 缺元素时按后端默认（开）处理，别把用户已经打开的开关悄悄关掉
      mobileFlowView: $('setMobileFlow') ? $('setMobileFlow').checked : true,
      onlyRoots: $('setOnlyRoots').checked,
      roots: pendingRoots.slice(),
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
      var prevOnly = cfg.onlyRoots;
      var prevRoots = (cfg.roots || []).join('|');
      applySettings(res.settings);
      fillSettingsForm();
      $('saveHint').textContent = '已保存';
      closeSettings();
      toast('设置已保存');
      // 画质/缩略图目录/尺寸都可能变了：换一批 URL，避免浏览器沿用旧缩略图
      bumpThumbCacheBust();
      // ★ 可见范围变了 ⇒ 必须**回首页重新取数**，不能只重绘：
      //   当前所在的目录可能已经不在范围内，服务端会对它的请求返回 403，
      //   而界面还停在原地 —— 那就是"设置改了、界面没变"。
      if (prevOnly !== cfg.onlyRoots || prevRoots !== (cfg.roots || []).join('|')) {
        state.root = false;
        state.path = null;
        buildRoot();
        return;
      }
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
  // 重建按钮按当前目录**内容分流**（2.4.26）：
  //   · 有子文件夹 → 逐个重建子文件夹的封面（POST /dircov/rebuild，并发 4）；
  //   · 有媒体文件 → 清当前层这些文件的缩略图（/thumb/clear&shallow=1，**非递归**，
  //     不碰下层），清完前台请求 + 后台入队会立刻重新生成；
  //   · 两者都有 → 两者都做。空目录直接提示。
  $('btnRebuildThumbs').onclick = function () {
    if (!state.path) { toast('请先进入一个目录'); return; }
    var dirs = [], nFiles = 0;
    (state.items || []).forEach(function (it) {
      if (!it || !it.path) return;
      if (it.kind === 'dir') dirs.push(it.path);
      else nFiles++;
    });
    var doCover = dirs.length > 0 && cfg.coverEnabled;
    var doThumb = nFiles > 0 && cfg.thumbEnabled;
    if (!doCover && !doThumb) { toast('当前目录没有可重建的内容'); return; }
    var parts = [];
    if (doCover) parts.push(dirs.length + ' 个子文件夹的封面');
    if (doThumb) parts.push(nFiles + ' 个文件的缩略图');
    if (!confirm('重建当前目录？\n\n' + parts.join('，以及') + '\n\n' + state.path)) return;

    var coverOk = 0, coverFail = 0, thumbOk = false;
    var jobs = [];
    if (doCover) {
      // 并发 4：与格子缩略图的并发纪律对齐，再高只会跟前台抢磁盘/CPU
      var CONC = 4, idx = 0;
      function worker() {
        if (idx >= dirs.length) return Promise.resolve();
        var p = dirs[idx++];
        return fetch(API + '/dircov/rebuild?path=' + encodeURIComponent(p) +
          '&size=' + (cfg.thumbSize || 320), { method: 'POST' })
          .then(function (r) { if (r.ok) coverOk++; else coverFail++; })
          .catch(function () { coverFail++; })
          .then(worker);
      }
      for (var i = 0; i < Math.min(CONC, dirs.length); i++) jobs.push(worker());
    }
    var chain = Promise.all(jobs).then(function () {
      if (!doThumb) return;
      // shallow=1：只清当前层文件的缩略图，不递归下层（下层交给各自的"重建"去管）
      return fetch(API + '/thumb/clear?path=' + encodeURIComponent(state.path) + '&shallow=1', { method: 'POST' })
        .then(function (r) {
          return r.text().then(function (t) {
            var d;
            try { d = JSON.parse(t); } catch (e) { d = { ok: false }; }
            thumbOk = !!d.ok;
          });
        })
        .catch(function () { thumbOk = false; });
    });
    chain.then(function () {
      var msg = [];
      if (doCover) {
        msg.push('封面重建 ' + coverOk + ' 个' + (coverFail ? '（失败 ' + coverFail + '）' : ''));
      }
      if (doThumb) {
        msg.push(thumbOk ? '缩略图已清，正在重新生成…' : '缩略图清除失败');
      }
      toast(msg.join('；'));
      bumpThumbCacheBust();
      setTimeout(function () { loadDir(state.path); }, 300);
    });
  };
  $('settingsClose').onclick = closeSettings;
  $('settingsCancel').onclick = closeSettings;
  $('settingsMask').onclick = closeSettings;
  $('settingsSave').onclick = saveSettings;

  // ---------- 一键生成文件夹封面 ----------
  // 与按需生成共用同一套磁盘缓存（服务端同 buildCover / 同 dircov 目录），
  // 所以重复点不会重做，已生成目录直接跳过（幂等）。
  var coverPregenTimer = null;
  var coverPregenWasRunning = false;

  function coverPregenStart() {
    // 起始目录 = 当前浏览的目录；在聚合根/还没进目录时从头开始
    var startPath = (state.root || !state.path) ? '/' : state.path;
    var url = API + '/dircov/pregen?path=' + enc(startPath) +
      '&size=' + (cfg.thumbSize || 320);
    var el = $('coverPregenStatus');
    if (el) el.textContent = '准备中…';
    fetch(url, { method: 'POST' }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error((t || '').trim()); });
      return r.json();
    }).then(function () {
      coverPregenWasRunning = false;
      coverPregenPoll();
    }).catch(function (e) {
      if (el) el.textContent = '启动失败：' + (e.message || '');
      toast('一键生成启动失败：' + (e.message || ''));
    });
  }

  // coverPregenDoneText 完成文案。
  //
  // truncated 是服务端给的"目录清单撞到安全阀、还有目录没排进来"——
  // **必须说出来**：2.4.45 之前上限固定 3000 且截断是静默的，界面只显示
  // "已完成：新生成 N 个"，用户以为全做完了，实际后面几千个文件夹永远没封面。
  function coverPregenDoneText(s) {
    var msg = '已完成：新生成 ' + (s.generated || 0) + ' 个，跳过 ' + (s.skipped || 0) + ' 个';
    if (s.failed) msg += '，失败 ' + s.failed + ' 个';
    if (s.truncated) {
      msg += '。目录太多，本次只排到前 ' + (s.total || 0) +
        ' 个目录：再点一次「开始」继续（已生成的会跳过）';
    }
    return msg;
  }

  function coverPregenPoll() {
    if (coverPregenTimer) { clearTimeout(coverPregenTimer); coverPregenTimer = null; }
    fetchJSON(API + '/dircov/pregen').then(function (s) {
      var el = $('coverPregenStatus');
      var btn = $('btnCoverPregen');
      if (!s || !s.running) {
        if (btn) btn.textContent = '开始';
        if (s && s.finished) {
          var msg = coverPregenDoneText(s);
          if (el) el.textContent = msg;
          toast(msg);
          // 从「生成中」→「完成」这一次刷新网格，让刚生成的封面显示出来
          if (coverPregenWasRunning) { coverPregenWasRunning = false; redraw(); }
        }
        return;
      }
      coverPregenWasRunning = true;
      var pct = s.total > 0 ? Math.floor(s.done * 100 / s.total) : 0;
      if (el) el.textContent = '生成中 ' + s.done + ' / ' + (s.total || '?') + '（' + pct + '%）';
      if (btn) btn.textContent = '取消';
      coverPregenTimer = setTimeout(coverPregenPoll, 800);
    }).catch(function () {
      coverPregenTimer = setTimeout(coverPregenPoll, 1500);
    });
  }

  // 每次打开设置页都恢复一次进度显示：一键生成跑在**服务端后台**，
  // 关掉面板 / 刷新页面都不该中断它。不恢复的话按钮会退回「开始」，
  // 用户以为任务被取消了，再点一次只会得到「已经在一键生成中」。
  function coverPregenRestore() {
    fetchJSON(API + '/dircov/pregen').then(function (s) {
      if (!s) return;
      var el = $('coverPregenStatus');
      var btn = $('btnCoverPregen');
      if (s.running) {
        coverPregenWasRunning = true;
        if (btn) btn.textContent = '取消';
        coverPregenPoll();
        return;
      }
      if (btn) btn.textContent = '开始';
      if (s.finished && el) {
        el.textContent = coverPregenDoneText(s);
      }
    }).catch(function () {});
  }

  function coverPregenCancel() {
    fetch(API + '/dircov/pregen/cancel', { method: 'POST' }).then(function () {
      if (coverPregenTimer) { clearTimeout(coverPregenTimer); coverPregenTimer = null; }
      coverPregenWasRunning = false;
      var el = $('coverPregenStatus');
      if (el) el.textContent = '已取消（已生成的部分仍然有效）';
      var btn = $('btnCoverPregen');
      if (btn) btn.textContent = '开始';
    }).catch(function () {});
  }

  if ($('btnCoverPregen')) {
    $('btnCoverPregen').onclick = function () {
      if (this.textContent === '取消') coverPregenCancel();
      else coverPregenStart();
    };
  }
  $('setThumbEnabled').onchange = syncThumbOpts;
  $('setTheme').onchange = function () {
    applyTheme(this.value);
  };
  $('btnBrowse').onclick = function () {
    var box = $('dirBrowser');
    // 只有"开着且用途就是本处"才收起；否则切换用途（共用面板，不能一律关掉）
    if (!box.classList.contains('hidden') && dbPickTarget === 'thumbDir') {
      box.classList.add('hidden');
      return;
    }
    dbPickTarget = 'thumbDir';
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
    if (!browsePath) return;
    if (dbPickTarget === 'root') {
      if (pendingRoots.indexOf(browsePath) >= 0) {
        setRootHint('这个文件夹已经在列表里了');
      } else {
        pendingRoots.push(browsePath);
        renderRootsList();
        setRootHint('已添加：' + browsePath + '（点「保存」生效）');
        // 列表在面板上方，选择器在底部 —— 不滚回去用户看不到刚加的那一项，
        // 会以为还是没加上，于是一直点。
        try { $('rootList').scrollIntoView({ block: 'nearest' }); } catch (e) {}
      }
    } else {
      $('setThumbDir').value = browsePath;
      $('dirHint').textContent = '已选择：' + browsePath;
    }
    $('dirBrowser').classList.add('hidden');
  };
  $('btnAddRoot').onclick = function () {
    var box = $('dirBrowser');
    // 已经开着且用途就是本处 ⇒ 再点一次收起（与「选择目录」按钮同一套手感）
    if (!box.classList.contains('hidden') && dbPickTarget === 'root') {
      box.classList.add('hidden');
      return;
    }
    dbPickTarget = 'root';
    setRootHint('');
    openBrowser(null); // 从"存储空间聚合层"起步：这里本来就要挑别的目录
  };
  $('dbClose').onclick = function () {
    $('dirBrowser').classList.add('hidden');
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

  // 2.4.40：**第一条历史条目必须自带目录**。
  //
  //   从桌面图标进来时 URL 是 `/app/mediaview`（没有 ?dir=）。就那样停着的话，
  //   用户点进第一个文件夹再按返回，回到的是一条"没有目录"的页面 —— 它只能靠
  //   localStorage 猜目录，很可能又落回同一个文件夹（看起来像"返回没反应"）。
  //   这里用 replaceState 把**当前这条条目改名**成显式目录：不重载、不新增条目。
  function fixEntryURL(dir) {
    if (qsHadDir) return;   // 已经带 ?dir=（真导航落地的页面）：URL 本身就是真相，别动它
    try {
      history.replaceState(null, '', GW + '/?dir=' + encodeURIComponent(dir || '/'));
    } catch (e) { /* 沙箱里不让改 URL：只是这一层不能靠 URL 恢复，不影响功能 */ }
  }

  var qsHadDir = false;

  function boot() {
    var qs = new URLSearchParams(location.search);
    var single = qs.get('path');
    qsHadDir = qs.get('dir') !== null;
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
      // 2.4.31：**不等 settings**，先把目录快照铺上。
      //   快照里只有 items（网格内容），而 layout 只用得到 cellMode（localStorage）
      //   和容器宽度 —— 都不依赖设置接口。所以"回到刚看过的目录"可以做到
      //   **接口还没回、画面已经在**，这才是真正的"不闪"。
      //   loadDir 里还有一次快照应用，靠 sameItems 判重避免重复布局。
      var earlySnap = loadSnap(dirParam, state.sort);
      if (earlySnap) {
        state.items = earlySnap.items;
        state.files = earlySnap.files || [];
        renderCrumb();
        // 连滚动位置一起恢复：整页重载回来时也停在"你刚才看到的那一屏"
        applyItems(null, earlySnap.top || 0);
      }
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
      // 2.4.16：开启「只显示我添加的文件夹」时**不恢复上次目录**。
      // 启动就该落在"我的文件夹"这一层：旧地址可能已不在可见范围内，
      // 恢复它要么先吃一个 403 再回退、要么把用户送进一个他刚表示不想看到的位置。
      if (last && !cfg.onlyRoots) {
        fixEntryURL(last);
        fetchJSON(API + '/list?path=' + enc(last)).then(function () { loadDir(last); })
          .catch(function () { fixEntryURL('/'); buildRoot(); });
      } else {
        fixEntryURL('/');
        buildRoot();
      }
    });
  }
  // 关窗 / 刷新 / 宿主销毁页面时立即恢复缩略图生成，避免暂停状态残留到 TTL 到期。
  // beforeunload 与 pagehide 都挂：移动端与部分宿主里前者不触发，后者更标准；
  // pagehide 加了 persisted 判断，进 bfcache（页面没销毁、返回后查看器还开着）时不误恢复。
  window.addEventListener('beforeunload', flushResumeThumbGen);
  // 2.4.40：真导航 / 宿主手势返回都会卸载页面 —— 离开前把"看到第几屏"写进快照，
  //   返回时才能停回原处（navigateTo 里也调了一次；这里兜住不由我们发起的那种离开）。
  window.addEventListener('pagehide', function (e) {
    try { rememberScroll(); } catch (e2) {}
    if (!e.persisted) flushResumeThumbGen();
  });
  boot();
})();
