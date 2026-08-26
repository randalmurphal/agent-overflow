// Verify b08be13b: wheel jiggle (the old auto-page killer) must not break
// follow or spawn paging chips while a giant run streams.
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';
const page = await connectPage();
const read = async () => JSON.parse(await evaluate(page, `JSON.stringify((()=>{
  const pane=document.querySelectorAll('[data-pane-id]')[0];
  const c=pane.querySelector('[data-testid="activity-run-clip"]');
  const p=pane.querySelector('[data-testid="message-timeline-scroll"]');
  const chips=[...pane.querySelectorAll('button')].filter(b=>/Load (older|newer)|Jump to latest/.test(b.textContent)).map(b=>b.textContent.trim());
  return {clipTop:Math.round(c.scrollTop),clipSh:Math.round(c.scrollHeight),clipAtBottom:c.scrollHeight-c.scrollTop-c.clientHeight<2,
          paneTop:Math.round(p.scrollTop),paneSh:Math.round(p.scrollHeight),chips};})())`));
try {
  const rect = JSON.parse(await evaluate(page, `JSON.stringify((()=>{
    const c=document.querySelectorAll('[data-pane-id]')[0].querySelector('[data-testid="activity-run-clip"]');
    const r=c.getBoundingClientRect();return {x:r.x+r.width/2,y:r.y+r.height/2};})())`));
  console.log('t0:', JSON.stringify(await read()));
  // The old killer: wheel up a bit (arms gates, emits scroll events), then back down to bottom.
  await page.send('Input.synthesizeScrollGesture', { x: rect.x, y: rect.y, yDistance: 600, speed: 1500, gestureSourceType: 'mouse' });
  await sleep(400);
  console.log('t1 (wheeled up):', JSON.stringify(await read()));
  await page.send('Input.synthesizeScrollGesture', { x: rect.x, y: rect.y, yDistance: -4000, speed: 3000, gestureSourceType: 'mouse' });
  await sleep(400);
  console.log('t2 (rode down):', JSON.stringify(await read()));
  for (let i = 0; i < 4; i++) {
    await sleep(5000);
    console.log(`t${3+i} (+${(i+1)*5}s):`, JSON.stringify(await read()));
  }
} finally { await done([page]); }
