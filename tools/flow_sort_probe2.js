/*
 * 竖图流排序跟随性取证（第二支：从聚合层逐层点进目录，再改排序）
 *
 * 用法：node tools/flow_sort_probe2.js http://127.0.0.1:PORT/app/mediaview/ "根目录绝对路径" "子目录名"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2];
const DIR = process.argv[3] || '';
const SUB = process.argv[4] || '';
const PORT = 9850 + Math.floor(Math.random() * 90);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-sort2-' + Date.now()),
    '--remote-debugging-port=' + PORT, '--window-size=390,844', 'about:blank'
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

  const out = { steps: [] };

  // 进子目录（点目录格）
  const entered = await evalJS(`(function(){
    var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    var d = cells.filter(function (c) { return !!c.querySelector('.cell-fallback'); })[0];
    if (!d) return { error: 'no dir cell', total: cells.length };
    d.querySelector('.cell-inner').click();
    return { ok: true };
  })()`);
  await sleep(2000);
  out.steps.push({ name: 'enter-sub', entered, path: await evalJS("location.search") });

  async function setSort(field, wantDesc) {
    await evalJS(`(function(){
      var sel = document.getElementById('sortSel');
      var f = ${JSON.stringify(field)};
      for (var i = 0; i < sel.options.length; i++) {
        if (sel.options[i].value.split(':')[0] === f) {
          sel.value = sel.options[i].value;
          sel.dispatchEvent(new Event('change', { bubbles: true }));
          return true;
        }
      }
      return false;
    })()`);
    await sleep(1200);
    const t = await evalJS("document.getElementById('sortDirBtn').textContent.trim()");
    if ((t === '↓') !== wantDesc) {
      await evalJS("document.getElementById('sortDirBtn').click()");
      await sleep(1200);
    }
  }

  async function openFirstAndRead(tag) {
    const grid = await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
      return { count: imgs.length, names: imgs.map(function (x) {
        var n = x.querySelector('.cell-label-name') || x.querySelector('.cell-name') || x.querySelector('.cell-label');
        return n ? (n.textContent || '').trim() : '';
      }), sort: localStorage.getItem('mediaview_sort') };
    })()`);
    await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
      if (imgs[0]) imgs[0].querySelector('.cell-inner').click();
    })()`);
    await sleep(1800);
    const flow = await evalJS(`(function(){
      if (!window.MVFlow || !window.MVFlow.isOpen()) return { error: 'flow not open' };
      var els = [].slice.call(document.querySelectorAll('.vf-item'));
      els.sort(function (a, b) { return (+a.dataset.i) - (+b.dataset.i); });
      return {
        names: els.map(function (e) {
          var im = e.querySelector('img.vf-full');
          return { i: +e.dataset.i, alt: im ? (im.getAttribute('alt') || '') : '' };
        }),
        hud: (document.querySelector('.vf-count') || {}).textContent || '',
        idx: window.MVFlow.index()
      };
    })()`);
    out.steps.push({ name: tag, grid, flow });
    await evalJS("try { history.back(); } catch(e) {}");
    await sleep(900);
    if (await evalJS("!!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen())")) {
      await evalJS("window.MVFlow.close()");
      await sleep(300);
    }
  }

  await setSort('name', false);
  await openFirstAndRead('name:asc');

  await setSort('size', false);   // 大小升序
  await openFirstAndRead('size:asc');

  await setSort('mtime', true);
  await openFirstAndRead('mtime:desc');

  console.log(JSON.stringify(out, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
