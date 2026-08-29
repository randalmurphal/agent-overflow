// Shared CDP connect for the probes. WSL cannot reach Windows loopback, so these run under
// Windows node.exe against the WebView2 debug port of a DEBUG=1 dev build.
import { fail } from './format.mjs';

export const PORT = process.env.AO_CDP_PORT || '9223';
export const BASE = `http://127.0.0.1:${PORT}`;
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Ceiling for one CDP round trip. Sized for the slowest call a probe makes (HeapProfiler.takeHeapSnapshot
// on a few-hundred-MB heap), not for the poll intervals, which are plain sleeps between calls.
const CALL_TIMEOUT_MS = +(process.env.AO_PROBE_CALL_TIMEOUT_MS || 120000);

const NO_APP = `probe: nothing is listening on CDP port ${PORT}.\n`
  + `       Start the dev app with 'DEBUG=1 make dev-wsl' (port 9223), or the soak rig with 'make soak' (port 9224),\n`
  + `       or point AO_CDP_PORT at the right port.`;

async function getJson(path) {
  let r;
  try {
    r = await fetch(BASE + path);
  } catch {
    fail(NO_APP);
  }
  return r.json();
}

export async function connectPage() {
  const list = await getJson('/json/list');
  const page = list.find((t) => t.type === 'page');
  if (!page) fail(`probe: no page target on CDP port ${PORT} (the window may still be starting up).`);
  return connect(page.webSocketDebuggerUrl);
}

export async function connectBrowser() {
  const ver = await getJson('/json/version');
  if (!ver.webSocketDebuggerUrl) fail(`probe: no browser target on CDP port ${PORT}.`);
  return connect(ver.webSocketDebuggerUrl);
}

function connect(url) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    let id = 0;
    const pending = new Map();
    const events = [];
    const listeners = [];
    const eventWaiters = new Set();
    // The browser going away (app restart, window closed) must fail every caller loudly. Without
    // this, in-flight sends sit in `pending` forever and a poll loop hangs alive and silent,
    // still "running" to a supervisor while producing nothing.
    let dead = null;
    ws.onclose = () => {
      dead = new Error(`cdp: connection to ${url} closed by the browser (app restarted or window closed)`);
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
    ws.onopen = () => resolve({
      send: (method, params = {}) => new Promise((res, rej) => {
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
      }),
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
      close: () => ws.close(),
    });
    ws.onerror = () => reject(new Error('ws connect failed'));
  });
}

export async function readStream(c, handle) {
  let data = '';
  for (;;) {
    const ch = await c.send('IO.read', { handle, size: 1 << 20 });
    data += ch.base64Encoded ? Buffer.from(ch.data, 'base64').toString('utf8') : ch.data;
    if (ch.eof) break;
  }
  await c.send('IO.close', { handle });
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
