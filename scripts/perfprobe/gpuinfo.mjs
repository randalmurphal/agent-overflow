// Read-only: GPU adapter, ANGLE backend and feature status via CDP SystemInfo.
// Answers "whose driver is the untracked GPU-process memory" and whether raster
// is GPU or software. usage: probe gpuinfo
import { connectBrowser, done } from './lib/cdp.mjs';

const b = await connectBrowser();
const info = await b.send('SystemInfo.getInfo');
const gpu = info.gpu || {};
for (const d of gpu.devices || []) {
  console.log(`device: ${d.vendorString || d.vendorId} ${d.deviceString || d.deviceId} driver=${d.driverVersion || '?'}`);
}
const aux = gpu.auxAttributes || {};
const keep = ['glRenderer', 'glVendor', 'glVersion', 'isSoftwareRendering', 'passthroughCmdDecoder', 'inProcessGpu'];
for (const k of keep) if (aux[k] !== undefined) console.log(`${k}: ${aux[k]}`);
const fs = gpu.featureStatus || {};
for (const [k, v] of Object.entries(fs)) console.log(`feature ${k}: ${v}`);
if (info.modelName) console.log(`model: ${info.modelName}`);
if (info.commandLine) console.log(`commandLine: ${info.commandLine}`);
await done([b]);
