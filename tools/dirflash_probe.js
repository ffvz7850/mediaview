/*
 * 「前进/后退文件夹会闪一下」取证探针（真实 app）
 *
 * 做法：注入帧计数器 + MutationObserver（观察 #gridContent 的子节点变化），
 * 然后依次：
 *   ① 点开子文件夹（进目录）
 *   ② 点返回（退目录）
 *   ③ 再进一次（此时缩略图已在浏览器缓存里）
 * 每一步都记录「事件时间线」：时刻 / 第几帧 / 当前格子数 / 已有 src 的 img 数。
 *
 * 判据（会打印）：
 *   · 是否出现 children === 0 的中间态，且它跨越了 ≥1 帧（= 用户看到的空白闪）
 *   · 从点下去到格子上屏之间有几帧
 *
 * 用法：node tools/dirflash_probe.js http://127.0.0.1:PORT/app/mediaview/ "目录绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8140/app/mediaview/';
const DIR = process.argv[3] || '';
const PORT = 9800 + Math.floor(Math.random() * 120);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-flash-' + Date.now()),
    '--remote-debugging-port=' + PORT,
    '--window-size=1200,800',
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
  const url = DIR ? URL_BASE + '?dir=' + encodeURIComponent(DIR) : URL_BASE;
  await send('Page.navigate', { url });
  await sleep(2500);

  // 装探针：帧计数 + 子节点观察
  await evalJS(`(function(){
    window.__fl = { ev: [], frame: 0, t0: performance.now(), stop: false };
    (function loop(){ if (window.__fl.stop) return; window.__fl.frame++; requestAnimationFrame(loop); })();
    var c = document.getElementById('gridContent');
    var snap = function (tag) {
      window.__fl.ev.push({
        tag: tag,
        t: Math.round(performance.now() - window.__fl.t0),
        f: window.__fl.frame,
        n: c.children.length,
        withSrc: c.querySelectorAll('img.cell-thumb[src]').length
      });
    };
    window.__fl.snap = snap;
    new MutationObserver(function (muts) {
      var tag = 'mut';
      for (var i = 0; i < muts.length; i++) {
        if (muts[i].removedNodes && muts[i].removedNodes.length) tag = 'del';
      }
      snap(tag);
    }).observe(c, { childList: true });
    snap('init');
    return c.children.length;
  })()`);

  const out = { steps: [] };
  const step = async (name, jsClick, wait) => {
    await evalJS(`window.__fl.ev = []; window.__fl.snap('click');`);
    await evalJS(jsClick);
    await sleep(wait);
    const ev = await evalJS('window.__fl.ev.slice()');
    const cells = await evalJS("document.querySelectorAll('#gridContent .cell').length");
    const path = await evalJS('(window.__mvPathProbe || (document.querySelector(".crumb-current") || {}).textContent || "")');
    // 找出"归零"的中间态：n 掉到 0 之后多久恢复
    let zero = ev.filter(e => e.n === 0);
    let zeroFrames = 0;
    if (zero.length) {
      const z = zero[0];
      const back = ev.find(e => e.t > z.t && e.n > 0);
      zeroFrames = back ? (back.f - z.f) : -1;
    }
    out.steps.push({ name, events: ev.length, cells, zeroEvents: zero.length, zeroFrames, path,
                     timeline: ev.slice(0, 40) });
  };

  // ① 进子目录（点文件夹格子的 .cell-inner）
  await step('enter-sub', `(function(){
    var cells = document.querySelectorAll('#gridContent .cell');
    for (var i = 0; i < cells.length; i++) {
      if (/sub/.test(cells[i].textContent)) { cells[i].querySelector('.cell-inner').click(); return 'ok'; }
    }
    return 'nosub';
  })()`, 900);

  // ② 返回（工具栏 ←）
  await step('back-up', `(function(){ document.getElementById('navBack').click(); return 'ok'; })()`, 900);

  // ③ 再进一次（缩略图应已在缓存）
  await step('enter-again', `(function(){
    var cells = document.querySelectorAll('#gridContent .cell');
    for (var i = 0; i < cells.length; i++) {
      if (/sub/.test(cells[i].textContent)) { cells[i].querySelector('.cell-inner').click(); return 'ok'; }
    }
    return 'nosub';
  })()`, 900);

  console.log(JSON.stringify(out, null, 2));

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
