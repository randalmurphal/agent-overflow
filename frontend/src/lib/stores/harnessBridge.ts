// The wire edge of the harness frontend bridge (§4 of
// docs/specs/testing-harness.md). Everything DOM- or perf-shaped lives in
// `lib/harness/`; this file is the subscription, the reply call, and the
// gate that keeps both out of an ordinary boot.
//
// THE GATE. `installHarnessBridge` runs on every launch, but in a normal
// session it does exactly one thing: read a boolean and register a
// callback that is never invoked. The bridge modules are reached only
// through `import('../harness/bridge')` inside that callback, so they are
// a separate rolldown chunk that a non-harness page never fetches, never
// parses and never evaluates. That is the "zero cost when the flag is
// absent" the brief asks for, and it is checkable: the chunk must not
// appear in `dist/index.html`'s modulepreload list.
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
import { wailsEventOn } from './wailsEvents';

interface UIQueryEvent {
  id?: unknown;
  spec?: unknown;
}

type BridgeModule = typeof import('../harness/bridge');

let bridgeModule: Promise<BridgeModule> | null = null;

function loadBridge(): Promise<BridgeModule> {
  bridgeModule ??= import('../harness/bridge').then((mod) => {
    // The mutation clock has to start with the bridge, not with the first
    // query: `settled` means "nothing changed in the last 300ms", and an
    // observer installed at query time would answer that about itself.
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
 * Arms the harness bridge if (and when) this session turns out to be
 * attached to a `--harness` / `--soak` backend. Returns a teardown that
 * is safe to call whether or not the bridge ever armed.
 */
export function installHarnessBridge(): () => void {
  let stopSubscription: (() => void) | null = null;
  const cancelArm = whenHarnessSession(() => {
    stopSubscription = wailsEventOn<UIQueryEvent>('harness:ui-query', (event) => {
      const id = typeof event?.id === 'string' ? event.id : '';
      if (!id) return;
      void answer(id, event.spec);
    });
    void loadBridge();
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
