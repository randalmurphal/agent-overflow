// The frontend half of the harness ui-query protocol (§4 of
// docs/specs/testing-harness.md): one dispatch function over a tagged,
// versioned query union. Lifecycle state for the mutation clock and teardown
// receipt lives in bridgeLifecycle.ts.
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
// leave the backend waiter parked for its full 10s over a typo. The one
// answer that is not an answer is HARNESS_NO_REPLY — see its doc comment.
//
// The query dispatcher delegates settledness tracking to bridgeLifecycle. That
// module owns the lazy MutationObserver and teardown receipt because both
// outlive an individual query and must be reset when the page is replaced.

import { readHarnessGlobal } from './globals';
import { armMutationClock, disarmMutationClock, sinceLastMutationMs } from './bridgeLifecycle';
import { dispatchMonitor } from './monitorBridge';
import { validateQueryShape, type HarnessQueryError } from './queryValidation';
import {
  collectPerfSample,
  perfMeterNames,
  perfRunActive,
  perfRunAddressed,
  perfRunId,
  perfSelfDisarmMessage,
  startPerfRun,
  stopPerfRun,
  unknownPerfMeters,
  type PerfStartOptions,
} from './perf';
import {
  DEFAULT_ELEMENT_TEXT_CAP,
  DEFAULT_SETTLED_MS,
  readElement,
  readViewport,
} from './snapshot';

export type { HarnessQueryError } from './queryValidation';
export {
  activateHarnessBridge,
  HARNESS_TEARDOWN_RECEIPT_STORAGE_KEY,
  lastHarnessBridgeTeardownReceipt,
  MUTATION_CLOCK_LINGER_MS,
  mutationClockArmed,
  sinceLastMutationMs,
  stopHarnessBridge,
} from './bridgeLifecycle';
export type { HarnessBridgeTeardownReceipt } from './bridgeLifecycle';

/**
 * The one answer that is NOT sent. The store routes each query only to the
 * registered page ID. Keeping this sentinel also protects the perf machine
 * from answering a query addressed to another page during a stale event.
 */
export const HARNESS_NO_REPLY = Symbol('harness:no-reply');

/** Whether `answerHarnessQuery` declined to answer (see HARNESS_NO_REPLY). */
export function isHarnessNoReply(value: unknown): boolean {
  return value === HARNESS_NO_REPLY;
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
      // The one query kind that reports settledness, and therefore the one
      // that pays for the observer. `element`, `globals`, `perf`, `reload`
      // and `open` answer without it and must not install it.
      armMutationClock();
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
          {
            textCap: num(spec, 'textCap', DEFAULT_ELEMENT_TEXT_CAP),
            includeScroll: spec.includeScroll === true,
          },
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
    case 'monitor':
      return dispatchMonitor(spec);
    case 'reload':
      return dispatchReload(num(spec, 'delayMs', DEFAULT_RELOAD_DELAY_MS));
    case 'open':
      return dispatchOpen(str(spec, 'threadId'), spec.newPane === true);
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

// ---------------------------------------------------------------------------
// open
//
// The out-of-page spelling of "open this thread in a NEW pane". The plain
// open already has one — `notification:activated`, the channel an OS
// notification click rides, which the SPA turns into `openThreadInPane` —
// and that path is deliberately left alone (cmd/ao-harness/bench_drive.go
// #activateThread): it exercises a production door end to end, and a bench
// built on it measures what a notification click costs.
//
// There is no such channel for the new-pane door. `openThreadInNewPane` is
// reached only by ctrl-clicking a sidebar row, the thread context menu, and
// a builtin command — all in-page gestures a shell driver cannot make. So
// this kind calls the SAME production function those three call, rather
// than reimplementing pane minting harness-side, which would measure a pane
// nobody ships.
//
// The stores are reached by DYNAMIC import, not a static one. The bridge is
// itself a lazily-imported chunk, so this costs nothing at run time (panes
// and threads are already in the startup graph) — but a static import here
// would drag the whole pane/thread store graph into every unit test that
// merely asks this module about a malformed spec.
async function dispatchOpen(threadId: string, newPane: boolean): Promise<unknown> {
  if (!threadId) return fail('open query requires a threadId');
  const [panes, threads] = await Promise.all([
    import('../stores/panes.svelte'),
    import('../stores/threads.svelte'),
  ]);
  const thread = threads.getThreadById(threadId);
  if (!thread) {
    // The registry, not the store: a thread the backend has but the page
    // has not listed yet is a real state and reads very differently from a
    // typo'd id. Naming the page is what tells the two apart.
    return fail(`this page's thread registry has no thread ${JSON.stringify(threadId)}`);
  }
  const pane = newPane
    ? await panes.openThreadInNewPane(thread)
    : await panes.openThreadInPane(thread);
  return { v: 1, opened: true, threadId, paneId: pane.paneId, newPane };
}

function dispatchPerf(spec: Record<string, unknown>): unknown {
  switch (str(spec, 'op')) {
    case 'disarm':
      // Setup viewport barriers may have armed the mutation clock. Clean
      // legs explicitly clear that observer before their measured action.
      disarmMutationClock();
      return { v: 1, armed: false };
    case 'start': {
      const hasMeters = spec.meters !== undefined && spec.meters !== null;
      if (hasMeters && !Array.isArray(spec.meters)) {
        return fail('perf meters must be an array of names when supplied');
      }
      const requested = hasMeters
        ? (spec.meters as unknown[]).filter((m): m is string => typeof m === 'string')
        : [];
      if (hasMeters && requested.length !== (spec.meters as unknown[]).length) {
        return fail('perf meters must contain only strings');
      }
      // An unknown name would filter to a NARROWER set — at the limit, an
      // empty one — and a run that armed nothing still answers
      // `{armed:true}` and then reports zeros forever. Same reasoning as
      // the globals whitelist: a typo must read as a typo.
      const unknown = unknownPerfMeters(requested);
      if (unknown.length > 0) {
        return fail(
          `unknown perf meter${unknown.length > 1 ? 's' : ''} ${unknown
            .map((name) => JSON.stringify(name))
            .join(', ')} (allowed: ${perfMeterNames().join(', ')})`,
        );
      }
      // A run starts clean. Bench setup opens a thread by POLLING
      // `viewport` (cmd/ao-harness/bench_drive.go#waitForActiveThread) and
      // then arms immediately, so without this the first seconds of every
      // measured workload would carry an observer the linger had not
      // expired yet. A caller that wants settledness DURING the run just
      // asks, and re-arms it.
      disarmMutationClock();
      // Budgets are cleaned page-side (normalizeBudgetsMs), so a caller that
      // sends them unsorted or with a stray zero gets a report rather than a
      // refusal — unlike a meter NAME, a bad budget cannot silently narrow
      // the run to nothing.
      const budgetsMs = Array.isArray(spec.budgetsMs)
        ? spec.budgetsMs.filter((value): value is number => typeof value === 'number')
        : [];
      const options: PerfStartOptions = {
        longFrameMs: num(spec, 'longFrameMs', 0) || undefined,
        // Omitted means the default all-meter set. An explicit [] is a
        // valid zero-meter run and must remain distinguishable from that
        // default through the bridge.
        meters: hasMeters ? requested : undefined,
        budgetsMs: budgetsMs.length > 0 ? budgetsMs : undefined,
        runId: str(spec, 'runId'),
      };
      const superseded = startPerfRun(options);
      // A superseded run's summary rides back rather than vanishing: a
      // double-start is a caller bug, and silently discarding the numbers
      // it already collected makes it an invisible one.
      // `runId` rides back so the caller can tell WHICH page armed, and so
      // a backend that stamps ids can correlate the collects that follow.
      return { v: 1, armed: true, runId: perfRunId(), superseded };
    }
    case 'collect': {
      const refusal = perfRunRefusal(spec);
      if (refusal !== null) return refusal;
      return collectPerfSample();
    }
    case 'stop': {
      const refusal = perfRunRefusal(spec);
      if (refusal !== null) return refusal;
      // The end of a run is the end of anything that could still want the
      // clock, and a run's own report is the number most damaged by an
      // observer nobody asked for. Don't wait out the linger.
      disarmMutationClock();
      // Non-null by the check above: `perfRunRefusal` already established a
      // run is armed on this page.
      return stopPerfRun();
    }
    case 'status':
      return { v: 1, armed: perfRunActive(), runId: perfRunId() };
    default:
      return fail(`unknown perf op ${JSON.stringify(str(spec, 'op'))}`);
  }
}

/**
 * What a `collect`/`stop` gets instead of an answer, or null when this page
 * should go ahead and answer it. Three outcomes, in order:
 *
 *   - the spec names another page's run → HARNESS_NO_REPLY (say nothing);
 *   - the watchdog disarmed this page's run → the reason it did;
 *   - nothing was ever armed here → the plain refusal.
 */
function perfRunRefusal(
  spec: Record<string, unknown>,
): HarnessQueryError | typeof HARNESS_NO_REPLY | null {
  if (!perfRunAddressed(str(spec, 'runId'))) return HARNESS_NO_REPLY;
  const selfDisarmed = perfSelfDisarmMessage();
  if (selfDisarmed !== null && !perfRunActive()) return fail(selfDisarmed);
  if (!perfRunActive()) return fail('no perf run is armed');
  return null;
}

/**
 * Answers one query spec. Resolves to the answer, to `{error}`, or to
 * `HARNESS_NO_REPLY` when the query is addressed to a different page;
 * never rejects. `v` is validated because the union is versioned and a
 * client from a future wave must be told so rather than silently mis-read.
 */
export async function answerHarnessQuery(rawSpec: unknown): Promise<unknown> {
  const spec = asRecord(rawSpec);
  if (!spec) return fail('query spec must be a JSON object');
  const shapeError = validateQueryShape(spec);
  if (shapeError) return shapeError;
  try {
    return await dispatch(spec);
  } catch (err) {
    return fail(err instanceof Error ? err.message : String(err));
  }
}
