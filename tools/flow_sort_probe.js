/*
 * 竖图流排序跟随性取证（真实 app，手机 profile）
 *
 * 做法：进目录 → 用工具栏的排序下拉切到某个规则（并点箭头切方向）
 *      → 读网格前 N 个格子的文件名（= 用户所见顺序）
 *      → 点第一个格子进竖图流 → 读流里前 N 个 item 的文件名（= 流里的顺序）
 *      → 两者逐位比对
 *
 * 用法：node tools/flow_sort_probe.js http://127.0.0.1:PORT/app/mediaview/ "目录绝对路径"
 */
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const os = require('os');

const EDGE = process.env.MV_EDGE ||
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const URL_BASE = process.argv[2] || 'http://127.0.0.1:8140/app/mediaview/';
const DIR = process.argv[3] || '';
const PORT = 9750 + Math.floor(Math.random() * 90);

const sleep = ms => new Promise(r => setTimeout(r, ms));

async function main() {
  const proc = spawn(EDGE, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--user-data-dir=' + path.join(os.tmpdir(), 'mv-sort-' + Date.now()),
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
  await send('Page.navigate', { url: URL_BASE + '?dir=' + encodeURIComponent(DIR) });
  await sleep(2600);

  const out = { cases: [], errors: [] };

  // 用例：字段 + 方向（方向用箭头按钮翻）
  const CASES = [
    { field: 'name:asc', wantDesc: false },
    { field: 'name:asc', wantDesc: true },   // 名称降序
    { field: 'size:desc', wantDesc: true },  // 大小降序
    { field: 'size:desc', wantDesc: false }, // 大小升序
    { field: 'type:asc', wantDesc: false },  // 类型升序（扩展名）
    { field: 'type:asc', wantDesc: true },   // 类型降序
    { field: 'mtime:desc', wantDesc: true }  // 修改时间降序
  ];

  for (const c of CASES) {
    // 选字段
    const applied = await evalJS(`(function(){
      var sel = document.getElementById('sortSel');
      if (!sel) return { error: 'no sortSel' };
      // 找到字段匹配的 option（方向另管）
      var f = ${JSON.stringify(c.field.split(':')[0])};
      var hit = null;
      for (var i = 0; i < sel.options.length; i++) {
        if (sel.options[i].value.split(':')[0] === f) { hit = sel.options[i]; break; }
      }
      if (!hit) return { error: 'no option for ' + f };
      sel.value = hit.value;
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      return { value: hit.value };
    })()`);
    await sleep(1200);

    // 方向：读箭头当前状态，不一致就点一下
    const dirNow = await evalJS(`(function(){
      var b = document.getElementById('sortDirBtn');
      return { text: b ? b.textContent.trim() : '', title: b ? b.title : '' };
    })()`);
    const isDesc = dirNow.text === '↓';
    if (isDesc !== c.wantDesc) {
      await evalJS("document.getElementById('sortDirBtn').click()");
      await sleep(1200);
    }

    const grid = await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
      return {
        sort: (window.localStorage.getItem('mediaview_sort') || ''),
        names: imgs.map(function (x) {
          var n = x.querySelector('.cell-label-name');
          return n ? n.textContent : '';
        })
      };
    })()`);

    // 点第一个进流
    const opened = await evalJS(`(function(){
      var cells = [].slice.call(document.querySelectorAll('#gridContent .cell'));
      var imgs = cells.filter(function (x) { return !x.querySelector('.cell-fallback'); });
      if (!imgs.length) return { error: 'no cell' };
      imgs[0].querySelector('.cell-inner').click();
      return { ok: true };
    })()`);
    await sleep(1500);

    const flow = await evalJS(`(function(){
      if (!window.MVFlow || !window.MVFlow.isOpen()) return { error: 'flow not open' };
      // 流里的顺序：把所有已建过的 item 按 data-i 排序读 alt
      var els = [].slice.call(document.querySelectorAll('.vf-item'));
      els.sort(function (a, b) { return (+a.dataset.i) - (+b.dataset.i); });
      // 只能读到"挂载中"的几张，够判断前几张的顺序即可
      var names = els.map(function (e) {
        var im = e.querySelector('img.vf-full');
        return { i: +e.dataset.i, alt: im ? (im.getAttribute('alt') || '') : '' };
      });
      var hud = (document.querySelector('.vf-count') || {}).textContent || '';
      var cur = window.MVFlow.index();
      var curAlt = '';
      var t = document.querySelector('.vf-item[data-i="' + cur + '"] img.vf-full');
      if (t) curAlt = t.getAttribute('alt') || '';
      return { names: names, hud: hud, idx: cur, curAlt: curAlt };
    })()`);

    out.cases.push({
      case: c.field + (c.wantDesc ? ':desc' : ':asc'),
      appliedSort: grid.sort,
      gridNames: grid.names,
      flowNames: (flow.names || []).map(x => x.alt),
      flowIndexes: (flow.names || []).map(x => x.i),
      flowIdx: flow.idx,
      flowCurAlt: flow.curAlt,
      hud: flow.hud,
      opened: opened,
      error: flow.error || null
    });

    // 关流
    await evalJS("try { history.back(); } catch(e) {}");
    await sleep(900);
    const stillOpen = await evalJS("!!(window.MVFlow && window.MVFlow.isOpen && window.MVFlow.isOpen())");
    if (stillOpen) {
      await evalJS("window.MVFlow.close()");
      await sleep(300);
    }
  }

  console.log(JSON.stringify(out, null, 2));

  try { ws.close(); } catch (e) {}
  try { proc.kill(); } catch (e) {}
}

main().catch(e => { console.error('PROBE FAIL:', e.message); process.exit(1); });
