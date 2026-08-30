// Attribute ResizeObserver loop errors to the observers that ran immediately before them.
// usage: probe resizewatch [seconds=55]
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { connectPage, done, evaluate, PORT, sleep } from './lib/cdp.mjs';

if (PORT === '9223') {
  throw new Error('resizewatch refuses the read-only user app on CDP port 9223');
}

const seconds = +(process.argv[2] || 55);
if (!Number.isFinite(seconds) || seconds <= 0) {
  throw new Error('resizewatch duration must be a positive number of seconds');
}
const page = await connectPage();

const injected = `(() => {
  const prefix = '[ao-resizewatch]';
  const NativeResizeObserver = window.ResizeObserver;
  const state = {
    startedAt: performance.now(),
    nextId: 1,
    observers: {},
    recent: [],
    errorCount: 0,
    errors: [],
  };
  const describe = (entry) => ({
    element: {
      tag: entry.target.tagName,
      id: entry.target.id,
      classes: typeof entry.target.className === 'string' ? entry.target.className.slice(0, 180) : '',
      testId: entry.target.getAttribute('data-testid') || '',
      rowIndex: entry.target.getAttribute('data-row-index') || '',
      itemId: entry.target.getAttribute('data-item-id') || '',
    },
    width: entry.contentRect.width,
    height: entry.contentRect.height,
  });
  window.ResizeObserver = class ResizeObserver extends NativeResizeObserver {
    constructor(callback) {
      const id = state.nextId++;
      const stack = new Error(prefix + ' registration').stack || '';
      state.observers[id] = { id, stack, calls: 0, entries: 0, lastTargets: [] };
      super((entries, observer) => {
        const now = performance.now();
        const record = state.observers[id];
        record.calls += 1;
        record.entries += entries.length;
        record.lastTargets = entries.map(describe);
        state.recent.push({
          id,
          at: now,
          targets: record.lastTargets,
        });
        if (state.recent.length > 200) state.recent.splice(0, state.recent.length - 200);
        callback(entries, observer);
      });
    }
  };
  const onError = (event) => {
    if (!String(event.message).includes('ResizeObserver loop')) return;
    const now = performance.now();
    const recent = state.recent.filter((entry) => now - entry.at <= 20);
    state.errorCount += 1;
    if (state.errors.length < 100) state.errors.push({ at: now, recent });
    if (state.errorCount <= 3) console.error(prefix, 'loop', JSON.stringify(recent));
  };
  window.addEventListener('error', onError);
  window.__aoResizeWatch = {
    dump() {
      return JSON.stringify({
        durationMs: performance.now() - state.startedAt,
        observers: Object.values(state.observers),
        errorCount: state.errorCount,
        errors: state.errors,
      });
    },
  };
})();`;

let injectedScriptId;
let runError;
try {
  await page.send('Page.enable');
  const installed = await page.send('Page.addScriptToEvaluateOnNewDocument', { source: injected });
  injectedScriptId = installed.identifier;
  await page.send('Page.reload', { ignoreCache: true });
  let ready = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    if (await evaluate(page, `Boolean(window.__aoResizeWatch)`)) {
      ready = true;
      break;
    }
    await sleep(50);
  }
  if (!ready) throw new Error('resizewatch did not install after reload');
  console.log(`resizewatch installed for ${seconds}s`);
  await sleep(seconds * 1000);
  const raw = await evaluate(page, `window.__aoResizeWatch.dump()`);
  const data = JSON.parse(raw);
  const out = join(
    process.env.AO_PERFPROBE_OUT || process.env.TEMP || '.',
    `ao-resizewatch-${Date.now()}.json`,
  );
  writeFileSync(out, JSON.stringify(data, null, 1));
  console.log(`${data.errorCount} ResizeObserver loop errors`);
  for (const observer of data.observers.sort((a, b) => b.calls - a.calls)) {
    console.log(`observer ${observer.id}: ${observer.calls} calls, ${observer.entries} entries`);
    console.log(observer.stack.split('\n').slice(1, 5).join(' | '));
    console.log(JSON.stringify(observer.lastTargets));
  }
  console.log(`full JSON: ${out}`);
} catch (error) {
  runError = error;
}

const cleanupErrors = [];
if (injectedScriptId !== undefined) {
  try {
    await page.send('Page.removeScriptToEvaluateOnNewDocument', {
      identifier: injectedScriptId,
    });
  } catch (error) {
    cleanupErrors.push(error);
  }
  // Existing observers close over the diagnostic state. A clean reload is
  // the only way to remove that retained instrumentation from later memory
  // profiles without disconnecting observers the app still owns.
  try {
    await page.send('Page.reload', { ignoreCache: false });
    let restored = false;
    let lastRestoreEvaluationError;
    for (let attempt = 0; attempt < 200; attempt++) {
      try {
        restored = await evaluate(
          page,
          `document.readyState !== 'loading' && window.__aoResizeWatch === undefined`,
        );
        lastRestoreEvaluationError = undefined;
      } catch (error) {
        // The old execution context is destroyed between reload and the new
        // document becoming available. Retain the last failure so a page that
        // never recovers reports the actual CDP cause rather than only timing
        // out here.
        lastRestoreEvaluationError = error;
      }
      if (restored) break;
      await sleep(50);
    }
    if (!restored) {
      throw new Error(
        'resizewatch cleanup reload did not reach a clean document',
        lastRestoreEvaluationError === undefined
          ? undefined
          : { cause: lastRestoreEvaluationError },
      );
    }
  } catch (error) {
    cleanupErrors.push(error);
  }
}
try {
  await done([page]);
} catch (error) {
  cleanupErrors.push(error);
}
if (runError !== undefined && cleanupErrors.length > 0) {
  throw new AggregateError(
    [runError, ...cleanupErrors],
    'resizewatch failed and could not cleanly restore the page',
  );
}
if (runError !== undefined) throw runError;
if (cleanupErrors.length > 0) {
  throw new AggregateError(cleanupErrors, 'resizewatch could not cleanly restore the page');
}
