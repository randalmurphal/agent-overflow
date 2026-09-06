import { isNativeShell } from './platform';
import { networkPlugin } from './networkPlugin';
import { pinnedFetch } from './networkHttp';
import type { ComputerRoute } from '../transport/computerRoute';

let support: Promise<boolean> | undefined;

// Leave half of the native bridge's 16 HTTP slots available for app traffic.
// A flapping set of computers cannot allocate an unbounded admission queue.
let active = 0;
const waiting = new Set<() => void>();
function admit(signal: AbortSignal): Promise<() => void> {
  signal.throwIfAborted();
  if (active >= 8 && waiting.size >= 32) return Promise.reject(new Error('Computer connection checks are busy. Retry.'));
  return new Promise((resolve, reject) => {
    const aborted = () => { waiting.delete(start); reject(signal.reason); };
    const start = () => {
      waiting.delete(start);
      signal.removeEventListener('abort', aborted);
      active++;
      resolve(() => {
        active--;
        waiting.values().next().value?.();
      });
    };
    if (active < 8) start();
    else { waiting.add(start); signal.addEventListener('abort', aborted, { once: true }); }
  });
}

/** Older APKs retain their existing Tailscale connection. The downloaded SPA
 * must negotiate the native capability before using its WebPKI health path. */
export function canVerifyComputerRoutes(): Promise<boolean> {
  if (!isNativeShell()) return Promise.resolve(false);
  return support ??= networkPlugin().then(async (plugin) => (await plugin.getCapabilities?.())?.computerRoutes === true).catch(() => false);
}

/** Native TLS bypasses browser CORS, never certificate or identity checks.
 * No credential, device proof, page cookie or redirect crosses this probe. */
export async function verifyComputerRoute(route: ComputerRoute, backendId: string, signal: AbortSignal): Promise<void> {
  if (!backendId) throw new Error('Computer identity is unavailable.');
  const release = await admit(signal);
  try { await probe(route, backendId, signal); }
  finally { release(); }
}

async function probe(route: ComputerRoute, backendId: string, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted();
  const response = await pinnedFetch(`${route.endpoint}/healthz`, { signal, credentials: 'omit', redirect: 'error' }, route.certFingerprint || '');
  const reader = response.body?.getReader();
  if (!reader) throw new Error('Computer health response is empty.');
  try {
    if (response.status !== 200) throw new Error(`Computer health answered HTTP ${response.status}.`);
    const decoder = new TextDecoder();
    const chunks: string[] = [];
    let size = 0;
    while (true) {
      signal.throwIfAborted();
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 64 * 1024) throw new Error('Computer health response is too large.');
      chunks.push(decoder.decode(value, { stream: true }));
    }
    chunks.push(decoder.decode());
    const health = JSON.parse(chunks.join('')) as { backendId?: unknown };
    if (health?.backendId !== backendId) throw new Error('This address belongs to a different computer.');
  } finally { await reader.cancel().catch(() => undefined); }
}
