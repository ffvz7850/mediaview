/*
 * 竖图流排序取证（第四支）：**改完排序立刻点图**（列表还没回来就打开）
 *
 * 复现：加网络延迟（模拟 NAS）→ 用工具栏把排序从「修改时间↓」改成「按名称↑」
 *      → 200ms 后点第一张图（此时 /api/list 还没回来，state.items 仍是旧顺序）
 *      → 读竖图流的顺序；随后等列表回来，再读网格顺序作对照
 *
 * 用法：node tools/flow_sort_race_probe.js http://127.0.0.1:PORT/app/mediaview/ "目录绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2];
const DIR = process.argv[3] || '';
const PORT = 10050 + Math.floor(Math.random() * 40);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-race-' + Date.now()),
    '--remote-debugging-port=' + PORT, '--window-size=390,844', 'about:blank'
  ], { stdio: 'ignore' });

  let pages = [];
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch('http://127.0.0.1:' + PORT + '/json/list');
      pages = await r.json();
      if (pages.some(t => t.type === 'page')) break;
    } catch (e) {}
    await sleep(250);
  }
  const page = pages.filter(t => t.type === 'page')[0];
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
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  // 先以「修改时间↓」进目录（无网络延迟，把状态坐实）
  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);
  const before = await evalJS(`(function(){
    var els = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    return { sort: localStorage.getItem('mediaview_sort'), n: els.length };
  })()`);

  // 加延迟，模拟 NAS：/api/list 要 ~1.2s
  await send('Network.enable');
  await send('Network.emulateNetworkConditions', {
    offline: false, latency: 1200, downloadThroughput: -1, uploadThroughput: -1
  });

  // 改排序 → 200ms 后点第一张（此时列表还没回来）
  await evalJS(`(function(){
    var sel = document.getElementById('sortSel');
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value.split(':')[0] === 'name') {
        sel.value = sel.options[i].value;
        sel.dispatchEvent(new Event('change', { bubbles: true }));
        return sel.value;
      }
    }
  })()`);
  await sleep(200);
  await evalJS(`(function(){
    var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
    if (imgs[0]) imgs[0].querySelector('.cell-inner').click();
  })()`);
  await sleep(1600);

  const flow = await evalJS(`(function(){
    if (!window.MVFlow || !window.MVFlow.isOpen()) return { error: 'flow not open' };
    var els = [].slice.call(document.querySelectorAll('.vf-item'));
    els.sort(function (a, b) { return (+a.dataset.i) - (+b.dataset.i); });
    return {
      order: els.map(function (e) {
        var im = e.querySelector('img.vf-full');
        return im ? (im.getAttribute('alt') || '') : '';
      }),
      hud: (document.querySelector('.vf-count') || {}).textContent || ''
    };
  })()`);

  // 列表回来后的网格顺序（关掉流再看）
  await evalJS("try { history.back(); } catch(e) {}");
  await sleep(1000);
  if (await evalJS("!!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen())")) {
    await evalJS("window.MVFlow.close()");
    await sleep(400);
  }
  await sleep(1500);
  const after = await evalJS(`(function(){
    var els = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    return {
      sort: localStorage.getItem('mediaview_sort'),
      names: els.map(function (c) {
        var im = c.querySelector('img');
        return im ? (im.getAttribute('alt') || im.src.split('/').pop().split('?')[0]) : '';
      })
    };
  })()`);

  console.log(JSON.stringify({ before, flow, after }, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
