/*
 * 「点开一张图再返回，网格会不会自己跳」端到端探针
 *
 * 复现用户报的现象（2.4.43）：
 *   手机一屏两列八张（四行），点**任意**一张进竖图流再返回 ——
 *   网格总要把那一行摆到"视口上方 1/4 处"，整屏内容被推走，点的那张跳到"第二排"。
 *   本意是"滑了很远回来还能找到"，但**一屏之内**点开就纯属把刚看过的画面搬走。
 *
 * 两段，分别钉住"该不动的不动"和"该动的还能动"：
 *   ① 目标行完整可见 → 返回后 scrollTop 不变、所有格子的位置一个像素都不许变；
 *      （同时仍要有 .mv-flash 高亮 —— "我刚看的是这张"的提示不能丢）
 *   ② 目标被滑到很远（在流里滚到末尾）→ 返回后网格**必须**滚过去，且目标可见。
 *      这一段是防"为了不跳，干脆把定位整个关掉"。
 *
 * 用法：node tools/grid_locate_e2e.js   → 打印一行 JSON
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
const CDP_PORT = 9600 + Math.floor(Math.random() * 120);
const SITE_PORT = 19100 + Math.floor(Math.random() * 120);

const GIF = Buffer.from('R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7', 'base64');
const sleep = ms => new Promise(r => setTimeout(r, ms));

const DIR = '/vol1/1000/相册';
const N = 40;   // 够多屏：一屏两列八张，后面还有好几屏可以用来测"滑很远"
const FILES = [];
for (let i = 1; i <= N; i++) {
  const nm = 'b' + String(i).padStart(2, '0') + '.jpg';
  FILES.push({ name: nm, path: DIR + '/' + nm, kind: 'image', size: 1000 + i, mtime: 1000 + i, ext: '.jpg' });
}

const SETTINGS = {
  thumbEnabled: true, coverEnabled: false, coverNameBelow: false,
  mobileFlowView: true, onlyRoots: false, roots: [], rootsInfo: [],
  thumbDir: '', thumbSize: 320, preloadConcurrency: 0
};

function serveFile(res, name) {
  const p = path.join(WEB, name);
  if (!fs.existsSync(p)) { res.writeHead(404); res.end('no ' + name); return; }
  let body = fs.readFileSync(p);
  if (name === 'index.html') body = body.toString('utf8').replace(/__MV_VERSION__/g, '2443');
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
      const files = target === DIR ? FILES.slice() : [];
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ sort: u.searchParams.get('sort') || 'name:asc', dirs: [], files: files }));
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
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-locate-' + Date.now()),
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
    } catch (e) { /* 端口未就绪 */ }
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
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  await send('Page.navigate', { url: base + '?dir=' + encodeURIComponent(DIR) });

  const waitCells = async (n, timeout) => {
    const t0 = Date.now();
    while (Date.now() - t0 < (timeout || 15000)) {
      try {
        if (await evalJS("document.querySelectorAll('#gridContent > *').length") >= n) return true;
      } catch (e) { /* 导航中 */ }
      await sleep(100);
    }
    throw new Error('等 ' + n + ' 个格子超时');
  };
  await waitCells(8);
  await sleep(400);

  // 网格整体快照：scrollTop + 每个格子的屏幕位置（用来判"有没有动过"）
  const snap = () => evalJS(`(function(){
    var sc = document.getElementById('gridScroller');
    var out = { scroll: sc.scrollTop, vh: sc.clientHeight, cells: [] };
    [].forEach.call(document.querySelectorAll('#gridContent .cell'), function (c) {
      var r = c.getBoundingClientRect();
      out.cells.push({ p: c.dataset.mvPath, t: Math.round(r.top * 10) / 10, h: Math.round(r.height * 10) / 10 });
    });
    return out;
  })()`);

  // 选一个"完整可见"的格子（尽量取这一屏的中段，最能暴露"被摆到 1/4 处"的问题）
  const pickVisible = () => evalJS(`(function(){
    var sc = document.getElementById('gridScroller');
    var sr = sc.getBoundingClientRect();
    var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    var best = -1;
    for (var i = 0; i < cells.length; i++) {
      var r = cells[i].getBoundingClientRect();
      if (r.top >= sr.top - 0.5 && r.bottom <= sr.top + sc.clientHeight + 0.5) best = i;
    }
    if (best < 0) return null;
    var pick = Math.max(0, best - 2);
    var rr = cells[pick].getBoundingClientRect();
    if (!(rr.top >= sr.top - 0.5 && rr.bottom <= sr.top + sc.clientHeight + 0.5)) pick = best;
    return { idx: pick, path: cells[pick].dataset.mvPath };
  })()`);

  const tapAndOpen = async pick => {
    await evalJS(`(function(){
      var c = document.querySelectorAll('#gridContent .cell')[${pick.idx}];
      c.querySelector('.cell-inner').click();
      return true;
    })()`);
    const t = Date.now();
    while (Date.now() - t < 8000) {
      try { if (await evalJS("!!(window.MVFlow && window.MVFlow.isOpen())")) return true; } catch (e) {}
      await sleep(100);
    }
    return false;
  };
  const goBack = async () => {
    await evalJS('history.back(); true');
    const t = Date.now();
    while (Date.now() - t < 8000) {
      try { if (!(await evalJS("!!(window.MVFlow && window.MVFlow.isOpen())"))) return true; } catch (e) {}
      await sleep(100);
    }
    return false;
  };
  const flashAnywhere = () => evalJS("!!document.querySelector('#gridContent .cell.mv-flash')");

  const out = {};

  /* ---------- ① 一屏之内：一格都不许动 ---------- */
  const pickA = await pickVisible();
  out.a_pick = pickA;
  out.a_before = await snap();
  out.a_open = await tapAndOpen(pickA);
  await sleep(500);
  out.a_back = await goBack();
  await sleep(450);          // flash 在 1.4s 后自动撤，这里要抓"还在"的那一刻
  out.a_after = await snap();
  out.a_flash = await flashAnywhere();

  /* ---------- ② 滑很远：定位必须还生效 ---------- */
  await evalJS("document.getElementById('gridScroller').scrollTop = 0; true");
  await sleep(300);
  const pickB = await pickVisible();
  out.b_pick = pickB;
  out.b_before = await snap();
  out.b_open = await tapAndOpen(pickB);
  await sleep(500);
  // 在竖图流里滚到末尾（模拟"滑了很长一段"），让"当前张"变成后面某张
  await evalJS("(function(){var s=document.querySelector('.vf-scroll'); if (s) s.scrollTop = s.scrollHeight; return true;})()");
  await sleep(600);
  out.b_flowIdx = await evalJS("window.MVFlow ? window.MVFlow.index() : -9");
  out.b_back = await goBack();
  await sleep(450);
  out.b_after = await snap();
  out.b_flash = await flashAnywhere();

  try { proc.kill('SIGKILL'); } catch (e) {}
  site.close();
  console.log(JSON.stringify(out));
}

main().catch(e => {
  console.error(String(e && e.stack || e));
  process.exit(1);
});
