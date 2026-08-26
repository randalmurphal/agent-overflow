// Drive the user's repro: scroll pane 0 to top, click "Load older
// messages" repeatedly, and after each click record whether the
// conversation tail survived (a4c9bed3 verification).
import { connectPage, done, evaluate, sleep } from './lib/cdp.mjs';
const page = await connectPage();
const read = async () => JSON.parse(await evaluate(page, `JSON.stringify((()=>{
  const pane=document.querySelectorAll('[data-pane-id]')[0];
  const rows=[...pane.querySelectorAll('[data-testid="message-timeline-scroll"] [data-index]')];
  const chips=[...pane.querySelectorAll('button')].filter(b=>/Load (older|newer) messages|Jump to latest/.test(b.textContent)).map(b=>b.textContent.trim());
  const runHeader=[...pane.querySelectorAll('button,div')].find(el=>/\\d+ (Edit|Bash|Read|Write)/.test(el.textContent||'') && el.textContent.length<200);
  const p=pane.querySelector('[data-testid="message-timeline-scroll"]');
  return {chips, counts:runHeader?runHeader.textContent.trim().slice(0,90):null,
    lastText:(rows.at(-1)?.textContent||'').trim().slice(0,70),
    sh:Math.round(p.scrollHeight), top:Math.round(p.scrollTop)};})())`));
const clickOlder = () => evaluate(page, `(()=>{
  const pane=document.querySelectorAll('[data-pane-id]')[0];
  const b=[...pane.querySelectorAll('button')].find(x=>x.textContent.includes('Load older messages'));
  if(!b) return 'no-chip'; b.click(); return 'clicked';})()`);
try {
  // Park at top so the chip is visible/live.
  await evaluate(page, `(()=>{const p=document.querySelectorAll('[data-pane-id]')[0].querySelector('[data-testid="message-timeline-scroll"]');p.scrollTop=0;})()`);
  await sleep(300);
  console.log('t0:', JSON.stringify(await read()));
  for (let i = 1; i <= 4; i++) {
    const r = await clickOlder();
    await sleep(1200);
    console.log(`click${i} (${r}):`, JSON.stringify(await read()));
  }
} finally { await done([page]); }
