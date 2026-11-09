/*
 * flow 上线后的"只在手机版生效"验证（真实 app，不是 harness）
 *
 * 两个 profile 各跑一次：
 *   · mouse（不带设备模拟）→ 不许加载 flow.js、设置行必须 display:none
 *   · touch（390×844、DPR3、触屏）→ 必须加载 flow.js、设置行 display:flex
 * 同时收集页面 JS 异常（window.onerror）—— 加了一堆新代码，先确认没把主页搞崩。
 *
 * 用法：node tools/flow_live_probe.js http://127.0.0.1:PORT/app/mediaview/
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8130/app/mediaview/';
const PORT = 9500 + Math.floor(Math.random() * 200);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function openSession(mobile) {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(require('os').tmpdir(), 'mv-live-' + Date.now() + '-' + (mobile ? 'm' : 'd')),
    '--remote-debugging-port=' + PORT,
    mobile ? '--window-size=390,844' : '--window-size=1200,800',
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
  // 在任何页面脚本之前装错误收集器：新代码有没有把主页搞崩，用这个说话
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: 'window.__mvErrs=[];' +
      'window.addEventListener("error",function(e){window.__mvErrs.push(String(e.message||e));});' +
      'window.addEventListener("unhandledrejection",function(e){' +
      'var r=e.reason;window.__mvErrs.push("rejection:"+String((r&&r.message)||r));});'
  });
  if (mobile) {
    await send('Emulation.setDeviceMetricsOverride', { width: 390, height: 844, deviceScaleFactor: 3, mobile: true });
    await send('Emulation.setTouchEmulationEnabled', { enabled: true, maxTouchPoints: 5 });
  }
  await send('Page.navigate', { url: process.env.MV_LIVE_URL || URL_BASE });
  await sleep(2200);

  const out = await evalJS(`(function(){
    var errs = window.__mvErrs || [];
    var row = document.getElementById('rowMobileFlow');
    var btn = document.getElementById('btnSettings');
    if (btn) btn.click();
    var disp = row ? getComputedStyle(row).display : 'missing';
    return {
      flowLoaded: window.__mvFlowLoaded === true,
      touchLoaded: window.__mvTouchLoaded === true,
      hasMVFlow: typeof window.MVFlow === 'object' && !!window.MVFlow,
      mobileFlowCfg: (window.__mvCfgProbe || null),
      rowDisplay: disp,
      coarse: matchMedia('(pointer: coarse)').matches,
      errs: errs
    };
  })()`);

  if (mobile) {
    // 端到端：点第一张图 → 应进入竖图流；再按返回 → 应关流并回到网格
    // ⚠️ 必须点 .cell-inner（onclick 挂在内层）：对 .cell 直接 .click() 不会命中，
    //    因为 dispatch 不经过命中测试、也不会向下冒泡到子元素（第一版就踩了这个坑）。
    out.openFlow = await evalJS(`(function(){
      var inner = document.querySelector('.cell .cell-inner');
      var cell = document.querySelector('.cell');
      if (!inner) return { error: 'no cell', cells: document.querySelectorAll('.cell').length,
                           search: location.search };
      inner.click();
      return { clicked: true, cellClass: cell ? cell.className : '',
               search: location.search, cells: document.querySelectorAll('.cell').length };
    })()`);
    await sleep(900);
    out.flowState = await evalJS(`(function(){
      var root = document.getElementById('vfRoot');
      return {
        isOpen: !!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen()),
        visible: !!(root && getComputedStyle(root).display !== 'none'),
        viewerHidden: document.getElementById('viewer').classList.contains('hidden')
      };
    })()`);
    await evalJS('history.back()');
    await sleep(800);
    out.afterBack = await evalJS(`(function(){
      var root = document.getElementById('vfRoot');
      return {
        isOpen: !!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen()),
        visible: !!(root && getComputedStyle(root).display !== 'none'),
        viewerHidden: document.getElementById('viewer').classList.contains('hidden')
      };
    })()`);
  }

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
  return out;
}

(async () => {
  const desktop = await openSession(false);
  const mobile = await openSession(true);
  const res = { desktop, mobile };
  console.log(JSON.stringify(res, null, 2));

  const fails = [];
  if (desktop.flowLoaded) fails.push('桌面加载了 flow.js（应只在触屏档注入）');
  if (desktop.hasMVFlow) fails.push('桌面暴露了 window.MVFlow');
  if (desktop.rowDisplay !== 'none') fails.push('桌面设置行应 display:none，实际 ' + desktop.rowDisplay);
  if (!mobile.flowLoaded) fails.push('手机端没加载 flow.js');
  if (!mobile.hasMVFlow) fails.push('手机端没有 window.MVFlow');
  if (mobile.rowDisplay === 'none') fails.push('手机端设置行被隐藏了（应可见）');
  // 端到端三条：进得去、看得见、退得干净（不然会一直叠在网格上）
  if (mobile.openFlow && mobile.openFlow.error) {
    fails.push('点开图片失败: ' + mobile.openFlow.error + ' ' + JSON.stringify(mobile.openFlow));
  }
  if (mobile.flowState && !mobile.flowState.isOpen) fails.push('点击图片后没有进入竖图流');
  if (mobile.flowState && mobile.flowState.viewerHidden) fails.push('进入流后查看器仍是隐藏的');
  if (mobile.afterBack && mobile.afterBack.isOpen) fails.push('返回后竖图流没关掉');
  if (mobile.afterBack && !mobile.afterBack.viewerHidden) fails.push('返回后查看器没关掉');
  // 已知无害：不在飞牛桌面 iframe 里时 host bridge 必然 reject（既有行为，与本次无关）
  function realErrs(list) {
    return (list || []).filter(function (m) { return m.indexOf('Host bridge is not available') < 0; });
  }
  if (realErrs(desktop.errs).length) fails.push('桌面有 JS 异常: ' + JSON.stringify(realErrs(desktop.errs)));
  if (realErrs(mobile.errs).length) fails.push('手机有 JS 异常: ' + JSON.stringify(realErrs(mobile.errs)));
  if (fails.length) { console.error('LIVE PROBE FAIL:\n - ' + fails.join('\n - ')); process.exit(1); }
  console.log('LIVE PROBE OK');
})();
