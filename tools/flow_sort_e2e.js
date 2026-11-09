/*
 * 「竖图流排序跟随」端到端探针（真实页面 + 真实 flow.js + 真实工具栏）
 *
 * 复现的正是用户报的那个窗口（2.4.33 审查报告的复现 A）：
 *   ① 目录里有 6 张图：名称 a1..a6，但 **mtime 与名称相反**（a6 最新）。
 *      所以「名称升序」= a1..a6，「修改时间降序」= a6..a1。
 *   ② /api/list 故意延迟 600ms（贴近 NAS 扫盘）。
 *   ③ 打开目录（默认 mtime:desc）→ 网格是 a6..a1。
 *   ④ 切到「按名称」（此刻网格仍是旧顺序，列表要 600ms 后才回来）。
 *   ⑤ **立刻**点第一个格子（那是 a6）。
 *   ⑥ 竖图流必须以 name:asc 铺开（a1..a6），并且**停在 a6**（用户点的那张，在末尾）。
 *
 * 判据：idx === 5 且 第一张是 a1 —— 只要"没自排"或"按下标换算"，这一条必红。
 *
 * 用法：node tools/flow_sort_e2e.js
 */
'use strict';

const { spawn } = require('child_process');
const http = require('http');
const fs = require('fs');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const WEB = path.resolve(__dirname, '..', 'src', 'web');
const CDP_PORT = 9750 + Math.floor(Math.random() * 150);
const SITE_PORT = 18900 + Math.floor(Math.random() * 150);

const GIF = Buffer.from('R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7', 'base64');
const sleep = ms => new Promise(r => setTimeout(r, ms));

const DIR = '/vol1/1000/相册';
// 名称 a1..a6；mtime 反过来（a6 最新）；size 也反过来（a6 最小）
const FILES = [];
for (let i = 1; i <= 12; i++) {
  FILES.push({
    name: 'a' + i + '.jpg',
    path: DIR + '/a' + i + '.jpg',
    kind: 'image', size: (13 - i) * 100, mtime: 1000 * i, ext: '.jpg'
  });
}
function sortFiles(sort) {
  const p = String(sort || 'mtime:desc').toLowerCase().split(':');
  const f = p[0], desc = p[1] !== 'asc';
  const arr = FILES.slice().sort((x, y) => {
    let c = 0;
    if (f === 'name') c = x.name < y.name ? -1 : (x.name > y.name ? 1 : 0);
    else if (f === 'size') c = x.size - y.size;
    else c = x.mtime - y.mtime;
    return desc ? -c : c;
  });
  return arr;
}

const SETTINGS = {
  thumbEnabled: true, coverEnabled: true, coverNameBelow: false,
  mobileFlowView: true, onlyRoots: false, roots: [], rootsInfo: [],
  thumbDir: '', thumbSize: 320, preloadConcurrency: 0
};

function serveFile(res, name) {
  const p = path.join(WEB, name);
  if (!fs.existsSync(p)) { res.writeHead(404); res.end('no ' + name); return; }
  let body = fs.readFileSync(p);
  if (name === 'index.html') body = body.toString('utf8').replace(/__MV_VERSION__/g, '2441');
  const ext = path.extname(name);
  const mime = ext === '.js' ? 'text/javascript; charset=utf-8'
    : ext === '.css' ? 'text/css; charset=utf-8'
      : ext === '.html' ? 'text/html; charset=utf-8' : 'application/octet-stream';
  res.writeHead(200, { 'Content-Type': mime, 'Cache-Control': 'no-store' });
  res.end(body);
}

const site = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://127.0.0.1');
  let p = u.pathname;
  if (p.startsWith('/app/mediaview')) p = p.slice('/app/mediaview'.length) || '/';
  if (p.startsWith('/api/')) {
    const api = p.slice(5);
    if (api === 'list') {
      const target = u.searchParams.get('path') || '/';
      const sort = u.searchParams.get('sort') || 'mtime:desc';
      const files = target === DIR ? sortFiles(sort) : [];
      const body = JSON.stringify({ sort: sort, dirs: [], files: files });
      setTimeout(() => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(body);
      }, +(process.env.MV_LIST_DELAY || 600));
      return;
    }
    if (api === 'settings') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(SETTINGS));
      return;
    }
    if (api === 'meta') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ w: 400, h: 600 }));   // 2:3 竖图
      return;
    }
    if (api === 'thumb' || api === 'dircov' || api === 'raw') {
      res.writeHead(200, { 'Content-Type': 'image/gif', 'Cache-Control': 'max-age=600' });
      res.end(GIF);
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end('{}');
    return;
  }
  if (p === '/' || p === '') { serveFile(res, 'index.html'); return; }
  serveFile(res, p.replace(/^\//, ''));
});

async function main() {
  await new Promise(r => site.listen(SITE_PORT, '127.0.0.1', r));
  const base = 'http://127.0.0.1:' + SITE_PORT + '/app/mediaview/';

  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-fsort-' + Date.now()),
    '--remote-debugging-port=' + CDP_PORT,
    '--window-size=390,844',
    'about:blank'
  ], { stdio: 'ignore' });

  let page = null;
  for (let i = 0; i < 80; i++) {
    try {
      const r = await fetch('http://127.0.0.1:' + CDP_PORT + '/json/list');
      const list_ = await r.json();
      page = list_.filter(t => t.type === 'page')[0];
      if (page) break;
    } catch (e) {}
    await sleep(200);
  }
  if (!page) throw new Error('连不上 Edge 调试端口');

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
  // 手机档：竖图流只在 PROFILE === 'touch' 时才走（指针必须是 coarse）
  await send('Emulation.setDeviceMetricsOverride', {
    width: 390, height: 844, deviceScaleFactor: 2, mobile: true
  });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: "window.__probeDoc = (window.__probeDoc || 0) + 1;"
  });

  const out = { profile: null };
  const url = base + '?dir=' + encodeURIComponent(DIR);
  await send('Page.navigate', { url });

  // 等首屏格子（list 600ms + 渲染）
  const waitCells = async (n, timeout) => {
    const t0 = Date.now();
    while (Date.now() - t0 < (timeout || 15000)) {
      try {
        const got = await evalJS("document.querySelectorAll('#gridContent > *').length");
        if (got >= n) return true;
      } catch (e) {}
      await sleep(100);
    }
    throw new Error('等 ' + n + ' 个格子超时');
  };
  await sleep(300);
  out.profile = await evalJS(
    "JSON.stringify({coarse: matchMedia('(pointer: coarse)').matches," +
    " hoverNone: matchMedia('(hover: none)').matches, maxTouch: navigator.maxTouchPoints," +
    " flowHost: !!window.__mvFlowHost})");

  await waitCells(12);
  out.gridBefore = await evalJS(
    "[].map.call(document.querySelectorAll('#gridContent > *'), function (e) {" +
    " return (e.dataset.mvPath || '').split('/').pop(); })");

  // 切到「按名称」—— 列表要 600ms 后才回来，此刻网格还是旧顺序
  await evalJS(
    "var s = document.getElementById('sortSel'); s.value = 'name:asc';" +
    "s.dispatchEvent(new Event('change')); true");
  await sleep(60);   // 「改完排序，手指马上落下」

  // 诊断：把 app 真正传给 open 的参数抓下来
  await evalJS(
    "window.__openArgs = null; var o = window.MVFlow.open; window.MVFlow.open = function (l, s) {" +
    " window.__openArgs = { n: l.length, start: s, first: l.slice(0,3).map(function(x){return x.name;})," +
    " sort: (window.__mvFlowHost && window.__mvFlowHost.currentSort) ? window.__mvFlowHost.currentSort() : '?' };" +
    " return o.apply(this, arguments); }; true");

  const clicked = await evalJS(
    "var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));" +
    "var cell = cells[5];" +                       // 旧顺序里第 6 个 = a7；新顺序里 a7 在第 7 位
    "var p = cell && cell.dataset.mvPath;" +
    "cell.querySelector('.cell-inner').click(); p");

  // 诊断：index 在打开后的演化（比例是异步补的，可能把位置带偏）
  await sleep(120);
  await sleep(300);
  await sleep(500);  // 等 flow 挂图（meta 很快）

  out.clicked = (clicked || '').split('/').pop();
  out.flowOpen = await evalJS("window.MVFlow ? window.MVFlow.isOpen() : false");
  out.flowIdx = await evalJS("window.MVFlow ? window.MVFlow.index() : -2");
  out.current = await evalJS(
    "(function(){ var i = window.MVFlow.index();" +
    " var el = document.querySelector('.vf-item[data-i=\"' + i + '\"] img.vf-full');" +
    " if (!el) return null;" +
    " var m = /[?&]path=([^&]+)/.exec(el.getAttribute('src') || '');" +
    " return m ? decodeURIComponent(m[1]).split('/').pop() : null; })()");

  // 滚到最顶，看第 0 张是谁（证明整条流是 name:asc）
  await evalJS("var sc = document.querySelector('.vf-scroll'); if (sc) sc.scrollTop = 0; true");
  await sleep(400);
  out.firstAtTop = await evalJS(
    "(function(){ var el = document.querySelector('.vf-item[data-i=\"0\"] img.vf-full');" +
    " if (!el) return null;" +
    " var m = /[?&]path=([^&]+)/.exec(el.getAttribute('src') || '');" +
    " return m ? decodeURIComponent(m[1]).split('/').pop() : null; })()");

  // 关掉流，等列表回来，确认网格最终是 name:asc
  await evalJS("window.MVFlow.close(); true");
  await sleep(900);
  out.gridAfter = await evalJS(
    "[].map.call(document.querySelectorAll('#gridContent > *'), function (e) {" +
    " return (e.dataset.mvPath || '').split('/').pop(); })");

  console.log(JSON.stringify(out, null, 0));

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
  try { site.close(); } catch (e) {}
}

main().catch(e => {
  console.error('PROBE ERROR', e && e.message);
  try { site.close(); } catch (e2) {}
  process.exit(1);
});
