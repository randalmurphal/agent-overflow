// The wire edge of the harness frontend bridge (§4 of
// docs/specs/testing-harness.md). Everything DOM- or perf-shaped lives in
// `lib/harness/`; this file is the subscription, the reply call, and the
// gate that keeps both out of an ordinary boot.
//
// THE GATE, in two stages.
//
// 1. NOT A HARNESS SESSION → nothing at all. `installHarnessBridge` runs
//    on every launch, but in a normal session it does exactly one thing:
//    read a boolean and register a callback that is never invoked. The
//    bridge modules are reached only through `import('../harness/bridge')`
//    inside the query handler, so they are a separate rolldown chunk that
//    a non-harness page never fetches, never parses and never evaluates.
//    That is the "zero cost when the flag is absent" the brief asks for,
//    and it is checkable: the chunk must not appear in `dist/index.html`'s
//    modulepreload list.
//
// 2. A HARNESS SESSION → the subscription, and NOTHING ELSE, until a query
//    actually arrives. The chunk import is deferred to the first
//    `harness:ui-query`, and the document-wide MutationObserver is deferred
//    further still: loading the chunk installs nothing, and only a query
//    that reports settledness (`viewport`) arms the clock — which then
//    disarms itself again once nothing is asking. A harness run that never
//    asks the page anything — which is every soak run, streaming for hours
//    to reproduce renderer memory and hang behaviour — therefore pays
//    nothing but one idle event listener, and a perf run or a bench
//    workload measures a renderer with no observer on it. An always-on
//    observer allocating a MutationRecord per text delta is a probe that
//    perturbs exactly the experiment the rig exists to run. What it costs
//    is history: a freshly armed clock has none, so that query's `settled`
//    reads false. See lib/harness/bridge.ts's header.
//
// A REMOTE SESSION NEVER ARMS. `harness:ui-query` is an
// AudienceLoopbackOnly channel (internal/transport/event_channels.go), so
// a browser attached over the LAN cannot receive one — arming there would
// install the observer for a query that can never come.
//
// WHY Call.ByName AND NOT A GENERATED BINDING. `methodgen` scans the
// repo-root `App` type only; `Harness` is deliberately not scanned, which
// is what keeps harness RPCs out of the production binding surface
// entirely. Teaching the generator a second receiver to reach ONE method
// would put a LocalOnly, harness-only call into `frontend/bindings/` for
// every build, where the architecture rules would then have to carve it
// out by name. `Call.ByName` goes through `wsClient.callByName`, which
// emits the identical `{type:"rpc", id, method, params}` frame the
// generated `Call.ByID` wrappers emit — same socket, same dispatcher,
// same LocalOnly authorization check on the Go side. Nothing about the
// transport boundary is bypassed; only the method IDENTIFICATION differs,
// and a name is what the harness client and `ao-harness` already use.
//
// SEQUENCING. A reply must not be able to outrun its query's waiter, and
// it cannot: the backend registers the waiter BEFORE it emits (see
// queryUI in app_harness_ui.go). What CAN happen is two queries arriving
// while the module is still loading, so answers are chained per query
// rather than serialised — a slow `globals` read must not delay an
// unrelated `viewport`.

import { Call } from '@wailsio/runtime';
import { whenHarnessSession } from '../transport/harnessMode';
import { isViewOnlySession } from '../transport/runMode';
import { wailsEventOn } from './wailsEvents';

interface UIQueryEvent {
  id?: unknown;
  spec?: unknown;
}

type BridgeModule = typeof import('../harness/bridge');

let bridgeModule: Promise<BridgeModule> | null = null;

function loadBridge(): Promise<BridgeModule> {
  bridgeModule ??= import('../harness/bridge').then((mod) => {
    // Activation installs no observer — the mutation clock is armed by the
    // queries that report `settled` and disarmed again when they stop
    // arriving. What this call does is give the chunk a clean slate: the
    // module survives a teardown, so a re-installed bridge must not inherit
    // the previous one's clock or its perf self-disarm notice.
    mod.activateHarnessBridge();
    return mod;
  });
  return bridgeModule;
}

async function answer(id: string, spec: unknown): Promise<void> {
  let result: unknown;
  try {
    const mod = await loadBridge();
    result = await mod.answerHarnessQuery(spec);
    // A query addressed to another attached page's perf run. Answering
    // would win the backend's first-reply race with a refusal and poison
    // the run that CAN answer; see HARNESS_NO_REPLY in harness/bridge.ts.
    if (mod.isHarnessNoReply(result)) return;
  } catch (err) {
    // A failure to even load the bridge still owes the backend a reply;
    // otherwise the caller waits the full 10s to learn nothing.
    result = { error: err instanceof Error ? err.message : String(err) };
  }
  try {
    await Call.ByName('HarnessUIQueryReply', id, result);
  } catch (err) {
    // A refused reply is a harness-side fault worth seeing in the page
    // console, but it must not take the subscription down with it.
    console.error('harness bridge: reply for query', id, 'failed:', err);
  }
}

/**
 * Subscribes to the ui-query channel if (and when) this session turns out
 * to be a LOCAL page attached to a `--harness` / `--soak` backend. The
 * bridge itself loads on the first query that arrives. Returns a teardown
 * that is safe to call whether or not either half ever happened.
 */
export function installHarnessBridge(): () => void {
  let stopSubscription: (() => void) | null = null;
  const cancelArm = whenHarnessSession(() => {
    // Locality, read at ARM time rather than at install time: the manifest
    // sets the remote bit before it sets the harness bit (see
    // transport/bootstrap.ts), so by the time this runs the answer is
    // final. A remote page can never be sent a `harness:ui-query`, so a
    // subscription here would be a listener for an event that cannot
    // arrive — and the module it would load is loopback-only tooling.
    if (isViewOnlySession()) return;
    stopSubscription = wailsEventOn<UIQueryEvent>('harness:ui-query', (event) => {
      const id = typeof event?.id === 'string' ? event.id : '';
      if (!id) return;
      void answer(id, event.spec);
    });
  });
  return () => {
    cancelArm();
    stopSubscription?.();
    stopSubscription = null;
    if (!bridgeModule) return;
    const pending = bridgeModule;
    bridgeModule = null;
    void pending.then((mod) => mod.stopHarnessBridge()).catch(() => {});
  };
}
