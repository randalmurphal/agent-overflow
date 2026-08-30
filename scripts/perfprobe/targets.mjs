// Read-only: list CDP targets on the port (type, title, url, ws id) to spot stale page targets.
import { BASE, loadInstanceManifest, validateManifestTarget } from './lib/cdp.mjs';

const manifest = loadInstanceManifest();
const list = await (await fetch(`${BASE}/json/list`)).json();
const page = list.find((target) => target.id === manifest.target.targetId);
if (!page) throw new Error(`perfprobe: supervisor target ${manifest.target.targetId} is not present on CDP port ${manifest.origin}`);
validateManifestTarget(page, manifest);
for (const t of list) {
  console.log(`${t.type}  ${t.id}  ${JSON.stringify(t.title)}  ${t.url}`);
}
