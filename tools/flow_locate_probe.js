/*
 * 「点第几张打开却从第一张开始」取证（真实 app，手机 profile）
 *
 * 做法：在网格里点第 k 个**图片**格子 → 进入竖图流 → 读：
 *   · MVFlow.index()        当前张索引
 *   · .vf-count 文本         HUD 显示
 *   · 目标格子相对视口的 top  应为 0（顶边对齐视口顶部）
 *   · 挂载的第一个 data-i   应该就是这个索引
 *
 * 用法：node tools/flow_locate_probe.js http://127.0.0.1:PORT/app/mediaview/ "目录绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8140/app/mediaview/';
const DIR = process.argv[3] || '';
const PORT = 9650 + Math.floor(Math.random() * 90);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-loc-' + Date.now()),
    '--remote-debugging-port=' + PORT,
    '--window-size=390,844',
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
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);

  const out = { clicks: [] };

  // 依次点第 1 / 3 / 5 / 7 个图片格
  for (const pick of [0, 2, 4, 6]) {
    // 关掉上一次的流（点完之后用返回）
    const opened = await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      // 只取图片格（不含目录）
      var imgs = cells.filter(function (c) { return !c.querySelector('.cell-fallback'); });
      var c = imgs[${pick}];
      if (!c) return { error: 'no cell at ${pick}', total: imgs.length };
      var name = (c.querySelector('.cell-label-name') || {}).textContent || '';
      c.querySelector('.cell-inner').click();
      return { clicked: true, name: name, total: imgs.length };
    })()`);
    await sleep(1200);

    const state = await evalJS(`(function(){
      var sc = document.querySelector('.vf-scroll');
      if (!sc) return { error: 'no flow' };
      var items = [].slice.call(document.querySelectorAll('.vf-item'));
      var cur = window.MVFlow.index();
      var target = document.querySelector('.vf-item[data-i="' + cur + '"]');
      var vis = items.map(function (e) { return { i: +e.dataset.i, top: Math.round(e.getBoundingClientRect().top) }; })
                     .sort(function (a, b) { return a.i - b.i; });
      var img = null;
      if (target) {
        var im = target.querySelector('img.vf-full');
        img = im ? (im.getAttribute('alt') || '') : '';
      }
      return {
        idx: cur,
        hud: (document.querySelector('.vf-count') || {}).textContent || '',
        targetTop: target ? Math.round(target.getBoundingClientRect().top) : null,
        scrollTop: Math.round(sc.scrollTop),
        firstMounted: vis.length ? vis[0].i : -1,
        mounted: vis.map(function (v) { return v.i; }),
        altAtIdx: img
      };
    })()`);

    out.clicks.push({ pick, opened, state });

    // 关流：按返回（触屏档会走历史/或直接 close）
    await evalJS("try { history.back(); } catch(e) {}");
    await sleep(900);
    const stillOpen = await evalJS("!!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen())");
    if (stillOpen) {
      await evalJS("window.MVFlow.close()");
      await sleep(300);
    }
  }

  console.log(JSON.stringify(out, null, 2));

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
