// Do Blink edit commands accumulate while nobody types?
// Two censuses separated by a wait, plus a DOM inventory of every editable surface.
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, isRenderer } from './lib/memdump.mjs';

const WAIT = +(process.argv[2] || 180);
const EDIT = /(?:^|::)(\w*Command)$/;

const browser = await connectBrowser();
const page = await connectPage();
await page.send('Runtime.enable');
await page.send('HeapProfiler.enable');

const ev = async (e) => (await page.send('Runtime.evaluate', { expression: e, returnByValue: true, awaitPromise: true })).result.value;

async function census() {
  await page.send('HeapProfiler.collectGarbage'); await sleep(900);
  await page.send('HeapProfiler.collectGarbage'); await sleep(900);
  const dump = await takeMemoryDump(browser);
  const p = Object.values(dump.byPid).filter(isRenderer)
    .sort((a, b) => (allocatorMB(b, 'blink_gc') || 0) - (allocatorMB(a, 'blink_gc') || 0))[0];
  const types = new Map(); const hostPages = new Set(); let committed = 0;
  const pageCommitted = new Map();
  for (const [n, a] of Object.entries(p.allocators)) {
    let m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/(page_\d+)$/);
    if (m) { pageCommitted.set(`${m[1]}/${m[2]}`, a.size ?? a.effectiveSize ?? 0); continue; }
    m = n.match(/^blink_gc\/main\/heap\/([A-Za-z0-9]+)\/pages\/(page_\d+)\/types\/(?:blink::)?([\w:]+?)(?: \(0x[0-9a-f]+\))?$/);
    if (m && EDIT.test(m[3])) {
      types.set(m[3], (types.get(m[3]) || 0) + (a.objectCount ?? 0));
      hostPages.add(`${m[1]}/${m[2]}`);
    }
  }
  for (const k of hostPages) committed += pageCommitted.get(k) || 0;
  return { types, pages: hostPages.size, committedMB: committed / 1048576, blink: allocatorMB(p, 'blink_gc') };
}

const inventory = await ev(`JSON.stringify((() => {
  const q = (s) => [...document.querySelectorAll(s)];
  const eds = [...q('textarea'), ...q('input'), ...q('[contenteditable]:not([contenteditable="false"])')];
  return {
    textareas: q('textarea').length,
    inputs: q('input').length,
    contentEditable: q('[contenteditable]:not([contenteditable=\\'false\\')]').length,
    xtermTextareas: q('.xterm textarea, .xterm-helper-textarea').length,
    terminals: q('.xterm').length,
    editableChars: eds.reduce((s, e) => s + ((e.value ?? e.textContent ?? '').length), 0),
    focused: document.activeElement ? (document.activeElement.tagName + (document.activeElement.getAttribute('aria-label') ? '[' + document.activeElement.getAttribute('aria-label') + ']' : '')) : null,
  };
})())`).catch(() => null);

console.log('DOM inventory:', inventory);
const a = await census();
console.log(`\nT+0        ${String(a.pages).padStart(4)} pages  ${a.committedMB.toFixed(1)}MB  blink_gc=${a.blink}MB`);
for (const [t, c] of [...a.types].sort((x, y) => y[1] - x[1]).slice(0, 6)) console.log(`             ${String(c).padStart(6)}  ${t}`);
console.log(`\nwaiting ${WAIT}s (do not type into the app)...`);
await sleep(WAIT * 1000);
const b = await census();
console.log(`\nT+${WAIT}s      ${String(b.pages).padStart(4)} pages  ${b.committedMB.toFixed(1)}MB  blink_gc=${b.blink}MB`);
console.log(`\n== delta over ${WAIT}s ==`);
const keys = new Set([...a.types.keys(), ...b.types.keys()]);
for (const t of [...keys].sort()) {
  const d = (b.types.get(t) || 0) - (a.types.get(t) || 0);
  if (d !== 0) console.log(`  ${d > 0 ? '+' : ''}${d}  ${t}`);
}
console.log(`  pages ${b.pages - a.pages >= 0 ? '+' : ''}${b.pages - a.pages}   committed ${(b.committedMB - a.committedMB >= 0 ? '+' : '')}${(b.committedMB - a.committedMB).toFixed(2)}MB`);
const inv2 = await ev(`document.querySelectorAll('textarea,input').length`).catch(() => null);
console.log(`  editable elements now: ${inv2}`);
await done([browser, page]);
