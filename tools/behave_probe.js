'use strict';
/*
 * 行为回归探针：切图之后"画面上那张"是不是"计数指向那张"。
 * 2.4.53 把 syncTrackToIndex 改成按路径认领多个盒子（为了省掉重复解码），
 * 这条必须验：连点 4 次「下一张」再连点 4 次「上一张」，每一格都对得上。
 */
const { spawn } = require('child_process');
const path = require('path'); const os = require('os');
const EDGE = 'C://Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT = 11400 + Math.floor(Math.random() * 50);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const BASE = process.argv[2];
const DIR = process.argv[3];

(async () => {
  const proc = spawn(EDGE, ['--headless=new','--no-first-run','--user-data-dir='+path.join(os.tmpdir(),'mv-bh-'+Date.now()),
    '--remote-debugging-port='+PORT,'--window-size=390,844','about:blank'], {stdio:'ignore'});
  let pages=[];
  for(let i=0;i<60;i++){try{const r=await fetch('http://127.0.0.1:'+PORT+'/json/list');pages=await r.json();if(pages.some(t=>t.type==='page'))break;}catch(e){}await sleep(250);}
  const page=pages.filter(t=>t.type==='page')[0];
  const ws=new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res,rej)=>{ws.onopen=res;ws.onerror=rej;});
  let id=0;const pending=new Map();
  ws.onmessage=ev=>{const m=JSON.parse(ev.data);if(m.id&&pending.has(m.id)){pending.get(m.id)(m);pending.delete(m.id);}};
  const send=(m,p)=>{const mid=++id;ws.send(JSON.stringify({id:mid,method:m,params:p||{}}));return new Promise(r=>pending.set(mid,r));};
  const evalJS=async e=>{const r=await send('Runtime.evaluate',{expression:e,returnByValue:true,awaitPromise:true});
    if(r.result&&r.result.exceptionDetails)throw new Error('EXC '+JSON.stringify(r.result.exceptionDetails).slice(0,300));return r.result.result.value;};
  await send('Runtime.enable');await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:3,mobile:true});
  await send('Emulation.setTouchEmulationEnabled',{enabled:true,maxTouchPoints:5});

  const names=require('fs').readdirSync(DIR).filter(n=>/\.jpe?g$/i.test(n)).sort().reverse();
  const start=path.join(DIR,names[4]);
  await send('Page.navigate',{url:BASE+'/?dir='+encodeURIComponent(DIR)+'&p='+encodeURIComponent(start)});
  await sleep(5000);

  const snap = () => evalJS(`(function(){
    var t=document.querySelector('.v-track');
    var mids=[].map.call(t?t.children:[],function(b){return (b._itemPath||'').split('\\\\').pop();});
    var c=(document.querySelector('.v-track > .v-vbox:nth-child(2) img')||{});
    return {
      counter:(document.getElementById('vCounterFloat')||{}).textContent,
      boxes:mids,
      centerVisible: c.style ? (c.style.visibility!=='hidden') : null,
      centerTransform: c.style ? (c.style.transform||'') : '',
      centerSrc: (c.currentSrc||'').split('%5C').pop().split('&')[0]
    };
  })()`);

  const out=[{step:'open', s:await snap()}];
  const tap = async (x) => {
    await send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x,y:420,id:1}]});
    await send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});
    await sleep(850);
  };
  for (let i=0;i<4;i++){ await tap(350); out.push({step:'next'+(i+1), s:await snap()}); }
  for (let i=0;i<4;i++){ await tap(40); out.push({step:'prev'+(i+1), s:await snap()}); }

  // 断言：计数递增/递减一格，中间那格 == 计数指向那张
  let bad=0;
  for (const r of out) {
    const [n] = (r.s.counter||'').split('/');
    const expect = names[parseInt(n,10)-1];
    const got = r.s.boxes[1];
    const ok = got===expect;
    if(!ok) bad++;
    console.log(r.step.padEnd(7), 'count=',String(n).padStart(2), 'center=',String(got).padEnd(14),
      'expect=',String(expect).padEnd(14), ok?'OK':'✗ MISMATCH',
      'vis=',r.s.centerVisible, 'tf=', JSON.stringify(r.s.centerTransform));
    if (r.s.boxes.length!==3) console.log('   ⚠ 轨道子元素数 =', r.s.boxes.length, r.s.boxes);
  }
  console.log(bad===0 ? 'ALL OK' : ('MISMATCH ' + bad));
  try{ws.close();}catch(e){}try{proc.kill();}catch(e){}
})().catch(e=>{console.error('FAIL',e);process.exit(1);});
