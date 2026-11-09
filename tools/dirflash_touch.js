/*
 * 手机档「进/退文件夹闪一下」取证（真实 app，触屏 profile）
 *
 * 触屏档换目录是**整页导航**（location.replace），所以"闪"发生在两个阶段：
 *   ① 旧页面卸载 → 新页面首帧；② 新页面空白等 /api/list。
 * 2.4.31 用「目录快照」把 ② 消掉：回到刚看过、内容没变的目录时，快照直接铺满、
 * 且真实列表回来发现一致就**不重建**。
 *
 * 本探针在每个新文档里数：从 navigationStart 到「#gridContent 里第一次出现格子」的毫秒数。
 * 判据：
 *   · 有快照的那次（返回父目录）必须明显快于首次进入（无快照）
 *   · 且 cells 数量应当一步到位（不是 0 → 几个 → 全部）
 *
 * 用法：node tools/dirflash_touch.js http://127.0.0.1:PORT/app/mediaview/ "父目录绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8140/app/mediaview/';
const DIR = process.argv[3] || '';
const PORT = 9900 + Math.floor(Math.random() * 90);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-flasht-' + Date.now()),
    '--remote-debugging-port=' + PORT,
    '--window-size=390,844',
    'about:blank'
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
  // 给网络加 300ms 延迟：本地 list 太快（几十毫秒）看不出差异，
  // 真实 NAS/宿主代理下接口就是这个量级。这样"有没有快照"的差距才量得出来。
  await send('Network.enable');
  await send('Network.emulateNetworkConditions', {
    offline: false, latency: 300, downloadThroughput: -1, uploadThroughput: -1
  });

  // 每个新文档都装：记录"从导航开始到格子上屏"的毫秒数与当时的格子数，
  // 以及整个过程中格子数的变化序列（用来判断是否"一步到位"）
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: `window.__mvBoot = { nav: Math.round(performance.now()), tCells: -1, cells: 0, seq: [] };
      (function(){
        function watch(){
          var c = document.getElementById('gridContent');
          if (!c) { requestAnimationFrame(watch); return; }
          (function tick(){
            var k = c.children.length;
            if (window.__mvBoot.seq.length === 0 || window.__mvBoot.seq[window.__mvBoot.seq.length-1] !== k) {
              window.__mvBoot.seq.push(k);
            }
            if (k > 0 && window.__mvBoot.tCells < 0) {
              window.__mvBoot.tCells = Math.round(performance.now() - window.__mvBoot.nav);
              window.__mvBoot.cells = k;
            }
            requestAnimationFrame(tick);
          })();
        }
        requestAnimationFrame(watch);
      })();`
  });

  const out = { steps: [] };

  const readBoot = async () => evalJS('window.__mvBoot || null');

  // 首次打开父目录（无快照 → 要等 list）
  const url = URL_BASE + '?dir=' + encodeURIComponent(DIR);
  await send('Page.navigate', { url });
  await sleep(2600);
  out.steps.push({ name: 'open-parent(first)', boot: await readBoot() });

  // 点进 sub（首次，无快照）
  await evalJS(`(function(){
    var cells = document.querySelectorAll('#gridContent .cell');
    for (var i = 0; i < cells.length; i++) {
      if (/sub/.test(cells[i].textContent)) { cells[i].querySelector('.cell-inner').click(); return 'ok'; }
    }
    return 'nosub';
  })()`);
  await sleep(2600);
  out.steps.push({ name: 'enter-sub(first)', boot: await readBoot(),
                   path: await evalJS('location.search') });

  // 返回父目录（有快照）—— 直接用 URL 导航模拟"返回"
  //（Windows 路径不匹配 /volN/1000，点 navBack 会跳到聚合根，测不到目标路径）
  await send('Page.navigate', { url });
  await sleep(2600);
  out.steps.push({ name: 'back-to-parent(snap)', boot: await readBoot(),
                   path: await evalJS('location.search') });

  // 再进 sub（此时也有快照了）
  await evalJS(`(function(){
    var cells = document.querySelectorAll('#gridContent .cell');
    for (var i = 0; i < cells.length; i++) {
      if (/sub/.test(cells[i].textContent)) { cells[i].querySelector('.cell-inner').click(); return 'ok'; }
    }
    return 'nosub';
  })()`);
  await sleep(2600);
  out.steps.push({ name: 'enter-sub(again)', boot: await readBoot(),
                   path: await evalJS('location.search') });

  console.log(JSON.stringify(out, null, 2));

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
