/*
 * 手机档「大图浏览」性能探针 v3
 *
 * 三种动作，各自量一次主线程开销与"额外工作量"（每动作均摊）：
 *   track : 点左/右 1/3 切图 6 次  —— 量 新建 img / decode / 图片请求
 *   drag  : 一次纯拖动（30 个 move，不松手切图）—— 量 LayoutCount / RecalcStyleCount 增量，
 *           用来判断 pointermove 里有没有强制布局（读写交替 = 掉帧的经典来源）
 *   flow  : 竖图连续流滚三屏 —— 量挂载/卸载 img 次数与 decode 次数
 *
 * 用法：node swipe_probe.js <base> <dir> <track|drag|flow> <label>
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';

const BASE = process.argv[2] || 'http://127.0.0.1:8912/app/mediaview';
const DIR = process.argv[3];
const MODE = process.argv[4] || 'track';
const LABEL = process.argv[5] || MODE;
const PORT = 11200 + Math.floor(Math.random() * 80);
const sleep = ms => new Promise(r => setTimeout(r, ms));

const INSTRUMENT = `
window.__p = { imgs: 0, decodes: 0, rawReq: 0, srcSet: 0, long: [] };
(function () {
  var oc = document.createElement.bind(document);
  document.createElement = function (tag) {
    var el = oc.apply(document, arguments);
    if (String(tag).toLowerCase() === 'img') {
      window.__p.imgs++;
      var d = Object.getOwnPropertyDescriptor(HTMLImageElement.prototype, 'src');
      try {
        Object.defineProperty(el, 'src', {
          configurable: true,
          get: function () { return d.get.call(el); },
          set: function (v) { window.__p.srcSet++; d.set.call(el, v); }
        });
      } catch (e) {}
    }
    return el;
  };
  var od = HTMLImageElement.prototype.decode;
  if (od) {
    HTMLImageElement.prototype.decode = function () {
      window.__p.decodes++;
      return od.apply(this, arguments);
    };
  }
  try {
    new PerformanceObserver(function (l) {
      l.getEntries().forEach(function (e) { window.__p.long.push(Math.round(e.duration)); });
    }).observe({ entryTypes: ['longtask'] });
  } catch (e) {}
  try {
    new PerformanceObserver(function (l) {
      l.getEntries().forEach(function (e) { if (/\\/api\\/raw/.test(e.name)) window.__p.rawReq++; });
    }).observe({ entryTypes: ['resource'] });
  } catch (e) {}
})();
`;

const round = (v, n) => Math.round(v * Math.pow(10, n)) / Math.pow(10, n);

(async () => {
  const proc = spawn(EDGE, ['--headless=new', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-perf-' + Date.now()),
    '--remote-debugging-port=' + PORT, '--window-size=390,844', 'about:blank'], { stdio: 'ignore' });
  let pages = [];
  for (let i = 0; i < 60; i++) {
    try { const r = await fetch('http://127.0.0.1:' + PORT + '/json/list'); pages = await r.json(); if (pages.some(t => t.type === 'page')) break; } catch (e) {}
    await sleep(250);
  }
  const page = pages.filter(t => t.type === 'page')[0];
  if (!page) throw new Error('no devtools page');
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0; const pending = new Map();
  ws.onmessage = ev => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
  const send = (m, p) => { const mid = ++id; ws.send(JSON.stringify({ id: mid, method: m, params: p || {} })); return new Promise(r => pending.set(mid, r)); };
  const evalJS = async e => {
    const r = await send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true });
    if (r.result && r.result.exceptionDetails) throw new Error('EXC ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
    return r.result.result.value;
  };
  const metrics = async () => {
    const r = await send('Performance.getMetrics');
    const o = {}; (r.result.metrics || []).forEach(m => { o[m.name] = m.value; }); return o;
  };

  await send('Runtime.enable'); await send('Page.enable'); await send('Performance.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  await send('Page.addScriptToEvaluateOnNewDocument', { source: INSTRUMENT });

  // 网格按 mtime 降序 = 名字倒序（测试照片名字越大越新）。取正中那张，左右都有邻居。
  const names = require('fs').readdirSync(DIR).filter(n => /\.jpe?g$/i.test(n)).sort().reverse();
  const startPhoto = path.join(DIR, names[Math.floor(names.length / 2)]);
  const url = BASE + '/?dir=' + encodeURIComponent(DIR) +
    (MODE === 'flow' ? '' : '&p=' + encodeURIComponent(startPhoto));
  await send('Page.navigate', { url });
  await sleep(3600);
  const open = await evalJS(`!document.getElementById('viewer').classList.contains('hidden')`);
  if (!open) {
    // flow 模式没有深链入口：点网格正中那一格打开
    await evalJS(`(function(){ var c=document.querySelectorAll('#gridContent .cell-inner'); c[Math.floor(c.length/2)].click(); })()`);
    await sleep(2600);
  }
  await sleep(3000); // 静置：让首屏缩略图与首轮预载先跑完，别污染基线

  const state0 = await evalJS(`({
    flow: !!document.querySelector('#vfRoot.vf-on'),
    track: !!document.querySelector('.v-track'),
    boxes: document.querySelectorAll('.v-track > .v-vbox').length,
    counter: (document.getElementById('vCounterFloat')||{}).textContent,
    vfItems: document.querySelectorAll('.vf-item').length
  })`);

  const M_KEYS = ['TaskDuration', 'ScriptDuration', 'LayoutDuration', 'RecalcStyleDuration', 'LayoutCount', 'RecalcStyleCount'];
  const C_KEYS = ['imgs', 'decodes', 'srcSet', 'rawReq'];

  const m0 = await metrics();
  const c0 = await evalJS(`({imgs:window.__p.imgs,decodes:window.__p.decodes,srcSet:window.__p.srcSet,rawReq:window.__p.rawReq})`);
  await evalJS('window.__p.long = []');

  let acts = 1;
  if (MODE === 'track') {
    acts = 6;
    for (let i = 0; i < acts; i++) {
      const x = (i % 2 === 0) ? 350 : 40;
      await send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: 420, id: 1 }] });
      await send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await sleep(900);
    }
  } else if (MODE === 'drag') {
    acts = 1;
    // 只拖不放：从中间往左拖 30 帧，再看主线程做了多少次布局/样式重算
    await send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: 320, y: 420, id: 1, force: 1 }] });
    for (let k = 1; k <= 30; k++) {
      await send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: 320 - k * 7, y: 420, id: 1, force: 1 }] });
      await sleep(16);
    }
    await sleep(300);
  } else {
    acts = 3;
    for (let r = 0; r < acts; r++) {
      const x = 195, y0 = 700, y1 = 170;
      await send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: y0, id: 1, force: 1 }] });
      for (let k = 1; k <= 26; k++) {
        await send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y0 + (y1 - y0) * k / 26, id: 1, force: 1 }] });
        await sleep(16);
      }
      await send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await sleep(650);
    }
  }

  const m1 = await metrics();
  const c1 = await evalJS(`({imgs:window.__p.imgs,decodes:window.__p.decodes,srcSet:window.__p.srcSet,rawReq:window.__p.rawReq})`);
  const longs = await evalJS('window.__p.long');

  const cpu = {}, work = {};
  M_KEYS.forEach(k => {
    const d = (m1[k] || 0) - (m0[k] || 0);
    cpu[k] = (k.indexOf('Count') >= 0) ? Math.round(d / acts * 100) / 100 : round(d * 1000 / acts, 2) + 'ms';
  });
  C_KEYS.forEach(k => { work[k] = round((c1[k] - c0[k]) / acts, 2); });

  console.log(JSON.stringify({
    label: LABEL, mode: MODE, start: state0, acts,
    perAct: work, cpuPerAct: cpu,
    longTasks: longs, longSum: longs.reduce((a, b) => a + b, 0),
    endCounter: await evalJS('(document.getElementById("vCounterFloat")||{}).textContent'),
    endVf: await evalJS('document.querySelectorAll(".vf-item").length')
  }, null, 2));

  try { ws.close(); } catch (e) {} try { proc.kill(); } catch (e) {}
})().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
