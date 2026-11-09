/*
 * 竖图流排序取证（第三支）：**快照顺序 vs 当前排序** 的竞态
 *
 * 复现真实链路：
 *   ① 以 mtime:desc 浏览目录 A（此时写入 A 的快照，顺序=修改时间降序）
 *   ② 把当前排序改成 name:asc（模拟"在别的目录改了排序"）
 *   ③ 再次以 ?dir=A 打开（手机换目录=整页导航）→ 页面先用**快照**铺满
 *   ④ 在列表接口回来之前点第一张图 → 竖图流拿到的是**快照的顺序**
 * 断言：流的顺序 == 快照顺序（mtime 降序）而不是 name 升序 ⇒ 复现成功
 *
 * 用法：node tools/flow_sort_snap_probe.js http://127.0.0.1:PORT/app/mediaview/ "目录A绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2];
const DIR = process.argv[3] || '';
const PORT = 9950 + Math.floor(Math.random() * 40);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-snap-' + Date.now()),
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

  const out = {};

  // ① 以 mtime:desc 打开目录 A（写入快照）
  await evalJS("1"); // noop, 保证 Runtime 就绪
  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);
  await evalJS(`(function(){
    var sel = document.getElementById('sortSel');
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value.split(':')[0] === 'mtime') {
        sel.value = sel.options[i].value;
        sel.dispatchEvent(new Event('change', { bubbles: true }));
        break;
      }
    }
  })()`);
  await sleep(1200);
  const btn = await evalJS("document.getElementById('sortDirBtn').textContent.trim()");
  if (btn !== '↓') { await evalJS("document.getElementById('sortDirBtn').click()"); await sleep(1200); }
  out.snapshotSort = await evalJS("localStorage.getItem('mediaview_sort')");
  out.snapOrder = await evalJS(`(function(){
    var s = sessionStorage.getItem('mv_snap:' + ${JSON.stringify(DIR)});
    if (!s) return null;
    var o = JSON.parse(s);
    return o.items.filter(function (x) { return x.kind !== 'dir'; }).map(function (x) { return x.name; }).slice(0, 8);
  })()`);

  // ② 把排序改成 name:asc（模拟在别的目录改了排序；不改动 A 的快照）
  await evalJS("localStorage.setItem('mediaview_sort', 'name:asc')");

  // ③ 重新以 ?dir=A 打开（整页导航）→ 快照先铺；④ 一出现格子就点第一张图
  //    加网络延迟模拟 NAS：本地 list 只要几十毫秒，"快照先铺 / 列表后到"的阶段抓不到。
  //    自动点击放到「新文档一执行就装上」的脚本里，才能精确点在快照那一帧。
  await send('Network.enable');
  await send('Network.emulateNetworkConditions', {
    offline: false, latency: 1200, downloadThroughput: -1, uploadThroughput: -1
  });
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: `
      window.__tapInfo = { at: 0, ok: false, sort: '' };
      var t0 = Date.now();
      var iv = setInterval(function () {
        var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
        var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
        if (!imgs.length) return;
        if (!(window.MVFlow && window.MVFlow.open)) return;   // 等 flow.js 就绪（真实用户点的也是加载完的页面）
        clearInterval(iv);
        window.__tapInfo.at = Date.now() - t0;
        window.__tapInfo.sort = localStorage.getItem('mediaview_sort') || '';
        try { imgs[0].querySelector('.cell-inner').click(); window.__tapInfo.ok = true; } catch (e) {}
      }, 16);
    `
  });
  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(6000);
  out.tappedAt = await evalJS("window.__tapInfo || null");
  const tap = { ok: true };
  await sleep(1500);
  out.tap = tap;
  out.flow = await evalJS(`(function(){
    if (!window.MVFlow || !window.MVFlow.isOpen()) {
      return { error: 'flow not open',
               viewerHidden: document.getElementById('viewer').classList.contains('hidden') };
    }
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
  out.sortNow = await evalJS("localStorage.getItem('mediaview_sort')");

  console.log(JSON.stringify(out, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
