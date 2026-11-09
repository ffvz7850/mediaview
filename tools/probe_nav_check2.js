/*
 * 验证导航探针 v2 的归因是否正确：
 *   A) 点文件夹 → 卸载应归因为 tap（不是"无前兆"）
 *   B) 宿主返回（脚本 history.back）→ 计数 ✅ +1，卸载归因为 pop
 * 用法：node tools/probe_nav_check2.js http://127.0.0.1:PORT/app/mediaview/ "父目录绝对路径" [截图]
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2];
const DIR = process.argv[3] || '';
const SHOT = process.argv[4] || '';
const PORT = 10250 + Math.floor(Math.random() * 60);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-nav2-' + Date.now()),
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
  const head = () => evalJS(`(function(){
    var h = document.querySelector('#mvNavProbe');
    if (!h) return { error: 'no panel' };
    var lines = [].slice.call(h.children[1].children).map(function (x) { return x.innerText; });
    return { lines: lines, hist: history.length, boot: sessionStorage.getItem('mv_probe_boot_v1') };
  })()`);

  await send('Runtime.enable');
  await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  const url = URL_BASE + '?dir=' + encodeURIComponent(DIR);
  await send('Page.navigate', { url });
  await sleep(2600);
  const s1 = await head();

  // A) 点文件夹（应归因 tap）
  await evalJS(`(function(){
    var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    var d = cells.filter(function (c) { return !!c.querySelector('.cell-fallback'); })[0];
    if (d) d.querySelector('.cell-inner').click();
    return !!d;
  })()`);
  await sleep(2200);
  const s2 = await head();

  // B) 模拟宿主返回（应计数 ✅ 且归因 pop）
  // 用 __mvpSimHost 模拟"宿主发起的后退"（不加这个标记会被当成我们自己退历史）
  await evalJS('window.__mvpSimHost = 1; history.back(); window.__mvpSimHost = 0;');
  await sleep(2200);
  const s3 = await head();

  let shot = null;
  if (SHOT) {
    const r = await send('Page.captureScreenshot', { format: 'png' });
    if (r.result && r.result.data) { fs.writeFileSync(SHOT, Buffer.from(r.result.data, 'base64')); shot = SHOT; }
  }

  console.log(JSON.stringify({ afterBoot: s1, afterTapFolder: s2, afterBack: s3, shot }, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('CHECK FAIL:', e.message); process.exit(1); });
