/*
 * 竖图流「到底加载的是哪张图」取证（真实 app）
 *
 * 目的：确认 vf-item 里最终显示的是 /api/raw（大图链路）还是 /api/thumb（缩略图），
 *       以及 full 层是否真的可见（opacity/complete/naturalWidth）。
 * 输出：
 *   · resources：本页所有 /raw 与 /thumb 请求 URL（截断到可读）
 *   · items：每个挂载的 vf-item 里 lqip / full 两张 img 的状态
 *   · viewer：同一目录下用大图查看器打开时的 img src，用来对照「同一条链路」
 *
 * 用法：node tools/flow_img_probe.js http://127.0.0.1:PORT/app/mediaview/ "<目录绝对路径>"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8130/app/mediaview/';
const DIR = process.argv[3] || '';
const PORT = 9700 + Math.floor(Math.random() * 200);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function openSession(mobile) {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-img-' + Date.now() + '-' + (mobile ? 'm' : 'd')),
    '--remote-debugging-port=' + PORT,
    mobile ? '--window-size=390,844' : '--window-size=1200,800',
    'about:blank'
  ], { stdio: 'ignore' });

  let list = [];
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch('http://127.0.0.1:' + PORT + '/json/list');
      list = await r.json();
      if (list.some(t => t.type === 'page')) break;
    } catch (e) {}
    await sleep(250);
  }
  const page = list.filter(t => t.type === 'page')[0];
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

  let id = 0;
  const pending = new Map();
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  };
  const send = (method, params) => {
    const mid = ++id;
    ws.send(JSON.stringify({ id: mid, method, params: params || {} }));
    return new Promise(res => pending.set(mid, res));
  };
  const evalJS = async expr => {
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
    return r.result.result.value;
  };

  await send('Runtime.enable');
  await send('Page.enable');
  if (mobile) {
    await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
    await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  }
  const url = DIR ? URL_BASE + '?dir=' + encodeURIComponent(DIR) : URL_BASE;
  await send('Page.navigate', { url });
  await sleep(2500);

  const out = {};
  out.settings = await evalJS(`(function(){
    var b = document.getElementById('btnSettings'); if (b) b.click();
    return { rowDisplay: (function(){ var r = document.getElementById('rowMobileFlow'); return r ? getComputedStyle(r).display : 'missing'; })(),
             cells: document.querySelectorAll('.cell').length, search: location.search };
  })()`);
  await evalJS(`(function(){ var b=document.querySelector('#settingsMask, .settings-mask'); if (b) b.click(); return 1; })()`);

  // 进流
  // 先记一次缩略图请求数：这些是**网格**为渲染格子发的，与 flow 无关。
  // flow 打开后若这个数字还涨，就说明竖图流又在偷偷拿缩略图当画面（2.4.29 的老毛病）。
  out.thumbBefore = await evalJS(
    '(performance.getEntriesByType("resource")||[]).filter(function(r){return r.name.indexOf("/thumb?")>=0}).length');
  out.openFlow = await evalJS(`(function(){
    var inner = document.querySelector('.cell .cell-inner');
    if (!inner) return { error: 'no cell' };
    inner.click();
    return { clicked: true };
  })()`);
  await sleep(1600);
  out.thumbAfter = await evalJS(
    '(performance.getEntriesByType("resource")||[]).filter(function(r){return r.name.indexOf("/thumb?")>=0}).length');

  out.items = await evalJS(`(function(){
    var out = [];
    var its = document.querySelectorAll('.vf-item');
    for (var i = 0; i < its.length; i++) {
      var el = its[i];
      var rec = { i: el.getAttribute('data-i'),
                  text: (el.textContent || '').trim().slice(0, 12),
                  h: el.style.height,
                  w: Math.round(el.getBoundingClientRect().width),
                  boxRatio: +(el.getBoundingClientRect().width / el.getBoundingClientRect().height).toFixed(4),
                  hasLqip: !!el.querySelector('.vf-lqip') };
      var lq = el.querySelector('.vf-lqip');
      var fu = el.querySelector('img.vf-full');
      var pick = function (im) {
        if (!im) return null;
        var cs = getComputedStyle(im);
        return { cls: im.className, complete: im.complete,
                 nw: im.naturalWidth, nh: im.naturalHeight,
                 imgRatio: im.naturalHeight > 0 ? +(im.naturalWidth / im.naturalHeight).toFixed(4) : 0,
                 fit: cs.objectFit,
                 opacity: cs.opacity, filter: cs.filter,
                 cw: Math.round(im.getBoundingClientRect().width),
                 src: im.getAttribute('src') || '' };
      };
      rec.lqip = pick(lq);
      rec.full = pick(fu);
      out.push(rec);
    }
    return out;
  })()`);

  out.resources = await evalJS(`(function(){
    var rs = performance.getEntriesByType('resource') || [];
    var r = [], t = [];
    for (var i = 0; i < rs.length; i++) {
      var u = rs[i].name;
      if (u.indexOf('/raw?') >= 0) r.push(u.replace(/^.*\\/api/, ''));
      else if (u.indexOf('/thumb?') >= 0) t.push(u.replace(/^.*\\/api/, ''));
    }
    return { raw: r, thumb: t };
  })()`);

  // 关流，记录关流后的状态（用于确认没有残留）
  await evalJS('history.back()');
  await sleep(700);
  out.afterBack = await evalJS(`(function(){
    return { isOpen: !!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen()) };
  })()`);

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
  return out;
}

(async () => {
  const r = await openSession(true);
  console.log(JSON.stringify(r, null, 2));
})();
