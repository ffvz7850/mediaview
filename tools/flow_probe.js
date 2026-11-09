/*
 * flow 模块的真实渲染探针（headless Edge + CDP）
 *
 * 验证四件事（这些都是"单元测试证明不了"的）：
 *   ① 宽度约束：每张图宽度 === 滚动容器宽度（左右不留白）
 *   ② 无缝拼接：相邻两张 rect.bottom 与 rect.top 的差 === 0（不是"看起来差不多"）
 *   ③ 窗口化 + 索引：滚动后挂载的格子会换、HUD 计数跟上
 *   ④ 关闭：close() 返回"看到第几张"
 *
 * 用法：node tools/flow_probe.js [--shot out.png]
 */
'use strict';

const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT = 9333 + Math.floor(Math.random() * 200);
const HARNESS = 'file:///' + path.resolve(__dirname, 'flow_probe.html').replace(/\\/g, '/');

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

async function targets() {
  const r = await fetch('http://127.0.0.1:' + PORT + '/json/list');
  return r.json();
}

async function main() {
  const shotIdx = process.argv.indexOf('--shot');
  const shot = shotIdx > 0 ? process.argv[shotIdx + 1] : '';

  const proc = spawn(EDGE, [
    '--headless=new',
    '--disable-gpu',
    '--no-first-run',
    '--no-default-browser-check',
    '--user-data-dir=' + path.join(require('os').tmpdir(), 'mv-flow-probe-' + Date.now()),
    '--remote-debugging-port=' + PORT,
    '--window-size=390,844',
    HARNESS
  ], { stdio: 'ignore' });

  let ws = null;
  try {
    let list = [];
    for (let i = 0; i < 60; i++) {
      try {
        list = await targets();
        if (list.some(t => t.type === 'page' && /flow_probe\.html/.test(t.url))) break;
      } catch (e) {}
      await sleep(250);
    }
    const page = list.filter(t => t.type === 'page' && /flow_probe\.html/.test(t.url))[0];
    if (!page) throw new Error('找不到探针页面目标');

    ws = new WebSocket(page.webSocketDebuggerUrl);
    let id = 0;
    const pending = new Map();
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    ws.onmessage = ev => {
      const m = JSON.parse(ev.data);
      if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
    };
    function send(method, params) {
      const mid = ++id;
      ws.send(JSON.stringify({ id: mid, method, params: params || {} }));
      return new Promise(res => pending.set(mid, res));
    }
    async function evalJS(expr) {
      const r = await send('Runtime.evaluate', {
        expression: expr, returnByValue: true, awaitPromise: true
      });
      if (r.result && r.result.exceptionDetails) {
        throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails));
      }
      return r.result.result.value;
    }

    await send('Runtime.enable');
    await send('Page.enable');
    // 按手机模拟：390×844 CSS px、DPR 3、触屏（这样 clientWidth 才是真机的宽度，
    // 也是"宽度约束到手机宽度"这条需求的正确度量口径）
    await send('Emulation.setDeviceMetricsOverride', {
      width: 390, height: 844, deviceScaleFactor: 3, mobile: true
    });
    await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
    await sleep(200);

    // 固定 sleep 不够：target 出现只代表"导航开始"，flow.js 可能还没执行完。
    // 不等这一步会偶发 `Cannot read properties of undefined (reading 'open')`（探针假红）。
    let ready = false;
    for (let i = 0; i < 60; i++) {
      try {
        if (await evalJS("!!(window.__probe && window.MVFlow)")) { ready = true; break; }
      } catch (e) {}
      await sleep(150);
    }
    if (!ready) throw new Error('探针桩没就绪（flow.js 没执行完？）');

    const out = {};
    out.media = await evalJS("({coarse: matchMedia('(pointer: coarse)').matches, hoverNone: matchMedia('(hover: none)').matches, w: innerWidth})");
    out.open = await evalJS('window.__probe.open(0)');
    await sleep(400);
    out.at0 = await evalJS('window.__probe.metrics()');

    out.idxAt1500 = await evalJS('window.__probe.scrollTo(1500)');
    await sleep(250);
    out.at1500 = await evalJS('window.__probe.metrics()');

    out.idxAt4500 = await evalJS('window.__probe.scrollTo(4500)');
    await sleep(250);
    out.at4500 = await evalJS('window.__probe.metrics()');

    // 2.4.32：从**中间某张**打开必须定位到那张（曾经因 spacer 未撑开而被 clamp 到顶部）
    out.openAt4 = await evalJS('window.__probe.open(4)');
    await sleep(400);
    out.at4 = await evalJS('window.__probe.metrics()');

    // 2.4.44：末张 —— 几何上顶不到中部，只能贴底；但"当前张"必须是它
    out.openAtLast = await evalJS('window.__probe.open(7)');
    await sleep(400);
    out.atLast = await evalJS('window.__probe.metrics()');

    out.totalHeight = await evalJS('window.__probe.totalHeight()');
    out.closedIdx = await evalJS('window.__probe.close()');
    await sleep(150);

    if (shot) {
      await evalJS('window.__probe.open(0)');
      await sleep(500);
      const s = await send('Page.captureScreenshot', { format: 'png' });
      fs.writeFileSync(shot, Buffer.from(s.result.data, 'base64'));
      out.shot = shot;
    }

    console.log(JSON.stringify(out, null, 2));
  } finally {
    try { if (ws) ws.close(); } catch (e) {}
    try { proc.kill(); } catch (e) {}
  }
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
