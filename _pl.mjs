import { spawn } from 'node:child_process';
const EDGE = 'C://Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const PORT = 9375;
const proc = spawn(EDGE, ['--headless=new','--no-sandbox',`--remote-debugging-port=${PORT}`,'--user-data-dir=C://Users//schallophy//AppData//Local//Temp//edge-cdp-pl','about:blank'], {stdio:'ignore'});
const sleep = ms => new Promise(r=>setTimeout(r,ms));
let page;
for (let i=0;i<40;i++){ try{ const l=await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json(); page=l.find(t=>t.type==='page'); if(page) break; }catch{} await sleep(250); }
const ws = new WebSocket(page.webSocketDebuggerUrl);
let id=0; const pending=new Map();
ws.addEventListener('message', e=>{ const m=JSON.parse(e.data); if(m.id&&pending.has(m.id)){const p=pending.get(m.id);pending.delete(m.id);m.error?p.reject(new Error(JSON.stringify(m.error))):p.resolve(m.result);} });
await new Promise(r=>ws.addEventListener('open',r));
const send=(method,params={})=>new Promise((res,rej)=>{const n=++id;pending.set(n,{resolve:res,reject:rej});ws.send(JSON.stringify({id:n,method,params}));});
await send('Page.enable'); await send('Runtime.enable');
await send('Page.navigate',{url:'http://192.168.137.193:7788/item/72db900c63e6bd9a'});
await sleep(2800);
const r = await send('Runtime.evaluate',{returnByValue:true,awaitPromise:true,expression:`
(async ()=>{
  const out = {};
  out.artplayerLoaded = typeof Artplayer === 'function';
  out.subs = JSON.stringify(SUBS);
  document.getElementById('webplay').click();
  await new Promise(r=>setTimeout(r,2500));
  out.btnText = document.getElementById('webplay').textContent;
  out.playerVisible = !document.getElementById('playerSec').hidden;
  out.errShown = !document.getElementById('playerErr').hidden;
  const v = document.querySelector('#player video');
  out.videoError = v ? (v.error ? v.error.code : null) : 'no-video';
  out.hasSubtitleTrack = !!document.querySelector('#player track, #player .art-subtitle');
  out.subtitleUrl = art && art.subtitle ? 'configured' : 'none';
  return JSON.stringify(out, null, 1);
})()`});
console.log(r.result.value);
ws.close(); proc.kill();
