/*
 * 验证「导航探针面板」本身：面板能出、popstate 能记、重载后日志还在
 * 用法：node tools/probe_nav_check.js http://127.0.0.1:PORT/app/mediaview/ "目录绝对路径" [截图路径]
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
const PORT = 10150 + Math.floor(Math.random() * 60);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-navprobe-' + Date.now()),
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
  const readPanel = () => evalJS(`(function(){
    var p = document.getElementById('mvNavProbe');
    if (!p) return { error: 'no panel' };
    return { text: p.innerText, hist: history.length, boot: sessionStorage.getItem('mv_probe_boot_v1') };
  })()`);

  await send('Runtime.enable');
  await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  const url = URL_BASE + '?dir=' + encodeURIComponent(DIR);
  await send('Page.navigate', { url });
  await sleep(2600);
  const boot1 = await readPanel();

  // 模拟一次"系统返回"（等价于宿主让 WebView 历史后退一格）
  await evalJS("history.back()");
  await sleep(900);
  const afterBack = await readPanel();

  // 模拟一次"页面被重载"（宿主弹页栈时，我们看到的正是这种结果：新文档 + 旧日志）
  await send('Page.navigate', { url });
  await sleep(2000);
  const afterReload = await readPanel();

  let shot = null;
  if (SHOT) {
    const r = await send('Page.captureScreenshot', { format: 'png' });
    if (r.result && r.result.data) {
      fs.writeFileSync(SHOT, Buffer.from(r.result.data, 'base64'));
      shot = SHOT;
    }
  }

  console.log(JSON.stringify({ boot1, afterBack, afterReload, shot }, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('CHECK FAIL:', e.message); process.exit(1); });
