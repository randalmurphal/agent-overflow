import { connectBrowser, done } from './lib/cdp.mjs';
const b = await connectBrowser();
const { processInfo } = await b.send('SystemInfo.getProcessInfo');
for (const p of processInfo) console.log(`${p.type}  pid=${p.id}`);
await done([b]);
