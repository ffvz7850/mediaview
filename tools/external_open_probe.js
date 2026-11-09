/*
 * 「文件管理器双击打开范围外的图片 —— 到底能不能开」端到端探针
 *
 * 背景（2.4.42 修的 bug）：开启「只显示我添加的文件夹」后，在飞牛文件管理器里双击
 * 其它目录的图片/视频，应用打不开（服务端 403）。
 *
 * 这个探针把**真实后端**（由 Go 测试用 httptest 起，settings 里 OnlyRoots=true）
 * 和**真实前端**（src/web，真跑）接在一起，用 headless Edge 走两条路：
 *
 *   ① ?path=<范围外的文件>  —— 单文件模式，应当能出图；
 *   ② ?dir=<范围外的目录>   —— 应用内浏览，应当仍然被挡住（这是必须保住的边界）。
 *
 * 断言不靠"代码里有 external=1"，而靠**图真的解码出来了**（naturalWidth > 0）
 * 与**没有一条 /api/ 请求是 403**。
 *
 * 用法：MV_BASE=http://127.0.0.1:PORT/app/mediaview/ \
 *       MV_FILE=/vol1/1000/别的/a.jpg MV_DIR=/vol1/1000/别的 \
 *       node tools/external_open_probe.js   → 打印一行 JSON
 */
'use strict';

const { spawn } = require('child_process');
const os = require('os');
const path = require('path');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const BASE = process.env.MV_BASE;
const FILE = process.env.MV_FILE;
const DIR = process.env.MV_DIR;
const CDP_PORT = 9850 + Math.floor(Math.random() * 120);
const sleep = ms => new Promise(r => setTimeout(r, ms));

if (!BASE || !FILE || !DIR) {
  console.error('需要 MV_BASE / MV_FILE / MV_DIR');
  process.exit(2);
}

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-extopen-' + Date.now()),
    '--remote-debugging-port=' + CDP_PORT,
    // 390x844 + 触屏：单文件模式的 pickMaxdim 与真机接近
    '--window-size=390,844',
    'about:blank'
  ], { stdio: 'ignore' });

  let page = null;
  for (let i = 0; i < 80; i++) {
    try {
      const r = await fetch('http://127.0.0.1:' + CDP_PORT + '/json/list');
      const list = await r.json();
      page = list.filter(t => t.type === 'page')[0];
      if (page) break;
    } catch (e) { /* 端口还没起来 */ }
    await sleep(200);
  }
  if (!page) throw new Error('连不上 Edge 的调试端口');

  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

  let id = 0;
  const pending = new Map();
  const apiHits = [];        // 所有 /api/ 请求的 (url, status)
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); return; }
    if (m.method === 'Network.responseReceived') {
      const r = m.params.response;
      if (r.url.indexOf('/api/') >= 0) apiHits.push({ url: r.url, status: r.status });
    }
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
  await send('Network.enable');

  const out = {};

  /* ---------- ① 单文件模式：范围外的图片能不能开 ---------- */
  apiHits.length = 0;
  await send('Page.navigate', { url: BASE + '?path=' + encodeURIComponent(FILE) });
  const t0 = Date.now();
  let img = null;
  while (Date.now() - t0 < 15000) {
    try {
      img = await evalJS(`(function(){
        var im = document.querySelector('#vMediaWrap .v-vbox img');
        if (!im) return null;
        return { src: im.getAttribute('src') || '', w: im.naturalWidth || 0, h: im.naturalHeight || 0,
                 complete: !!im.complete };
      })()`);
      if (img && img.w > 0) break;
    } catch (e) { /* 导航中上下文销毁 */ }
    await sleep(150);
  }
  out.open = {
    img: img,
    ms: Date.now() - t0,
    api: apiHits.slice(),
    viewerVisible: await evalJS("!document.getElementById('viewer').classList.contains('hidden')")
  };

  /* ---------- ② 应用内浏览：同一个目录仍然要被挡住 ---------- */
  apiHits.length = 0;
  await send('Page.navigate', { url: BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2500);   // 等 loadDir 的 403 落地
  out.grid = {
    cells: await evalJS("(document.querySelectorAll('#gridContent > *').length)"),
    api: apiHits.slice()
  };

  try { proc.kill('SIGKILL'); } catch (e) {}
  console.log(JSON.stringify(out));
}

main().catch(e => {
  console.error(String(e && e.stack || e));
  process.exit(1);
});
