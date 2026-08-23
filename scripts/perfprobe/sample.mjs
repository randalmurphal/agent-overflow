// Steady-state samples: per-process private footprint and key allocators, js heap, node counts.
// usage: probe sample [--every <sec>] [--for <sec>]
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { takeMemoryDump, allocatorMB, role } from './lib/memdump.mjs';

const args = process.argv.slice(2);
const arg = (name, def) => { const i = args.indexOf(name); return i >= 0 ? +args[i + 1] : def; };
const EVERY = arg('--every', 0);
const FOR = arg('--for', 0);

const b = await connectBrowser();
const p = await connectPage();
const proto = await p.send('Runtime.evaluate', { expression: 'Node.prototype' });

async function sample() {
  const out = { t: new Date().toISOString(), procs: {} };
  const { byPid } = await takeMemoryDump(b, 'light');
  for (const proc of Object.values(byPid)) {
    out.procs[proc.pid] = {
      role: role(proc),
      privMB: proc.privMB,
      blink_gc: allocatorMB(proc, 'blink_gc'),
      cc: allocatorMB(proc, 'cc'),
      v8: allocatorMB(proc, 'v8'),
      malloc: allocatorMB(proc, 'malloc'),
    };
  }
  const ev = await p.send('Runtime.evaluate', {
    returnByValue: true,
    expression: `({ heapMB: +(performance.memory.usedJSHeapSize / 1048576).toFixed(1), panes: document.querySelectorAll('.scroll-composited-content').length })`,
  });
  out.page = ev.result?.value;
  // queryObjects walks every live Node, which is the only way to see detached ones from outside.
  const objs = await p.send('Runtime.queryObjects', { prototypeObjectId: proto.result.objectId });
  const cnt = await p.send('Runtime.callFunctionOn', {
    objectId: objs.objects.objectId,
    returnByValue: true,
    functionDeclaration: 'function(){ let t=0,d=0; for (let i=0;i<this.length;i++){ t++; try { if(!this[i].isConnected) d++; } catch(e){} } return {total:t,detached:d}; }',
  });
  out.nodes = cnt.result?.value;
  await p.send('Runtime.releaseObject', { objectId: objs.objects.objectId }).catch(() => {});
  console.log(JSON.stringify(out));
}

if (EVERY > 0 && FOR > 0) {
  const t0 = Date.now();
  for (;;) {
    await sample();
    if ((Date.now() - t0) / 1000 >= FOR) break;
    await sleep(EVERY * 1000);
  }
} else {
  await sample();
}
await done([b, p]);
