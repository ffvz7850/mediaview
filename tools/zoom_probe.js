'use strict';
/* 回归：放大 3 倍 → 切下一张，滑走那张不许带着 scale(3) 留在 prev 槽里 */
const { spawn } = require('child_process');
const path=require('path'),os=require('os');
const EDGE='C://Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT=11500+Math.floor(Math.random()*50);
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const BASE=process.argv[2], DIR=process.argv[3];
(async()=>{
  const proc=spawn(EDGE,['--headless=new','--no-first-run','--user-data-dir='+path.join(os.tmpdir(),'mv-zm-'+Date.now()),
    '--remote-debugging-port='+PORT,'--window-size=390,844','about:blank'],{stdio:'ignore'});
  let pages=[];
  for(let i=0;i<60;i++){try{const r=await fetch('http://127.0.0.1:'+PORT+'/json/list');pages=await r.json();if(pages.some(t=>t.type==='page'))break;}catch(e){}await sleep(250);}
  const page=pages.filter(t=>t.type==='page')[0];
  const ws=new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((res,rej)=>{ws.onopen=res;ws.onerror=rej;});
  let id=0;const pending=new Map();
  ws.onmessage=ev=>{const m=JSON.parse(ev.data);if(m.id&&pending.has(m.id)){pending.get(m.id)(m);pending.delete(m.id);}};
  const send=(m,p)=>{const mid=++id;ws.send(JSON.stringify({id:mid,method:m,params:p||{}}));return new Promise(r=>pending.set(mid,r));};
  const evalJS=async e=>{const r=await send('Runtime.evaluate',{expression:e,returnByValue:true,awaitPromise:true});
    if(r.result&&r.result.exceptionDetails)throw new Error('EXC '+JSON.stringify(r.result.exceptionDetails).slice(0,200));return r.result.result.value;};
  await send('Runtime.enable');await send('Page.enable');
  await send('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:3,mobile:true});
  await send('Emulation.setTouchEmulationEnabled',{enabled:true,maxTouchPoints:5});
  const names=require('fs').readdirSync(DIR).filter(n=>/\.jpe?g$/i.test(n)).sort().reverse();
  const start=path.join(DIR,names[4]);
  await send('Page.navigate',{url:BASE+'/?dir='+encodeURIComponent(DIR)+'&p='+encodeURIComponent(start)});
  await sleep(5200);

  const tf=()=>evalJS(`[].map.call(document.querySelectorAll('.v-track > .v-vbox img'),function(i){return i.style.transform||'(none)';})`);
  console.log('before zoom:', await tf());
  // 双击中间 1/3（两次 tap，间隔 80ms）
  for (let k=0;k<2;k++){
    await send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:195,y:420,id:1}]});
    await send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});
    await sleep(80);
  }
  await sleep(700);
  console.log('after dblclick:', await tf());
  // 点右 1/3 切下一张
  await send('Input.dispatchTouchEvent',{type:'touchStart',touchPoints:[{x:350,y:420,id:1}]});
  await send('Input.dispatchTouchEvent',{type:'touchEnd',touchPoints:[]});
  await sleep(1200);
  const after = await tf();
  console.log('after switch:', after, 'counter=', await evalJS('document.getElementById("vCounterFloat").textContent'));
  const stale = after.filter(s=>/scale\((?!1\))/.test(s));
  console.log(stale.length ? ('STALE ZIP: '+JSON.stringify(stale)) : 'no stale zoom on neighbors — OK');
  try{ws.close();}catch(e){}try{proc.kill();}catch(e){}
})().catch(e=>{console.error('FAIL',e);process.exit(1);});
