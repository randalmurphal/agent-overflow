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
import { harnessPageMarker, whenHarnessSession } from '../transport/harnessMode';
import { hasScope } from '../transport/scopes';
import { wailsEventOn } from './wailsEvents';
import { onTransportStatusChange } from './transportStatus.svelte';

interface UIQueryEvent {
	id?: unknown;
	spec?: unknown;
	pageId?: unknown;
}

type BridgeModule = typeof import('../harness/bridge');

let bridgeModule: Promise<BridgeModule> | null = null;

// Immutable for this document lifetime. Re-installing the bridge after a
// reconnect must not let a stale page identity answer a newer page's query.
const FRONTEND_PAGE_ID = (() => {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  return `page-${Math.random().toString(36).slice(2)}-${Date.now().toString(36)}`;
})();

/** Test seam for constructing a targeted synthetic event. */
export function __frontendPageIDForTest(): string {
  return FRONTEND_PAGE_ID;
}

let pageRegistration: Promise<void> | null = null;

function publishPageIdentity(): void {
  if (typeof window === 'undefined' || !window.history || typeof window.history.replaceState !== 'function') return;
  try {
    const current = new URL(window.location.href);
    if (current.searchParams.get('pageId') === FRONTEND_PAGE_ID) return;
    current.searchParams.set('pageId', FRONTEND_PAGE_ID);
    window.history.replaceState(null, '', current.pathname + `?${current.searchParams.toString()}` + current.hash);
  } catch (err) {
    console.error('harness bridge: publish page identity failed:', err);
  }
}

function registerPage(): Promise<void> {
  pageRegistration ??= Call.ByName('HarnessRegisterPage', FRONTEND_PAGE_ID, harnessPageMarker(),
    typeof window !== 'undefined' ? window.location.origin : '').then((identity: unknown) => {
      if (!identity || typeof identity !== 'object' || (identity as { pageId?: unknown }).pageId !== FRONTEND_PAGE_ID) {
        throw new Error('harness page registration returned a different page identity');
      }
    });
  return pageRegistration;
}

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

async function answer(id: string, pageId: string, spec: unknown): Promise<void> {
  let result: unknown;
  try {
    await registerPage();
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
    await Call.ByName('HarnessUIQueryReply', pageId, id, result);
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
  let stopTransportStatus: (() => void) | null = null;
  let active = true;
  const subscribe = () => {
    if (!active || stopSubscription !== null) return;
    stopSubscription = wailsEventOn<UIQueryEvent>('harness:ui-query', (event) => {
      if (!active) return;
      const id = typeof event?.id === 'string' ? event.id : '';
      const pageId = typeof event?.pageId === 'string' ? event.pageId : '';
      if (!id) return;
      if (!pageId || pageId !== FRONTEND_PAGE_ID) return;
      void answer(id, pageId, event.spec);
    });
  };
  const cancelArm = whenHarnessSession(() => {
    if (!active) return;
    // Host presence, read at ARM time rather than at install time: the
    // manifest publishes locality before it sets the harness bit (see
    // transport/bootstrap.ts), so by the time this runs the answer is
    // final. The harness drives THIS desktop's renderer, which is what the
    // `host` scope names — a page elsewhere can never be sent a
    // `harness:ui-query`, so a subscription would be a listener for an
    // event that cannot arrive, and the module it would load is host
    // tooling.
    if (!hasScope('host')) return;
    publishPageIdentity();
    stopTransportStatus = onTransportStatusChange((status) => {
      if (!active) return;
      if (status.status === 'connected') {
        pageRegistration = null;
        void registerPage().then(subscribe).catch((err) => {
          console.error('harness bridge: page registration failed:', err);
        });
      } else {
        pageRegistration = null;
      }
    });
    // The initial transport may already be usable even when the status
    // mirror has not observed its first connected edge yet.
    void registerPage().then(subscribe).catch((err) => {
      console.error('harness bridge: page registration failed:', err);
    });
  });
  return () => {
    active = false;
    cancelArm();
    const unregister = () => {
      stopSubscription?.();
      stopTransportStatus?.();
      stopSubscription = null;
      stopTransportStatus = null;
    };
    if (!bridgeModule) {
      unregister();
      return;
    }
    const pending = bridgeModule;
    bridgeModule = null;
    void pending.then((mod) => {
      const receipt = mod.stopHarnessBridge('page-unload');
      if (receipt.errors.length > 0) {
        console.error('harness bridge: teardown receipt reported errors:', receipt.errors);
      }
      if (receipt.perf !== null || receipt.monitors.length > 0) {
        console.info('harness bridge: retained partial teardown receipt', receipt);
      }
    }).catch((err) => {
      console.error('harness bridge: teardown failed:', err);
    }).finally(unregister);
  };
}
