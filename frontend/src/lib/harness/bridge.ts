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
// leave the backend waiter parked for its full 10s over a typo. The one
// answer that is not an answer is HARNESS_NO_REPLY — see its doc comment.
//
// ARMING IS LAZY AND THE OBSERVER'S LIFETIME MATCHES ITS NEED. The mutation
// observer below is installed by a query that ASKS about settledness, not at
// page load and not by the bridge chunk loading: it is document-wide over
// childList, characterData and attributes, so it allocates a MutationRecord
// per text delta, and the soak rig streams for hours precisely to reproduce
// renderer memory and hang behaviour. An observer that is always on is a
// probe that perturbs the experiment it was built to run — and a perf run or
// a bench workload is exactly the experiment, so a run nobody asked
// `settled` about must measure a renderer with no observer on it at all.
// The clock therefore disarms again once nothing needs it (a short linger
// past the last such query, and immediately when a perf run stops), and
// re-arms transparently on the next one. The cost is that a fresh arm has no
// history and reports `settled:false` — the clock starts with the observer,
// so nothing can claim quiet it did not witness.

import { readHarnessGlobal } from './globals';
import {
  clearPerfSelfDisarm,
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

export interface HarnessQueryError {
  error: string;
}

/**
 * The one answer that is NOT sent. Every ui-query event reaches EVERY
 * attached page and the backend takes the first reply, so a page that can
 * see the query is not addressed to it must stay silent rather than answer
 * a refusal that would win the race against the page that can answer
 * properly. Silence is safe: the backend's waiter times out on its own if
 * no page answers, which is the same outcome as no page being attached.
 */
export const HARNESS_NO_REPLY = Symbol('harness:no-reply');

/** Whether `answerHarnessQuery` declined to answer (see HARNESS_NO_REPLY). */
export function isHarnessNoReply(value: unknown): boolean {
  return value === HARNESS_NO_REPLY;
}

// ---------------------------------------------------------------------------
// settled: the mutation clock.
//
// A one-shot query cannot WAIT for quiet without holding the backend
// waiter hostage, so it reports how long the document has been quiet and
// lets the caller poll. The observer is document-wide and harness-only;
// its callback does nothing but stamp a number, so the cost is the
// engine's own record-keeping rather than anything this code does — which
// is exactly why its lifetime is scoped to the queries that need it (see
// the header): the engine's record-keeping is not free in a rig that is
// measuring the engine.
//
// The clock therefore starts at each fresh ARM, not at page load and not
// at chunk load. That query answers `settled:false` because the bridge has
// only just begun watching, and that is the honest answer: quiet nobody
// observed is not evidence of quiet. A caller polling for settle gets it on
// the next query, which is how every settle wait in the CLI and the e2e
// suite already works — and the linger below is what keeps a poll loop from
// re-arming (and so re-zeroing) the clock on every lap.

let lastMutationAt = 0;
let observer: MutationObserver | null = null;
let lingerTimer: ReturnType<typeof setTimeout> | null = null;

/**
 * How long the mutation clock stays armed after the last query that wanted
 * it. Long enough that a settle POLL — `expect.poll` in the e2e suite, a
 * human running `ao-harness ui` twice — keeps one continuous history rather
 * than restarting the clock every lap; short enough that a perf run or a
 * bench workload started after a stray `ui` snapshot is not measuring a
 * renderer carrying an observer production does not have.
 */
export const MUTATION_CLOCK_LINGER_MS = 5_000;

function now(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

/**
 * Arms the mutation clock for a query that reports settledness, and pushes
 * the linger out. Idempotent while armed: re-arming inside the linger keeps
 * the history the clock already has, because a poll loop that re-zeroed the
 * clock every lap could never observe a settle.
 */
function armMutationClock(): void {
  if (observer !== null) {
    scheduleMutationClockDisarm();
    return;
  }
  // A fresh arm has no history. Stamped even when the observer cannot be
  // installed at all (no MutationObserver, no document), because the
  // alternative — a `lastMutationAt` of 0 — reads as hours of quiet and
  // would answer `settled:true` off an observation nobody made.
  lastMutationAt = now();
  if (typeof MutationObserver === 'undefined' || typeof document === 'undefined') return;
  observer = new MutationObserver(() => {
    lastMutationAt = now();
  });
  observer.observe(document.documentElement, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
  });
  scheduleMutationClockDisarm();
}

function scheduleMutationClockDisarm(): void {
  if (typeof setTimeout !== 'function') return;
  if (lingerTimer !== null && typeof clearTimeout === 'function') clearTimeout(lingerTimer);
  lingerTimer = setTimeout(() => {
    lingerTimer = null;
    disarmMutationClock();
  }, MUTATION_CLOCK_LINGER_MS);
  // The linger must never be the reason a Node-side test process (or a
  // future SSR pass) stays alive. Same contract as the perf watchdog.
  (lingerTimer as { unref?: () => void }).unref?.();
}

/** Disconnects the observer and cancels the linger. Safe when not armed. */
function disarmMutationClock(): void {
  if (lingerTimer !== null && typeof clearTimeout === 'function') clearTimeout(lingerTimer);
  lingerTimer = null;
  observer?.disconnect();
  observer = null;
}

/** Whether the document-wide mutation observer is installed right now. */
export function mutationClockArmed(): boolean {
  return observer !== null;
}

/**
 * Per-page-load init for the bridge chunk. Installs NOTHING: the mutation
 * clock is armed by the queries that need it and disarmed again when they
 * stop asking, so a soak or a perf run that never asks for `settled` carries
 * no document-wide observer at all.
 *
 * What it does do is start from a clean slate. The chunk survives a
 * teardown — `stores/harnessBridge.ts` drops its reference but the module
 * stays loaded — so a second activation in the same page must not inherit
 * the previous bridge's clock or its perf self-disarm notice.
 */
export function activateHarnessBridge(): () => void {
  disarmMutationClock();
  clearPerfSelfDisarm();
  return () => stopHarnessBridge();
}

export function stopHarnessBridge(): void {
  disarmMutationClock();
  if (perfRunActive()) stopPerfRun();
  clearPerfSelfDisarm();
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
      // The one query kind that reports settledness, and therefore the one
      // that pays for the observer. `element`, `globals`, `perf` and
      // `reload` answer without it and must not install it.
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
      const requested = Array.isArray(spec.meters)
        ? spec.meters.filter((m): m is string => typeof m === 'string')
        : [];
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
        meters: requested.length > 0 ? requested : undefined,
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
  const version = num(spec, 'v', 1);
  if (version !== 1) return fail(`unsupported query version ${version} (this bridge speaks v1)`);
  try {
    return await dispatch(spec);
  } catch (err) {
    return fail(err instanceof Error ? err.message : String(err));
  }
}
