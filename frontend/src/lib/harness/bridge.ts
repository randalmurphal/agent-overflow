// The frontend half of the harness ui-query protocol (§4 of
// docs/specs/testing-harness.md): one dispatch function over a tagged,
// versioned query union, plus the one piece of standing state a snapshot
// needs — when the document last changed.
//
// This module has NO WIRE SURFACE. It never subscribes, never calls an
// RPC, and never imports the transport. The wire edge is
// stores/harnessBridge.ts, for a structural reason: `architecture.test.ts`
// rule 2 bans `@wailsio/runtime` and `Events.On` outside `stores/`, and
// that rule is right — a subscription outside a store is a second copy of
// something with the wrong lifetime. Splitting here also means everything
// interesting is a pure-ish function over a Document, which is what makes
// it testable without a browser.
//
// Errors are DATA. `answerHarnessQuery` never rejects; a refusal comes
// back as `{error}`, which the backend unwraps into a Go error (see
// harnessUIResult in app_harness_ui.go). A rejected promise here would
// leave the backend waiter parked for its full 10s over a typo.

import { readHarnessGlobal } from './globals';
import {
  collectPerfSample,
  perfRunActive,
  startPerfRun,
  stopPerfRun,
  type PerfStartOptions,
} from './perf';
import {
  DEFAULT_ELEMENT_TEXT_CAP,
  DEFAULT_SETTLED_MS,
  readElement,
  readViewport,
} from './snapshot';

export interface HarnessQueryError {
  error: string;
}

// ---------------------------------------------------------------------------
// settled: the mutation clock.
//
// A one-shot query cannot WAIT for quiet without holding the backend
// waiter hostage, so it reports how long the document has been quiet and
// lets the caller poll. The observer is document-wide and harness-only;
// its callback does nothing but stamp a number, so the cost is the
// engine's own record-keeping rather than anything this code does.

let lastMutationAt = 0;
let observer: MutationObserver | null = null;

function now(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

/** Installs the mutation clock. Idempotent; returns a disposer. */
export function activateHarnessBridge(): () => void {
  lastMutationAt = now();
  if (observer || typeof MutationObserver === 'undefined' || typeof document === 'undefined') {
    return () => stopHarnessBridge();
  }
  observer = new MutationObserver(() => {
    lastMutationAt = now();
  });
  observer.observe(document.documentElement, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
  });
  return () => stopHarnessBridge();
}

export function stopHarnessBridge(): void {
  observer?.disconnect();
  observer = null;
  if (perfRunActive()) stopPerfRun();
}

/** Milliseconds since the last observed DOM mutation. */
export function sinceLastMutationMs(): number {
  return Math.max(0, now() - lastMutationAt);
}

// ---------------------------------------------------------------------------
// dispatch

function fail(message: string): HarnessQueryError {
  return { error: message };
}

function asRecord(value: unknown): Record<string, unknown> | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

function str(spec: Record<string, unknown>, key: string): string {
  const raw = spec[key];
  return typeof raw === 'string' ? raw : '';
}

function num(spec: Record<string, unknown>, key: string, fallback: number): number {
  const raw = spec[key];
  return typeof raw === 'number' && Number.isFinite(raw) ? raw : fallback;
}

async function dispatch(spec: Record<string, unknown>): Promise<unknown> {
  switch (str(spec, 'kind')) {
    case 'viewport':
      return readViewport(document, {
        settledMs: num(spec, 'settledMs', DEFAULT_SETTLED_MS),
        sinceMutationMs: sinceLastMutationMs(),
        textHead: num(spec, 'textHead', 0) || undefined,
      });
    case 'element': {
      const selector = str(spec, 'selector');
      if (!selector) return fail('element query requires a selector');
      try {
        return readElement(
          document,
          selector,
          num(spec, 'textCap', DEFAULT_ELEMENT_TEXT_CAP),
        );
      } catch (err) {
        // querySelectorAll throws SyntaxError on a malformed selector.
        // That is the caller's typo and must read as one, not as "no
        // such element" — the two get debugged very differently.
        return fail(`invalid selector ${JSON.stringify(selector)}: ${String(err)}`);
      }
    }
    case 'globals': {
      const name = str(spec, 'name');
      if (!name) return fail('globals query requires a name');
      const args = Array.isArray(spec.args) ? spec.args : [];
      return await readHarnessGlobal(window, name, args);
    }
    case 'perf':
      return dispatchPerf(spec);
    case 'reload':
      return dispatchReload(num(spec, 'delayMs', DEFAULT_RELOAD_DELAY_MS));
    case '':
      return fail('query spec has no kind');
    default:
      return fail(`unknown query kind ${JSON.stringify(str(spec, 'kind'))}`);
  }
}

// ---------------------------------------------------------------------------
// reload
//
// `HarnessReset` wipes app state behind the page's back and its contract ends
// with "reload the page after". A Playwright spec does that with
// `page.reload()`; a shell tool has no browser driver, so without this the CLI
// could reset an instance but never make the attached window agree — every
// bench and every scripted flow would be reading a stale registry.
//
// The reload is DEFERRED rather than immediate because the reply to this very
// query is an RPC on the socket the reload is about to tear down. Answering
// first and navigating a beat later means the caller learns the reload was
// accepted; a caller that gets a timeout instead has still had its page
// reloaded, which is why `ao-harness` treats a failed reload query as
// non-fatal and re-probes the bridge rather than aborting.

const DEFAULT_RELOAD_DELAY_MS = 50;

function dispatchReload(delayMs: number): unknown {
  if (typeof window === 'undefined' || typeof window.location?.reload !== 'function') {
    return fail('reload is not available in this environment');
  }
  const wait = Math.min(Math.max(delayMs, 0), 5000);
  setTimeout(() => window.location.reload(), wait);
  return { v: 1, reloading: true, delayMs: wait };
}

function dispatchPerf(spec: Record<string, unknown>): unknown {
  switch (str(spec, 'op')) {
    case 'start': {
      const options: PerfStartOptions = {
        longFrameMs: num(spec, 'longFrameMs', 0) || undefined,
        meters: Array.isArray(spec.meters)
          ? spec.meters.filter((m): m is string => typeof m === 'string')
          : undefined,
      };
      const superseded = startPerfRun(options);
      // A superseded run's summary rides back rather than vanishing: a
      // double-start is a caller bug, and silently discarding the numbers
      // it already collected makes it an invisible one.
      return { v: 1, armed: true, superseded };
    }
    case 'collect':
      if (!perfRunActive()) return fail('no perf run is armed');
      return collectPerfSample();
    case 'stop': {
      const summary = stopPerfRun();
      if (!summary) return fail('no perf run is armed');
      return summary;
    }
    case 'status':
      return { v: 1, armed: perfRunActive() };
    default:
      return fail(`unknown perf op ${JSON.stringify(str(spec, 'op'))}`);
  }
}

/**
 * Answers one query spec. Resolves to the answer or to `{error}`; never
 * rejects. `v` is validated because the union is versioned and a client
 * from a future wave must be told so rather than silently mis-read.
 */
export async function answerHarnessQuery(rawSpec: unknown): Promise<unknown> {
  const spec = asRecord(rawSpec);
  if (!spec) return fail('query spec must be a JSON object');
  const version = num(spec, 'v', 1);
  if (version !== 1) return fail(`unsupported query version ${version} (this bridge speaks v1)`);
  try {
    return await dispatch(spec);
  } catch (err) {
    return fail(err instanceof Error ? err.message : String(err));
  }
}
