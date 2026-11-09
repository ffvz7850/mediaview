/*
 * 「换目录 / 返回 / 前进 —— 到底闪不闪」端到端探针（真实 Edge + 真实页面导航）
 *
 * 为什么需要它：这类问题的答案不在源码里，而在**帧**上。单元测试只能证明"代码这么写"，
 * 证明不了"用户看不到空白"。所以这个探针自带一个假后端（/api/settings、/api/list、
 * 缩略图），把 src/web 真跑起来，然后用 CDP：
 *
 *   ① 打开 ?dir=/（根层，3 个文件夹）
 *   ② 点第一个文件夹 → 真的换文档（location.href）
 *   ③ history.back()  → 回根层
 *   ④ history.forward() → 再进那个文件夹
 *
 * 每个新文档里都注入了帧探针（Page.addScriptToEvaluateOnNewDocument），逐帧采样
 * `#gridContent` 的子节点数，记录：
 *   · firstCells : 从本文档开始到**第一帧有内容**过了多少毫秒（越小越"不闪"）
 *   · blankAfter : 内容出现之后又变回 0 格的帧数（>0 = 用户会看到的白闪）
 *   · doc        : 第几个文档（证明真的换了文档，不是内部切换）
 *
 * 用法：node tools/realnav_probe.js   → 打印一行 JSON
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
const CDP_PORT = 9700 + Math.floor(Math.random() * 150);
const SITE_PORT = 18700 + Math.floor(Math.random() * 150);

const GIF = Buffer.from('R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7', 'base64');
const sleep = ms => new Promise(r => setTimeout(r, ms));

/* ------------------------------- 假后端 ------------------------------- */
const ROOT = '/';
const SUB = '/vol1/1000/照片';
const FILES = [];
for (let i = 1; i <= 24; i++) {
  FILES.push({
    name: 'img' + String(i).padStart(2, '0') + '.jpg',
    path: SUB + '/img' + String(i).padStart(2, '0') + '.jpg',
    kind: 'image', size: 100000 + i, mtime: 1700000000 + i, ext: '.jpg'
  });
}
const SETTINGS = {
  thumbEnabled: true, coverEnabled: true, coverNameBelow: false,
  mobileFlowView: true, onlyRoots: false, roots: [], rootsInfo: [],
  thumbDir: '', thumbSize: 320, preloadConcurrency: 0, pregenCount: 0,
  theme: 'light', sortDefault: 'mtime:desc'
};

function listFor(p) {
  if (p === ROOT) {
    return {
      sort: 'mtime:desc',
      dirs: [
        { name: '照片', path: SUB },
        { name: '音乐', path: '/vol1/1000/音乐' },
        { name: '视频', path: '/vol1/1000/视频' }
      ],
      files: []
    };
  }
  if (p === SUB) return { sort: 'mtime:desc', dirs: [], files: FILES };
  return { sort: 'mtime:desc', dirs: [], files: [] };
}

function serveFile(res, name) {
  const p = path.join(WEB, name);
  if (!fs.existsSync(p)) { res.writeHead(404); res.end('no ' + name); return; }
  let body = fs.readFileSync(p);
  if (name === 'index.html') body = body.toString('utf8').replace(/__MV_VERSION__/g, '2440');
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
      const target = u.searchParams.get('path') || ROOT;
      // 故意慢一点：真机上 /api/list 要扫盘，几百毫秒很常见。
      // 有快照时首帧**不该**等它 —— 这正是本探针要区分的两件事。
      const body = JSON.stringify(listFor(target));
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
      res.end(JSON.stringify({ w: 400, h: 600 }));
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

/* ------------------------------- CDP ------------------------------- */
async function main() {
  await new Promise(r => site.listen(SITE_PORT, '127.0.0.1', r));
  const base = 'http://127.0.0.1:' + SITE_PORT + '/app/mediaview/';

  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-realnav-' + Date.now()),
    '--remote-debugging-port=' + CDP_PORT,
    '--window-size=390,844',
    // MV_NO_BFCACHE=1 ⇒ 关掉前进/后退缓存，强制走"整页重建"那条路
    //（飞牛 App 的 WebView 不保证有 bfcache，所以这条路必须单独量一遍）
    ...(process.env.MV_NO_BFCACHE ? ['--disable-features=BackForwardCache'] : []),
    'about:blank'
  ], { stdio: 'ignore' });

  let page = null;
  for (let i = 0; i < 80; i++) {
    try {
      const r = await fetch('http://127.0.0.1:' + CDP_PORT + '/json/list');
      const list = await r.json();
      page = list.filter(t => t.type === 'page')[0];
      if (page) break;
    } catch (e) {}
    await sleep(200);
  }
  if (!page) throw new Error('连不上 Edge 的调试端口');

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

  const probe = `(function(){
    var P = { doc: 0, t0: performance.now(), frames: 0, firstCells: null, blankAfter: 0, maxCells: 0 };
    try { P.doc = (+sessionStorage.getItem('__mvpDoc') || 0) + 1; sessionStorage.setItem('__mvpDoc', P.doc); } catch (e) {}
    P.href = location.href;
    window.__mvProbe = P;
    function tick() {
      var c = document.getElementById('gridContent');
      var n = c ? c.children.length : 0;
      if (n > 0 && P.firstCells === null) P.firstCells = Math.round(performance.now() - P.t0);
      if (P.firstCells !== null && n === 0) P.blankAfter++;
      if (n > P.maxCells) P.maxCells = n;
      P.frames++;
      if (P.frames < 400) requestAnimationFrame(tick);
    }
    requestAnimationFrame(tick);
  })();`;
  await send('Page.addScriptToEvaluateOnNewDocument', { source: probe });

  const waitReady = async (wantHrefPart, timeout) => {
    const t0 = Date.now();
    let seenAt = 0;
    while (Date.now() - t0 < (timeout || 12000)) {
      try {
        const st = await evalJS('({rs: document.readyState, href: location.href, ' +
          'first: window.__mvProbe ? window.__mvProbe.firstCells : null, ' +
          'frames: window.__mvProbe ? window.__mvProbe.frames : 0})');
        if (st && st.rs === 'complete' && st.href.indexOf(wantHrefPart) >= 0) {
          // 光 readyState=complete 不够：文档刚切换的那一小段时间里，Runtime.evaluate 有时还落在
          // 旧执行上下文上，读到的却是新文档的探针（内容还没画）。实测偶发 frames=39 /
          // firstCells=null，断言就会误报"进目录后一直没有格子"。
          // 所以等到"探针真的看到格子"（或跑满 400 帧、确实没内容）再开始攒样本。
          if (st.first !== null || st.frames >= 399) {
            if (!seenAt) seenAt = Date.now();
            if (Date.now() - seenAt >= 400) return await evalJS('window.__mvProbe');
          }
        }
      } catch (e) { /* 文档切换中上下文会被销毁，重试即可 */ }
      await sleep(120);
    }
    throw new Error('等待 ' + wantHrefPart + ' 超时');
  };

  // 等"格子真的画出来"（列表是慢接口，readyState=complete 时可能还是空的）
  const waitCells = async (n, timeout) => {
    const t0 = Date.now();
    while (Date.now() - t0 < (timeout || 15000)) {
      try {
        const got = await evalJS("(document.querySelectorAll('#gridContent > *').length)");
        if (got >= n) return true;
      } catch (e) {}
      await sleep(100);
    }
    throw new Error('等 ' + n + ' 个格子超时');
  };

  const out = { root: null, sub: null, back: null, forward: null, scrolledAt: 0, forwardScroll: -1 };

  // 首访（没有任何快照）要等慢接口，这是正常的 —— 所以先等格子出来再读一次，
  // 拿到"最终耗时"。返回/前进就不许等了：那两处必须在接口回来之前就有画面。
  await send('Page.navigate', { url: base + '?dir=/' });
  await waitReady('?dir=/');
  await waitCells(3);
  out.root = await evalJS('window.__mvProbe');

  // 点第一个文件夹（真的换文档）
  await evalJS("document.querySelector('#gridContent .cell-inner').click(); true");
  await waitReady('dir=%2Fvol1%2F1000');
  await waitCells(10);
  out.sub = await evalJS('window.__mvProbe');

  // 记一个"看到第几屏"：返回/前进之后必须回到这里（否则永远跳回顶部）
  const SCROLL = 300;
  await evalJS('document.getElementById("gridScroller").scrollTop = ' + SCROLL + '; true');
  await sleep(250);
  out.scrolledAt = await evalJS('document.getElementById("gridScroller").scrollTop');

  // 返回：应当是"退到上一个页面"，且页面靠快照立刻有内容（不等接口）
  await evalJS('history.back(); true');
  out.back = await waitReady('?dir=/');

  // 前进：同样是真文档 + 快照
  await evalJS('history.forward(); true');
  out.forward = await waitReady('dir=%2Fvol1%2F1000');
  out.forwardScroll = await evalJS('document.getElementById("gridScroller").scrollTop');

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
