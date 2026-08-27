// Does a memory-pressure broadcast make the GPU process return its pools?
// Fires Memory.simulatePressureNotification on the BROWSER target (the earlier
// activetrim run fired it on the page target, which reaches the renderer only)
// and tracks the GPU process's private bytes before/after via powershell.
// usage: probe gpupressure [moderate|critical]   (default critical)
import { execFileSync } from 'node:child_process';
import { connectBrowser, connectPage, done, sleep } from './lib/cdp.mjs';
import { fail } from './lib/format.mjs';

const level = process.argv.includes('moderate') ? 'moderate' : 'critical';
const b = await connectBrowser();
const { processInfo } = await b.send('SystemInfo.getProcessInfo');
const gpu = (processInfo || []).find((p) => p.type === 'GPU');
if (!gpu) fail(`probe: no GPU row in getProcessInfo (types: ${(processInfo || []).map((p) => p.type).join(', ')})`);
const mb = () => {
  const out = execFileSync('powershell.exe', ['-NoProfile', '-Command',
    `(Get-Process -Id ${gpu.id}).PrivateMemorySize64`], { encoding: 'utf8', timeout: 15000 });
  return Number(out.trim()) / 1048576;
};
console.log(`gpu pid ${gpu.id}  level=${level}`);
console.log(`before: ${mb().toFixed(1)}MB`);
let browserOk = true;
try {
  await b.send('Memory.simulatePressureNotification', { level });
  console.log('fired on browser target');
} catch (e) {
  browserOk = false;
  console.log(`browser target refused: ${e.message.split('\n')[0]}`);
}
if (!browserOk) {
  const c = await connectPage();
  await c.send('Memory.simulatePressureNotification', { level });
  console.log('fired on page target instead');
  await done([c]);
}
for (const s of [3, 10, 30]) {
  await sleep(s === 3 ? 3000 : s === 10 ? 7000 : 20000);
  console.log(`after ${s}s: ${mb().toFixed(1)}MB`);
}
await done([b]);
