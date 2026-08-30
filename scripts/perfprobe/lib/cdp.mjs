// Shared CDP connect for the probes. WSL cannot reach Windows loopback, so these run under
// Windows node.exe against the WebView2 debug port of a DEBUG=1 dev build.
import { readFileSync } from 'node:fs';
import { fail } from './format.mjs';
import { acquireProbeLease } from './lease.mjs';
import { methodAllowed, policyForProbe, probeNameFromArgv } from './policy.mjs';

export function normalizePort(value = process.env.AO_CDP_PORT || '9223') {
  const text = String(value).trim();
  if (!/^\d+$/.test(text)) throw new Error(`perfprobe: CDP port must be an integer, got ${JSON.stringify(value)}`);
  const port = Number(text);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`perfprobe: CDP port must be between 1 and 65535, got ${JSON.stringify(value)}`);
  }
  return String(port);
}

export const PORT = normalizePort();
export const BASE = `http://127.0.0.1:${PORT}`;
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const PROBE = probeNameFromArgv();
// Node's test runner imports this module from a test file rather than a probe
// entrypoint. Tests cover the policy helpers directly and never open CDP.
const POLICY = process.env.NODE_TEST_CONTEXT ? { kind: 'read', capabilities: new Set(['read', 'measure', 'page-observer']) } : policyForProbe(PROBE);
let cachedManifest;
let cachedManifestPath;

function originOf(rawUrl) {
  const url = new URL(rawUrl);
  if (url.protocol !== 'http:' || !['127.0.0.1', 'localhost'].includes(url.hostname)) {
    throw new Error(`perfprobe: refusing non-loopback page target ${url.origin}`);
  }
  return url.origin;
}

export function loadInstanceManifest() {
  const manifestPath = process.env.AO_PERFPROBE_MANIFEST || process.env.AO_PERFPROBE_INSTANCE_MANIFEST;
  if (!manifestPath) {
    throw new Error(
      'perfprobe: online probes require AO_PERFPROBE_MANIFEST from the run supervisor; refusing an unowned CDP endpoint',
    );
  }
  if (!cachedManifest || cachedManifestPath !== manifestPath) {
    let value;
    try {
      value = JSON.parse(readFileSync(manifestPath, 'utf8'));
    } catch (error) {
      throw new Error(`perfprobe: cannot read instance manifest ${manifestPath}: ${error.message}`);
    }
    if (!value || typeof value !== 'object') throw new Error('perfprobe: instance manifest must be an object');
    const instanceId = value.instanceId || value.instance?.id || value.id;
    const target = value.target || value.page || {};
    const targetId = value.targetId || value.cdp?.targetId || target.targetId || target.id;
    const pageMarker = value.pageMarker || value.cdp?.pageMarker || value.marker
      || target.pageMarker || target.targetMarker || target.marker;
    const origin = value.origin || value.cdp?.origin || target.origin || target.url || value.url;
    if (!instanceId) throw new Error('perfprobe: instance manifest has no instanceId');
    if (!origin) {
      throw new Error('perfprobe: instance manifest has no exact page origin');
    }
    if (!targetId) throw new Error('perfprobe: instance manifest has no exact targetId');
    if (typeof pageMarker !== 'string' || pageMarker.length === 0) {
      throw new Error('perfprobe: instance manifest has no page marker');
    }
    cachedManifest = {
      ...value,
      origin,
      instanceId,
      target: { ...target, targetId, pageMarker },
      manifestPath,
    };
    cachedManifestPath = manifestPath;
  }
  return cachedManifest;
}

export function validateManifestTarget(target, manifest = loadInstanceManifest()) {
  const expected = manifest.target || {};
  if (target.type !== 'page') throw new Error(`perfprobe: expected a page target, got ${target.type || '<unknown>'}`);
  if (target.id !== expected.targetId) {
    throw new Error(`perfprobe: CDP target ${target.id || '<unknown>'} is not the supervisor target ${expected.targetId}`);
  }
  const actualOrigin = originOf(target.url || '');
  const expectedOrigin = originOf(expected.origin || manifest.origin || expected.url || manifest.url || '');
  if (actualOrigin !== expectedOrigin) {
    throw new Error(`perfprobe: page origin ${actualOrigin} does not match manifest origin ${expectedOrigin}`);
  }
  if (expected.url && target.url !== expected.url) {
    throw new Error(`perfprobe: page URL changed from the manifest target (${expected.url} -> ${target.url})`);
  }
  if (expected.title && target.title !== expected.title) {
    throw new Error(`perfprobe: page title changed from the manifest target`);
  }
  const marker = expected.pageMarker;
  let actualMarker = '';
  try {
    actualMarker = new URL(target.url).searchParams.get('page') || '';
  } catch {
    throw new Error(`perfprobe: selected target has an invalid URL`);
  }
  if (actualMarker !== marker) {
    throw new Error(`perfprobe: page marker ${JSON.stringify(marker)} is absent from the selected target`);
  }
  if (!target.webSocketDebuggerUrl) throw new Error('perfprobe: selected page has no debugger URL');
  return target;
}

async function ownedPage() {
  const manifest = loadInstanceManifest();
  const list = await getJson('/json/list');
  const pages = list.filter((target) => target.type === 'page');
  const target = pages.find((candidate) => candidate.id === manifest.target.targetId);
  if (!target) {
    throw new Error(`perfprobe: supervisor target ${manifest.target.targetId} is not present on CDP port ${PORT}`);
  }
  return { manifest, target: validateManifestTarget(target, manifest) };
}

function leaseFor(manifest) {
  return acquireProbeLease(manifest, PROBE, POLICY.kind);
}

// Ceiling for one CDP round trip. Sized for the slowest call a probe makes (HeapProfiler.takeHeapSnapshot
// on a few-hundred-MB heap), not for the poll intervals, which are plain sleeps between calls.
const CALL_TIMEOUT_MS = +(process.env.AO_PROBE_CALL_TIMEOUT_MS || 120000);
const HTTP_TIMEOUT_MS = +(process.env.AO_PROBE_HTTP_TIMEOUT_MS || 10000);
const EVENT_HISTORY_MAX_BYTES = byteLimit('AO_PROBE_MAX_EVENT_BYTES', 16 << 20);
const STREAM_MAX_BYTES = byteLimit('AO_PROBE_MAX_STREAM_BYTES', 512 << 20);

function byteLimit(name, fallback) {
  const raw = process.env[name];
  if (raw === undefined || raw === '') return fallback;
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value <= 0) {
    throw new Error(`perfprobe: ${name} must be a positive safe integer, got ${JSON.stringify(raw)}`);
  }
  return value;
}

const NO_APP = `probe: nothing is listening on CDP port ${PORT}.\n`
  + `       Start the dev app with 'DEBUG=1 make dev-wsl' (port 9223), or the soak rig with 'make soak' (port 9224),\n`
  + `       or point AO_CDP_PORT at the right port.`;

async function getJson(path) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), HTTP_TIMEOUT_MS);
  let r;
  try {
    r = await fetch(BASE + path, { signal: controller.signal });
  } catch (error) {
    if (controller.signal.aborted) {
      throw new Error(`perfprobe: CDP ownership check timed out after ${HTTP_TIMEOUT_MS}ms`);
    }
    fail(`${NO_APP}${error?.message ? ` (${error.message})` : ''}`);
  } finally {
    clearTimeout(timer);
  }
  if (!r.ok) {
    throw new Error(`perfprobe: CDP ownership check returned HTTP ${r.status}`);
  }
  return r.json();
}

export async function connectPage() {
  const { manifest, target } = await ownedPage();
  return connect(target.webSocketDebuggerUrl, { manifest, target, targetType: 'page' });
}

export async function connectBrowser() {
  const { manifest } = await ownedPage();
  const ver = await getJson('/json/version');
  if (!ver.webSocketDebuggerUrl) fail(`probe: no browser target on CDP port ${PORT}.`);
  return connect(ver.webSocketDebuggerUrl, { manifest, targetType: 'browser' });
}

/** Connect an already selected target. Callers must validate its identity first. */
export async function connectTarget(webSocketDebuggerUrl) {
  const { manifest } = await ownedPage();
  const target = (await getJson('/json/list')).find((candidate) => candidate.webSocketDebuggerUrl === webSocketDebuggerUrl);
  if (!target) throw new Error('perfprobe: requested CDP target is no longer present');
  validateManifestTarget(target, manifest);
  return connect(webSocketDebuggerUrl, { manifest, target, targetType: 'page' });
}

function connect(url, identity) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    let id = 0;
    const pending = new Map();
    const events = [];
    let eventBytes = 0;
    const listeners = [];
    const eventWaiters = new Set();
    let releaseLease;
    // The browser going away (app restart, window closed) must fail every caller loudly. Without
    // this, in-flight sends sit in `pending` forever and a poll loop hangs alive and silent,
    // still "running" to a supervisor while producing nothing.
    let dead = null;
    ws.onclose = () => {
      if (releaseLease) { releaseLease(); releaseLease = undefined; }
      if (!dead) dead = new Error(`cdp: connection to ${url} closed by the browser (app restarted or window closed)`);
      for (const { rej } of pending.values()) rej(dead);
      pending.clear();
      for (const waiter of eventWaiters) {
        clearTimeout(waiter.timer);
        waiter.reject(dead);
      }
      eventWaiters.clear();
    };
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id && pending.has(m.id)) {
        const { res, rej } = pending.get(m.id);
        pending.delete(m.id);
        m.error ? rej(new Error(m.error.message)) : res(m.result);
      } else if (m.method) {
        const eventBytesForMessage = Buffer.byteLength(JSON.stringify(m), 'utf8');
        if (eventBytes + eventBytesForMessage > EVENT_HISTORY_MAX_BYTES) {
          dead = new Error(`cdp: protocol event history exceeded ${EVENT_HISTORY_MAX_BYTES} bytes`);
          ws.close();
          return;
        }
        eventBytes += eventBytesForMessage;
        events.push(m);
        for (const l of listeners) l(m);
        for (const waiter of eventWaiters) {
          if (waiter.method !== m.method) continue;
          let matches;
          try {
            matches = waiter.predicate(m);
          } catch (error) {
            clearTimeout(waiter.timer);
            eventWaiters.delete(waiter);
            waiter.reject(error);
            continue;
          }
          if (!matches) continue;
          clearTimeout(waiter.timer);
          eventWaiters.delete(waiter);
          waiter.resolve(m.params);
        }
      }
    };
    ws.onopen = () => {
      try {
        releaseLease = leaseFor(identity.manifest);
      } catch (error) {
        ws.close();
        reject(error);
        return;
      }
      resolve({
      send: async (method, params = {}) => {
        if (dead) throw dead;
        if (!methodAllowed(POLICY, method)) {
          throw new Error(`perfprobe: ${PROBE} policy refuses CDP instrument ${method}`);
        }
        // Revalidate the supervisor's target before every page or instrument
        // command. A target id can be reused after a window restart.
        if (identity.targetType === 'page') {
          const current = (await getJson('/json/list')).find((candidate) => candidate.id === identity.target.id);
          if (!current) throw new Error(`perfprobe: owned page target ${identity.target.id} disappeared`);
          validateManifestTarget(current, identity.manifest);
        } else {
          await ownedPage();
        }
        return new Promise((res, rej) => {
          if (dead) { rej(dead); return; }
          const mid = ++id;
          // A killed browser never sends a close frame, so onclose can stay silent while the socket
          // black-holes. Every call therefore carries its own deadline: without one, a poll loop
          // parks on a promise that can never settle and looks healthy to its supervisor forever.
          // Generous enough for the slowest probe call (a heap snapshot on a large heap).
          const timer = setTimeout(() => {
            if (!pending.has(mid)) return;
            pending.delete(mid);
            rej(new Error(`cdp: ${method} got no answer in ${Math.round(CALL_TIMEOUT_MS / 1000)}s (browser gone or wedged)`));
          }, CALL_TIMEOUT_MS);
          pending.set(mid, {
            res: (v) => { clearTimeout(timer); res(v); },
            rej: (e) => { clearTimeout(timer); rej(e); },
          });
          ws.send(JSON.stringify({ id: mid, method, params }));
        });
      },
      events,
      on: (fn) => listeners.push(fn),
      waitFor: (method, predicate = () => true) => new Promise((res, rej) => {
        if (dead) { rej(dead); return; }
        const waiter = {
          method,
          predicate,
          resolve: res,
          reject: rej,
          timer: undefined,
        };
        waiter.timer = setTimeout(() => {
          if (!eventWaiters.delete(waiter)) return;
          rej(new Error(`cdp: ${method} event did not arrive within ${Math.round(CALL_TIMEOUT_MS / 1000)}s`));
        }, CALL_TIMEOUT_MS);
        eventWaiters.add(waiter);
      }),
      close: () => {
        try { ws.close(); } finally {
          if (releaseLease) { releaseLease(); releaseLease = undefined; }
        }
      },
    });
    };
    ws.onerror = () => reject(new Error('ws connect failed'));
  });
}

export async function readStream(c, handle) {
  let data = '';
  let bytes = 0;
  let failure;
  try {
    for (;;) {
      const ch = await c.send('IO.read', { handle, size: 1 << 20 });
      const chunk = ch.base64Encoded ? Buffer.from(ch.data, 'base64').toString('utf8') : ch.data;
      const chunkBytes = Buffer.byteLength(chunk, 'utf8');
      if (bytes + chunkBytes > STREAM_MAX_BYTES) {
        throw new Error(`cdp: IO stream exceeded ${STREAM_MAX_BYTES} bytes`);
      }
      bytes += chunkBytes;
      data += chunk;
      if (ch.eof) break;
    }
  } catch (error) {
    failure = error;
  } finally {
    try {
      await c.send('IO.close', { handle });
    } catch (error) {
      // Preserve a stream-size or read failure. A close failure is still
      // visible when it is the only failure.
      if (!failure) failure = error;
    }
  }
  if (failure) throw failure;
  return data;
}

export async function evaluate(conn, expression) {
  const r = await conn.send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) {
    throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text || 'evaluate failed');
  }
  return r.result?.value;
}

// Node on Windows asserts inside libuv when process.exit lands while a WebSocket is still tearing
// down, so close every connection and give the loop a tick before leaving.
//
// NOT process.exit() directly: done() runs in probes' finally blocks, and a
// hard exit(0) there swallows any error propagating past the finally — the
// probe dies silently with rc=0 (bit twice on 2026-08-25: driveburn given
// invented flags reported nothing and "succeeded"). Setting exitCode and
// draining the loop lets a throw print its stack and exit nonzero; the
// unref'd timer only fires if a stray handle keeps the loop alive.
export async function done(conns = [], code = 0) {
  for (const c of conns) {
    try {
      c.close();
    } catch (error) {
      console.warn('probe: CDP connection cleanup failed:', error);
    }
  }
  await sleep(50);
  process.exitCode = code;
  setTimeout(() => process.exit(code), 2000).unref();
}
