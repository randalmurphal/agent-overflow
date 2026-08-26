// Repro: park mid-clip via real wheel, then click Stop; trace pane+clip offsets.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';
const page = await connectPage();
try {
  await page.send('Page.enable').catch(() => {});
  const read = async () => JSON.parse(await evaluate(page, `JSON.stringify((()=>{
    const pane=document.querySelectorAll('[data-pane-id]')[0];
    const c=pane.querySelector('[data-testid="activity-run-clip"]');
    const p=pane.querySelector('[data-testid="message-timeline-scroll"]');
    const chips=[...pane.querySelectorAll('button')].filter(b=>/Load (older|newer)|Jump to latest/.test(b.textContent)).map(b=>b.textContent.trim());
    return {clip:c?{top:Math.round(c.scrollTop),range:Math.round(c.scrollHeight-c.clientHeight)}:null,
            pane:p?{top:Math.round(p.scrollTop),range:Math.round(p.scrollHeight-p.clientHeight)}:null,chips};
  })())`));
  const rect = JSON.parse(await evaluate(page, `JSON.stringify((()=>{
    const c=document.querySelectorAll('[data-pane-id]')[0].querySelector('[data-testid="activity-run-clip"]');
    const r=c.getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2};})())`));
  console.log('t0 (following):', JSON.stringify(await read()));
  // Park mid-clip: one upward wheel gesture.
  await page.send('Input.synthesizeScrollGesture', { x: rect.x, y: rect.y, yDistance: 400, speed: 1500, gestureSourceType: 'mouse' });
  await sleep(600);
  console.log('t1 (parked):   ', JSON.stringify(await read()));
  // Arm per-frame collector.
  await evaluate(page, `(()=>{const pane=document.querySelectorAll('[data-pane-id]')[0];
    const c=pane.querySelector('[data-testid="activity-run-clip"]');
    const p=pane.querySelector('[data-testid="message-timeline-scroll"]');
    window.__snap=[];const t0=performance.now();
    const loop=()=>{window.__snap.push([Math.round(performance.now()-t0),Math.round(p.scrollTop),c&&c.isConnected?Math.round(c.scrollTop):-1]);
      if(performance.now()-t0<4000)requestAnimationFrame(loop)};requestAnimationFrame(loop);return 'armed'})()`);
  // Click Stop.
  const clicked = await evaluate(page, `(()=>{const pane=document.querySelectorAll('[data-pane-id]')[0];
    const b=[...pane.querySelectorAll('button')].find(b=>/nterrupt|stop/i.test(b.getAttribute('title')||b.getAttribute('aria-label')||''));
    if(!b)return 'NO STOP BUTTON';b.click();return 'clicked'})()`);
  console.log('stop:', clicked);
  await sleep(2500);
  console.log('t2 (post-stop):', JSON.stringify(await read()));
  const trace = JSON.parse(await evaluate(page, `JSON.stringify((()=>{const r=window.__snap||[];
    let jumps=[];for(let i=1;i<r.length;i++){const dp=r[i][1]-r[i-1][1],dc=r[i][2]-r[i-1][2];
      if(Math.abs(dp)>40||Math.abs(dc)>80)jumps.push({t:r[i][0],pane:[r[i-1][1],r[i][1]],clip:[r[i-1][2],r[i][2]]});}
    return {frames:r.length,jumps:jumps.slice(0,15)};})())`));
  console.log('trace:', JSON.stringify(trace));
} finally { await done([page]); }
