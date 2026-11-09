/*
 * 守卫条目耐久性探针（真实 app，手机 profile）
 *
 * 目的：复现/排除「第一次返回正常、第二次返回就退出应用」。
 * 宿主按返回 = 让 WebView 退一格历史；能不能退出去取决于**历史里还有没有本应用的条目**。
 * 所以这里连续模拟 N 次宿主返回，每次记录：
 *   - 文档还在不在（mark）
 *   - history.length / history.state（守卫条目在不在）
 *   - popstate 次数
 *   - 面包屑（有没有真退一层）
 *   - scrollY / 视口是否仍在本应用
 *
 * 用法：node tools/guard_probe.js http://127.0.0.1:PORT/app/mediaview/ "起始目录" [次数] [截图]
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
const TIMES = parseInt(process.argv[4] || '5', 10);
const SHOT = process.argv[5] || '';
const PORT = 10450 + Math.floor(Math.random() * 60);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-guard-' + Date.now()),
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
  const snap = () => evalJS(`(function(){
    var c = document.getElementById('crumb');
    var st = history.state;
    return {
      mark: window.__mark || '(gone)',
      hist: history.length,
      state: st ? JSON.stringify(st).slice(0, 60) : 'null',
      isGuard: !!(st && st.mvGuard),
      crumb: c ? c.textContent.replace(/\\s+/g, ' ').trim().slice(0, 46) : '',
      url: location.search.slice(0, 40),
      pops: window.__pops || 0,
      depth: (function(){ try { var g = window.__mvGuardInfo && window.__mvGuardInfo(); return g ? g.depth : -1; } catch (e) { return -1; } })(),
      vdepth: (function(){ try { var g = window.__mvGuardInfo && window.__mvGuardInfo(); return g ? g.viewer : -1; } catch (e) { return -1; } })()
    };
  })()`);

  await send('Runtime.enable');
  await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);
  await evalJS(`window.__mark = 'alive'; window.__pops = 0;
    window.addEventListener('popstate', function(){ window.__pops++; });`);

  const steps = [];
  steps.push({ name: 'open', s: await snap() });

  // 进两层目录，制造"要退两次"的场景
  for (let k = 0; k < 2; k++) {
    const r = await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      var d = cells.filter(function (c) { return !!c.querySelector('.cell-fallback'); })[0];
      if (!d) return { ok: false };
      d.querySelector('.cell-inner').click();
      return { ok: true };
    })()`);
    await sleep(1300);
    steps.push({ name: 'enter#' + (k + 1), ok: r.ok, s: await snap() });
  }

  // 连续模拟宿主返回：观察守卫是否每次都能补上
  for (let k = 0; k < TIMES; k++) {
    await evalJS('window.__mvpSimHost = 1; history.back(); window.__mvpSimHost = 0;');
    await sleep(900);
    steps.push({ name: 'back#' + (k + 1), s: await snap() });
  }

  // 连退两格（模拟宿主"一次手势叠加两次 traversal"）：守卫必须还撑得住
  await evalJS('history.back(); history.back();');
  await sleep(900);
  steps.push({ name: 'burst', s: await snap() });

  let shot = null;
  if (SHOT) {
    const r = await send('Page.captureScreenshot', { format: 'png' });
    if (r.result && r.result.data) { fs.writeFileSync(SHOT, Buffer.from(r.result.data, 'base64')); shot = SHOT; }
  }

  console.log(JSON.stringify({ steps, shot }, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
