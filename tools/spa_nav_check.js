/*
 * 2.4.35「零重载」验证（真实 app，手机 profile）
 *
 * 断言：
 *   ① 点文件夹后 **文档没有重载**（window.__mark 还在、boot 号不变）
 *   ② 面包屑变了（说明确实换目录了）
 *   ③ 模拟宿主返回（popstate）也**不重载**，并退回上一层
 *   ④ history.length 不随换目录增长（换目录不再压历史）
 *
 * 用法：node tools/spa_nav_check.js http://127.0.0.1:PORT/app/mediaview/ "父目录" [截图]
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
const PORT = 10350 + Math.floor(Math.random() * 60);
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-spa-' + Date.now()),
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
    return {
      mark: window.__mark || '(gone)',
      boot: sessionStorage.getItem('mv_probe_boot_v1') || '',
      hist: history.length,
      crumb: c ? c.textContent.replace(/\\s+/g, ' ').trim() : '',
      cells: document.querySelectorAll('#gridContent .cell').length
    };
  })()`);

  await send('Runtime.enable');
  await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
  await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });

  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);
  await evalJS("window.__mark = 'alive'");
  const s1 = await snap();

  // ① 点文件夹（内部切换，不许重载）
  const clicked = await evalJS(`(function(){
    var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
    var d = cells.filter(function (c) { return !!c.querySelector('.cell-fallback'); })[0];
    if (!d) return { ok: false };
    d.querySelector('.cell-inner').click();
    return { ok: true };
  })()`);
  await sleep(1500);
  const s2 = await snap();

  // ② 模拟宿主返回（popstate，应退回上一层且不重载）
  await evalJS('window.__mvpSimHost = 1; history.back(); window.__mvpSimHost = 0;');
  await sleep(1500);
  const s3 = await snap();

  let shot = null;
  if (SHOT) {
    const r = await send('Page.captureScreenshot', { format: 'png' });
    if (r.result && r.result.data) { fs.writeFileSync(SHOT, Buffer.from(r.result.data, 'base64')); shot = SHOT; }
  }

  const fails = [];
  if (!clicked.ok) fails.push('起始目录里没有可点的文件夹');
  if (s2.mark !== 'alive') fails.push('★ 点文件夹后文档被重载了（mark 丢失）');
  if (s1.boot !== s2.boot) fails.push('★ boot 号变了 ⇒ 发生了重载');
  if (s2.crumb === s1.crumb) fails.push('★ 面包屑没变 ⇒ 其实没进目录（' + s1.crumb + '）');
  if (s3.mark !== 'alive') fails.push('★ 返回后文档被重载了（mark 丢失）');
  if (s3.boot !== s2.boot) fails.push('★ 返回后 boot 号变了 ⇒ 发生了重载');
  // 测试机是 Windows 路径（不在 /vol1/1000 里），goUpLevel 会判成"出用户目录"→ 回聚合根，
  //   所以这里只要求"确实退了一层"，不要求回到原字符串。
  if (s3.crumb === s2.crumb) fails.push('★ 返回后没有退层（crumb 仍是 ' + s3.crumb + '）');
  if (s2.hist > s1.hist) fails.push('★ 换目录把历史撑长了（' + s1.hist + ' → ' + s2.hist + '）');

  console.log(JSON.stringify({ boot: s1, afterEnter: s2, afterBack: s3, fails, shot }, null, 2));
  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
  if (fails.length) process.exit(1);
}

main().catch(e => { console.error('CHECK FAIL:', e.message); process.exit(1); });
