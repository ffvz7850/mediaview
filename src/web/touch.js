/*
 * 触屏手势增强（mediaview 触屏版）
 * ============================================================
 * **只在对触屏档位动态注入**，桌面永不下载、永不执行。
 * 注入方在 app.js：
 *     if (PROFILE === 'touch') { 动态 <script src="touch.js?v=<版本>"> }
 *
 * ⚠️ 禁止在 index.html 里写静态 `<script src="touch.js">`：静态标签会让**桌面**
 *    也下载并执行这一整块手势代码，那就"搅和"了。zzz_2100 有闸门测试禁止这种写法。
 *
 * 本模块**只做一件事：双指捏合缩放**。
 *   · app.js 保留：单指跟手切图、放大后单指平移、点击左/右半区切图、滚轮缩放、Esc/方向键
 *   · 本模块新增：双指捏合缩放（1×~6×，焦点保持）
 * 交集处靠 app.js 的「让位」机制解决：本模块 claimGesture() 之后，app.js 对**本次手势**
 * 的单指逻辑整体退出（不让位就会出现"两指捏合却翻页"）。
 *
 * ⛔ **本模块不再做"右边缘横滑返回"**（2.2.0 按用户要求整块删掉，别再补回来）。
 *    原因有两层：
 *      ① 那条边缘与**宿主自带的返回手势**重合。宿主的在原生层、我们拦不住也抢不赢，
 *         同一划被两方同时判定，谁先成功就谁作数 —— 真机上表现为"快滑一种结果、
 *         慢滑另一种结果"，用户实测报过多次；
 *      ② 返回本来就有**页面内的显式入口**（左上角箭头），语义确定、不看速度、不看落点。
 *    ⇒ 返回动作不再依赖任何手势。相关说明见 app.js 的「返回 / 退出」一段。
 *
 * 依赖：window.__mvMobile（只由 app.js 在触屏档位创建，是三层隔离里的"契约层"）。
 *   拿不到它就什么都不装 —— 宁可不生效，也不要"半套手势"（两个文件理论上可能版本错配）。
 *
 * 本模块**不碰**的东西（刻意的）：
 *   · 不改 app.js 的内部状态，一切读写都走 __mvMobile 暴露的窄接口；
 *   · 不做双击缩放 —— 原因见文件末尾「为什么没有双击缩放」。
 */
(function () {
  'use strict';

  // 这个标记只表示"文件被加载了"，用于验证桌面档位根本没加载它。
  window.__mvTouchLoaded = true;

  var READY_TRIES = 40;     // 最多等 40 × 50ms = 2s
  var READY_GAP_MS = 50;
  var tries = 0;

  // app.js 是**同步**执行完整个 IIFE 才轮到本文件（同一个事件循环任务），
  // 所以正常顺序下接口一定已经在了。这里仍然加一层轮询，是因为"脚本顺序"是
  // 实现细节，不值得让手势功能去赌它 —— 等不到就安静退出，绝不留半个手势在跑。
  (function waitApi() {
    if (window.__mvMobile) { install(window.__mvMobile); return; }
    if (++tries > READY_TRIES) {
      try { console.warn('[mv-touch] 等不到 __mvMobile，手势增强未启用'); } catch (e) {}
      return;
    }
    window.setTimeout(waitApi, READY_GAP_MS);
  })();

  function install(API) {
    if (typeof API.setView !== 'function' || typeof API.viewerOpen !== 'function') {
      try { console.warn('[mv-touch] __mvMobile 接口不完整，手势增强未启用'); } catch (e) {}
      return;
    }

    var stage = document.getElementById('vStage');
    if (!stage) {
      try { console.warn('[mv-touch] 找不到查看器舞台节点，手势增强未启用'); } catch (e) {}
      return;
    }

    // ---------------- 状态 ----------------
    var pointers = {};    // pointerId -> {x, y}：当前按在查看器上的触点
    var pinch = null;     // 当前生效的捏合会话（只用它做计算）
    var pinchUsed = false; // 本次手势**发生过**捏合 —— 松手时要据此定稿一次。
                           // 必须与会话分开：手指从 2 根降到 1 根时会话要作废（否则
                           // 剩下那根手指会把画面带偏），但"发生过捏合"这个事实不能丢，
                           // 否则松手时不会 commitView（≥1.5x 的原图升级就不触发了）。
    var claimed = false;  // 本次手势是否已向 app.js 声明"让位"

    function count() {
      var n = 0;
      for (var k in pointers) { if (pointers[k]) n++; }
      return n;
    }
    function ids() {
      var a = [];
      for (var k in pointers) { if (pointers[k]) a.push(k); }
      return a;
    }
    function dist(a, b) {
      var dx = a.x - b.x, dy = a.y - b.y;
      return Math.sqrt(dx * dx + dy * dy);
    }
    function mid(a, b) { return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 }; }
    function local(rect, p) { return { x: p.x - rect.left, y: p.y - rect.top }; }

    function claim() {
      if (claimed) return;
      claimed = true;
      try { API.claimGesture(); } catch (e) {}
    }
    function release() {
      if (!claimed) return;
      claimed = false;
      try { API.releaseGesture(); } catch (e) {}
    }
    // 一次手势的所有痕迹都清掉（查看器被关掉、窗口失焦、事件丢失时兜底）
    function reset() {
      pointers = {};
      pinch = null;
      pinchUsed = false;
      release();
    }

    // ---------------- 双指捏合 ----------------
    function beginPinch() {
      var a = ids();
      if (a.length < 2) { pinch = null; return; }
      var rect = API.stageRect();
      if (!rect) { pinch = null; return; }
      var cur = API.view();
      pinch = {
        ids: [a[0], a[1]],
        startDist: dist(pointers[a[0]], pointers[a[1]]) || 1,
        startScale: cur.scale,
        startTx: cur.tx,
        startTy: cur.ty,
        startMid: mid(pointers[a[0]], pointers[a[1]])
      };
      // 视频不支持缩放（app.js 只会对 <img> 做 transform）——
      // 先探一次，免得整段捏合过程都在空转、且松手时误报"已完成"。
      pinch.usable = API.setView(cur.scale, cur.tx, cur.ty) !== false;
      if (pinch.usable) pinchUsed = true;
    }

    function updatePinch() {
      if (!pinch || !pinch.usable) return;
      var A = pointers[pinch.ids[0]], B = pointers[pinch.ids[1]];
      if (!A || !B) return;
      var rect = API.stageRect();
      if (!rect) return;
      var ratio = dist(A, B) / pinch.startDist;
      var want = Math.max(1, Math.min(API.maxScale(), pinch.startScale * ratio));
      // 焦点保持：两指中点下的那个图像像素，在缩放前后停在同一屏幕位置。
      // 推导（transform: translate(t) scale(s)，transform-origin = 元素中心 C）：
      //   屏幕点 p 对应图像点 q = (p - C - t) / s
      //   要求 q 不变、s→s'，p 取当前中点 p' ⇒ t' = p' - C - (p - C - t) · (s'/s)
      // C 用 stage 中心近似：`.v-media-wrap` 铺满 `.v-stage`、图片在其中居中
      //（CSS max-width/max-height:100% + 居中），所以"图片中心 ≈ stage 中心"。
      // 极端长宽比下焦点会略偏，但 app.js 的 applyTransform 之后还有边界钳位兜底。
      var p = local(rect, mid(A, B));
      var p0 = local(rect, pinch.startMid);
      var c = { x: rect.width / 2, y: rect.height / 2 };
      var k = want / pinch.startScale;
      API.setView(
        want,
        (p.x - c.x) - ((p0.x - c.x) - pinch.startTx) * k,
        (p.y - c.y) - ((p0.y - c.y) - pinch.startTy) * k
      );
    }

    // ---------------- 事件 ----------------
    function onDown(e) {
      // 捏合只在查看器里有意义（网格里没有可缩放的对象），不打开就不接管。
      if (!API.viewerOpen()) { reset(); return; }
      // 控件上的指针事件不参与手势
      if (e.target.closest && e.target.closest('.v-controls')) return;
      if (pointers[e.pointerId]) return;                        // 重复 down，忽略
      pointers[e.pointerId] = { x: e.clientX, y: e.clientY };

      // 单指：什么都不做 —— 单指滑动切图归 app.js，这里**不 claim**（一 claim 就抢走了）。
      if (count() === 1) return;

      // 第二个触点落下：改判为捏合。
      claim();
      beginPinch();
    }

    function onMove(e) {
      var p = pointers[e.pointerId];
      if (!p) return;
      p.x = e.clientX;
      p.y = e.clientY;

      if (count() >= 2) updatePinch();
    }

    function onUp(e) {
      if (!pointers[e.pointerId]) return;
      var hadPinch = pinchUsed;
      delete pointers[e.pointerId];

      var left = count();
      if (left >= 2) {           // 三指松开一指：以剩下的两指重置基准
        pinch = null;
        beginPinch();
        return;
      }
      if (left === 1) {
        // 捏合结束但还剩一指：**不让剩下那根手指变成新的滑动起点**
        // （否则松手瞬间画面会跳一下）。app.js 仍在让位状态，这里只作废会话。
        pinch = null;
        return;
      }
      // 全部抬起
      pinch = null;
      pinchUsed = false;
      release();
      if (hadPinch) {
        try { API.commitView(); } catch (e2) {}   // ≥1.5x 触发原图升级，与滚轮同一条链路
      }
    }

    stage.addEventListener('pointerdown', onDown, { passive: true });
    stage.addEventListener('pointermove', onMove, { passive: true });
    stage.addEventListener('pointerup', onUp, { passive: true });
    stage.addEventListener('pointercancel', onUp, { passive: true });
    // 查看器被关掉 / 窗口失焦时兜底清干净，不留"半截手势"影响下一次
    window.addEventListener('blur', reset);

    // 暴露一点诊断信息：真机上排查"手势到底装了没有"用得上
    window.__mvTouchReady = {
      version: API.version,
      gestures: 'pinch-only'
    };
  }

  /*
   * 为什么没有双击缩放（别以为是漏了）
   * ------------------------------------------------
   * app.js 的 endSwipe 在**第一下** pointerup 就按"点击左/右半区"切了图，
   * 所以加双击识别必须把单击延迟 ~300ms 才能分辨两者 —— 那会让手机上**每一次切图
   * 都变钝**，代价明显大于收益。
   * 要做就得连"单击语义"一起改（推荐：手机上单击 = 显隐界面控件、双击 = 缩放、
   * 切图只靠滑动），那是交互取舍而不是实现细节，需单独确认后再做。
   * 现有的替代已经很顺：捏合放大 → 在 ≥2 倍时**单击即复位**（app.js 原有逻辑）。
   */
})();
