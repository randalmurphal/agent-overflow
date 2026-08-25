// Read-only: list CDP targets on the port (type, title, url, ws id) to spot stale page targets.
import { BASE } from './lib/cdp.mjs';

const list = await (await fetch(`${BASE}/json/list`)).json();
for (const t of list) {
  console.log(`${t.type}  ${t.id}  ${JSON.stringify(t.title)}  ${t.url}`);
}
