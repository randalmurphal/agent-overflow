// Does an active CSS animation license presenting a compensated move with
// tiles still un-rastered? Boots a throwaway headless Chrome on a synthetic
// timeline (scroller + repeated head splices that keep rows screen-stationary)
// and counts compositor draws that land while raster is still in flight.
//
// Arms: 'none' | 'inside' | 'outside' | <N> dots inside the scroller.
//   CHROME_BIN=<chrome> REPEATS=3 node scripts/perfprobe/present-policy-arms.mjs
//
// Written 2026-08-24 to check docs/architecture/frontend-scroll.md
// § The Print Doctrine, which attributed the 2026-08-17 checkerboard to a
// smoothness-priority flip. It is not that: tree_priority never leaves
// SAME_PRIORITY_FOR_BOTH_TREES. See the ledger entry in
// .claude/skills/perf-investigation/REFERENCE.md.
//
// Caveat this before quoting it: headless + SoftwareRenderer + one raster
// thread. The ABSOLUTE numbers mean nothing; only the arm-to-arm comparison
// does.
import { spawn } from 'node:child_process';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { dirname, join } from 'node:path';
const pageUrl = pathToFileURL(join(dirname(fileURLToPath(import.meta.url)), 'present-policy-page.html')).href;
const PORT = 9936;
const ARMS = ['none', 1, 3, 14, 30];
const REPEATS = Number(process.env.REPEATS ?? 3);
const chrome = spawn(process.env.CHROME_BIN, [
  '--headless=new','--no-sandbox',`--remote-debugging-port=${PORT}`,
  `--user-data-dir=/tmp/present-policy-prof-${process.pid}`,'--force-device-scale-factor=1',
  '--window-size=1400,1000','--num-raster-threads=1','about:blank'],{stdio:'ignore'});
async function dt(p){for(let i=0;i<80;i++){try{return await (await fetch(`http://127.0.0.1:${PORT}${p}`)).json();}catch{await sleep(200);}}throw new Error('x');}
const ver=await dt('/json/version');
const ws=new WebSocket(ver.webSocketDebuggerUrl);
await new Promise(r=>ws.addEventListener('open',r,{once:true}));
let id=0;const pend=new Map();let evs=[];
ws.addEventListener('message',e=>{const m=JSON.parse(e.data);
 if(m.id){const p=pend.get(m.id);pend.delete(m.id);m.error?p.rej(new Error(JSON.stringify(m.error))):p.res(m.result);}else evs.push(m);});
const send=(me,pa={},s)=>new Promise((res,rej)=>{const i=++id;pend.set(i,{res,rej});
 ws.send(JSON.stringify({id:i,method:me,params:pa,...(s?{sessionId:s}:{})}));
 setTimeout(()=>{if(pend.has(i)){pend.delete(i);rej(new Error('timeout '+me));}},60000);});
const {targetId}=await send('Target.createTarget',{url: pageUrl});
const {sessionId}=await send('Target.attachToTarget',{targetId,flatten:true});
const S=(m,p)=>send(m,p,sessionId);
await S('Page.enable');await S('Runtime.enable');await S('Emulation.setCPUThrottlingRate',{rate:8});await sleep(900);

async function arm(a){
  await S('Runtime.evaluate',{expression:`window.build(400,${JSON.stringify(a)})`,returnByValue:true});
  await sleep(1400);
  evs=[];
  await send('Tracing.start',{traceConfig:{includedCategories:['cc','viz','benchmark','disabled-by-default-devtools.timeline']},transferMode:'ReturnAsStream'});
  for(let i=0;i<10;i++){await S('Runtime.evaluate',{expression:'window.splice(30)',returnByValue:true});await sleep(180);}
  await sleep(500);
  await send('Tracing.end');
  let handle=null;
  for(let i=0;i<300&&!handle;i++){const e=evs.find(x=>x.method==='Tracing.tracingComplete');if(e)handle=e.params.stream;else await sleep(100);}
  let raw='';for(;;){const c=await send('IO.read',{handle,size:8_000_000});raw+=c.data;if(c.eof)break;}
  await send('IO.close',{handle});
  const tr=JSON.parse(raw);const list=Array.isArray(tr)?tr:tr.traceEvents;

  // Raster busy intervals on the worker thread.
  const raster=[];
  for(const e of list){
    if(e.name==='RasterizerTaskImpl::RunOnWorkerThread' && e.dur>0) raster.push([e.ts, e.ts+e.dur]);
  }
  raster.sort((x,y)=>x[0]-y[0]);
  const busyAt=(t)=>{ // binary search
    let lo=0,hi=raster.length-1;
    while(lo<=hi){const m=(lo+hi)>>1;
      if(raster[m][1] < t) lo=m+1; else if(raster[m][0] > t) hi=m-1; else return true;}
    return false;
  };
  // Draws that actually damaged something (not EarlyOut_NoDamage) …
  const noDamage=new Set(list.filter(e=>e.name==='LayerTreeHostImpl::CalculateRenderPasses::EmptyDamageRect').map(e=>Math.round(e.ts)));
  let draws=0, drawsDuringRaster=0, damaging=0, damagingDuringRaster=0;
  for(const e of list){
    if(e.name!=='LayerTreeHostImpl::PrepareToDraw') continue;
    draws++;
    const inRaster=busyAt(e.ts);
    if(inRaster) drawsDuringRaster++;
    // A damaging draw near no EmptyDamageRect marker at the same ts.
    let empty=false;
    for(let d=-2;d<=2;d++) if(noDamage.has(Math.round(e.ts)+d)) empty=true;
    if(!empty){ damaging++; if(inRaster) damagingDuringRaster++; }
  }
  return {arm:a, rasterTasks:raster.length, draws, drawsDuringRaster, damaging, damagingDuringRaster};
}
const out=[];
for(let r=0;r<REPEATS;r++) for(const a of ARMS) out.push(await arm(a));
const agg={};
for(const r of out){const k=String(r.arm);const g=agg[k]??={arm:k,rasterTasks:0,draws:0,drawsDuringRaster:0,damaging:0,damagingDuringRaster:0};
  for(const f of ['rasterTasks','draws','drawsDuringRaster','damaging','damagingDuringRaster']) g[f]+=r[f]; agg[k]=g;}
for(const a of ARMS){const g=agg[String(a)];
  console.log(`${String(a).padEnd(8)} rasterTasks=${String(g.rasterTasks).padStart(5)} draws=${String(g.draws).padStart(5)} duringRaster=${String(g.drawsDuringRaster).padStart(5)} damagingDraws=${String(g.damaging).padStart(5)} damagingDuringRaster=${String(g.damagingDuringRaster).padStart(4)}`);}
ws.close();chrome.kill();
