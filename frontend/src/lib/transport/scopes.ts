// What this page's session is allowed to do — the client half of
// docs/specs/remote-access.md §5's capability model, and the TypeScript
// mirror of internal/transport/scopes.go's vocabulary.
//
// A surface asks `hasScope('threads:operate')` rather than "am I a remote
// session", so it states the capability it needs instead of a proxy for
// it. That is the whole point: the two questions answer the same today
// (the owner's screen holds everything, and a page that never paired
// holds nothing of its own), and they stop answering the same the moment
// a paired device arrives holding a named subset. A gate written against
// the proxy would then be wrong in both directions at once.
//
// THIS IS NEVER AUTHORIZATION. The backend re-checks every RPC against
// the session row (internal/transport/authorize.go), so the worst a
// wrong answer here can do is offer a control that is refused, or hide
// one that would have worked. Which is exactly why the answer is allowed
// to be conservative when the page cannot know: see the unpaired arm
// below.
//
// Sources, in precedence order:
//
//  1. A PAIRED session's granted scopes. `/auth/pair` and `/auth/token`
//     publish them (transport.TokenGrant.Scopes) and ./deviceSession.ts
//     stores them alongside the credential, so the answer is the grant
//     set the backend will actually enforce for this connection. It wins
//     over locality because a paired browser presents that session on
//     the upgrade even on loopback — the socket IS that session, so its
//     grants are what the gate compares.
//  2. The LOCAL page: the owner's own screen, which holds every
//     grantable scope (its session is the local page channel, and
//     internal/identity/local.go grants it identity.Scopes entire).
//     Stated as an explicit all-scopes answer rather than as an absent
//     check, so a surface reads the same way here as anywhere else.
//  3. Neither: a page reached over the network that has not paired. It
//     names no session of its OWN — it borrows the local page channel
//     and is bounded by the origin gate, whose partition no client can
//     enumerate — so the honest answer to "what was granted to me" is
//     nothing. Reads are unaffected: nothing asks permission before
//     reading, and this arm only ever hides controls whose RPC that page
//     cannot reach anyway.
//
// `host` is answered separately, and by PRESENCE. It is a method
// property rather than a grant — no session holds it, identity does not
// declare it, and internal/transport/authorize.go authorizes it from
// "is the caller on this machine" alone — so this module answers it the
// same way, from whether the page was served over loopback. A paired
// session on the owner's own machine therefore still opens files in an
// editor, which is what the backend would do.
//
// `session` is the other method property: the FLOOR the backend admits on
// session presence alone. Nothing here gates on it — a page that reached
// this module at all is talking to a backend that already answered the
// question — so it has no arm below and is only ever a name a refusal
// could carry.
//
// Reactivity mirrors ./runMode.ts: a `createSubscriber` notified at the
// two moments the answer can move, and never polled. Those moments are
// the bootstrap manifest resolving (every boot, and every reconnect
// refetch) and the pairing flow completing. Nothing clears the answer on
// a disconnect — the ladder is climbing back to the same backend, and a
// capability that flapped to "nothing" for the length of an outage would
// blank half the UI mid-reconnect. Same reasoning as the hello
// snapshot's survival in ./wsClient.ts.

import { createSubscriber } from 'svelte/reactivity';
import { pairedSessionScopes } from './deviceSession';

/**
 * One capability name. Mirrors internal/transport/scopes.go — same
 * spellings, same order, with the two values that are not grants last:
 * `session` (the method FLOOR — any live session passes) and `host`.
 *
 * Neither is ever held. No surface gates on them: `host` is answered from
 * page locality below, and `session` is a floor the backend admits on
 * session presence alone, so a client asking about it would be asking the
 * wrong question. They are in the union because the vocabulary is pinned
 * to the Go one in both directions, and because ./scopeRefusal.ts has to
 * be able to present a refusal naming either.
 */
export type Scope =
  | 'threads:read'
  | 'files:read'
  | 'threads:operate'
  | 'approvals:respond'
  | 'threads:autonomy'
  | 'terminal:operate'
  | 'git:operate'
  | 'attachments:write'
  | 'settings:read'
  | 'settings:write'
  | 'access:admin'
  | 'session'
  | 'host';

/**
 * The EXECUTE-tier capability names, mirroring
 * internal/transport/scopes.go's `scopeTiers` (every entry whose tier is
 * `TierExecute`). Restated here rather than derived, for the reason the
 * scope list itself is restated: this build has to be able to answer the
 * question without a round trip, and the two sides are pinned to each
 * other by test.
 *
 * `session` and `host` are absent on purpose — neither is a grant, so
 * neither can be in a session's set (see the module header).
 */
export const EXECUTE_SCOPES: readonly Scope[] = [
  'threads:operate',
  'approvals:respond',
  'threads:autonomy',
  'terminal:operate',
  'git:operate',
  'attachments:write',
  'settings:write',
  'access:admin',
] as const;

/** Every declared capability, in the spec table's order. */
export const SCOPES: readonly Scope[] = [
  'threads:read',
  'files:read',
  'threads:operate',
  'approvals:respond',
  'threads:autonomy',
  'terminal:operate',
  'git:operate',
  'attachments:write',
  'settings:read',
  'settings:write',
  'access:admin',
  'session',
  'host',
] as const;

const SCOPE_SET: ReadonlySet<string> = new Set(SCOPES);
const EXECUTE_SCOPE_SET: ReadonlySet<Scope> = new Set(EXECUTE_SCOPES);

/**
 * Is this a capability name this build knows?
 *
 * A name it does not know is never treated as held: a bundle older than
 * the backend that granted it cannot reason about a capability it has no
 * gates for, and inventing one would light up a surface on a guess.
 */
export function isScope(value: unknown): value is Scope {
  return typeof value === 'string' && SCOPE_SET.has(value);
}

/** Where the current answer came from, and what it says. */
export interface ScopeSnapshot {
  /**
   * Which of the three arms above resolved.
   * - `local-page`: the owner's own screen; every grantable scope.
   * - `paired-session`: this browser's paired session; `scopes` names them.
   * - `unpaired`: a networked page holding no session of its own.
   */
  source: 'local-page' | 'paired-session' | 'unpaired';
  /** True when every grantable scope is held; `scopes` is then empty. */
  everyScope: boolean;
  /** The named grants. Empty under `local-page` and `unpaired` alike. */
  scopes: ReadonlySet<Scope>;
  /**
   * Whether this page runs on the host desktop. Authorizes the `host`
   * scope, and is what a step-up proof resolves to this phase
   * (internal/transport/authorize.go stepUpProven).
   */
  onHost: boolean;
}

const NO_SCOPES: ReadonlySet<Scope> = new Set<Scope>();

// The pre-bootstrap answer. Nothing is known yet, and the page is not
// assumed to be the owner's screen: a control that appears a frame late
// is better than one that appears and then has to be taken away.
const UNRESOLVED: ScopeSnapshot = {
  source: 'unpaired',
  everyScope: false,
  scopes: NO_SCOPES,
  onHost: false,
};

let snapshot: ScopeSnapshot = UNRESOLVED;
// Locality as the manifest reported it, held apart from the snapshot
// because it is an input to two of its fields and survives a re-resolve
// that the pairing store triggers.
let pageOnHost = false;

let notifyScopesChanged: (() => void) | null = null;
const subscribeScopes = createSubscriber((update) => {
  notifyScopesChanged = update;
  return () => {
    if (notifyScopesChanged === update) notifyScopesChanged = null;
  };
});

function sameSnapshot(a: ScopeSnapshot, b: ScopeSnapshot): boolean {
  if (a.source !== b.source || a.everyScope !== b.everyScope || a.onHost !== b.onHost) return false;
  if (a.scopes.size !== b.scopes.size) return false;
  for (const scope of a.scopes) {
    if (!b.scopes.has(scope)) return false;
  }
  return true;
}

// resolve rebuilds the snapshot from the two sources and publishes it
// only when it moved. The equality check is what keeps a manifest
// refetch on every reconnect from invalidating every gated surface in
// the app for an answer that did not change.
function resolve(): void {
  const paired = pairedSessionScopes();
  let next: ScopeSnapshot;
  if (paired !== null) {
    // Unknown names are dropped rather than carried: this build has no
    // gate that could ask about one, and keeping it would only make the
    // snapshot compare unequal to itself across a rebuild.
    const known = new Set<Scope>();
    for (const name of paired) {
      if (isScope(name)) known.add(name);
    }
    next = { source: 'paired-session', everyScope: false, scopes: known, onHost: pageOnHost };
  } else if (pageOnHost) {
    next = { source: 'local-page', everyScope: true, scopes: NO_SCOPES, onHost: true };
  } else {
    next = { source: 'unpaired', everyScope: false, scopes: NO_SCOPES, onHost: false };
  }
  if (sameSnapshot(snapshot, next)) return;
  snapshot = next;
  notifyScopesChanged?.();
}

/**
 * Was this page's session granted `scope`?
 *
 * Reactive when read from a `$derived` or a template, so a surface
 * re-evaluates when the manifest lands and when pairing completes.
 *
 * `host` is answered from host presence rather than from the grant set,
 * because no session holds it — see the module header.
 */
export function hasScope(scope: Scope): boolean {
  subscribeScopes();
  if (scope === 'host') return snapshot.onHost;
  return snapshot.everyScope || snapshot.scopes.has(scope);
}

/**
 * The whole resolved answer, for diagnostics and for a caller that needs
 * to explain WHY rather than only whether. Reactive on the same
 * subscription `hasScope` uses.
 */
export function grantedScopes(): ScopeSnapshot {
  subscribeScopes();
  return snapshot;
}

/**
 * Is this page in VIEW-ONLY mode — a session that was granted a set, and
 * whose set contains no execute-tier capability?
 *
 * The one definition of the mode, so the ambient indicator and any
 * surface that needs the mode rather than a named capability read the
 * same answer. It is derived from the GRANT SET and never from a device
 * class: the pairing surface mints `view-only` for a phone and `full`
 * for a phone alike (docs/specs/remote-access.md §5), so "is it a
 * browser" answers a different question.
 *
 * A control still gates on the capability it needs — `hasScope('git:operate')`
 * — never on this. A view-only session is not the only reason a given
 * control is out of reach, and a mode-shaped gate would disable a
 * `git:operate` button for a session that holds it while lacking
 * something else.
 *
 * Three answers are deliberately FALSE:
 *  - the local page, which holds every scope;
 *  - a full-access paired device, which holds execute-tier names;
 *  - a page whose answer has not resolved yet, and an unpaired networked
 *    page — both hold an EMPTY set, which is "nothing was granted to me",
 *    not "I was granted a read-only slice". Answering true there would
 *    flash the indicator on every boot and would label the pairing prompt
 *    as a working read-only app.
 */
export function isViewOnly(): boolean {
  subscribeScopes();
  if (snapshot.everyScope) return false;
  return isViewOnlyGrantSet(snapshot.scopes);
}

/**
 * Is an arbitrary grant set view-only — non-empty, and naming no
 * execute-tier capability?
 *
 * The same question `isViewOnly` asks about THIS page, asked about a set
 * that came from somewhere else: the settings pane labels each paired
 * device from the grants its session carries
 * (`AccessSession.Scopes`). One definition, because a device labelled
 * "View only" on one screen and "Full access" on another is worse than
 * either answer alone — which is also why the backend ships the grant
 * set rather than a verdict of its own.
 *
 * An EMPTY set is false, for the reason `isViewOnly` gives: "nothing was
 * granted to me" is not "I was granted a read-only slice". Unknown names
 * are ignored rather than treated as execute-tier — this build cannot
 * reason about a capability it has no gates for, and guessing would
 * label a full-access device read-only.
 */
export function isViewOnlyGrantSet(scopes: Iterable<string>): boolean {
  let named = false;
  for (const scope of scopes) {
    named = true;
    if (isScope(scope) && EXECUTE_SCOPE_SET.has(scope)) return false;
  }
  return named;
}

/**
 * Called only by ./bootstrap.ts once it has validated a manifest.
 * `remote` is the manifest's own word for "this request did not come
 * from this machine"; its negation is host presence.
 *
 * Keeping the update at that boundary is what stops a non-boolean wire
 * value from deciding what the app offers, the same reason the harness
 * latch sits there.
 */
export function setPageGrantsFromBootstrap(remote: boolean): void {
  pageOnHost = remote !== true;
  resolve();
}

/**
 * Re-read the paired-session grant set.
 *
 * The one caller is `wsClient.redialAfterPairing()`: completing the
 * pairing flow both stores a credential and re-dials under it, and the
 * grants that arrived with that credential are what this page may now
 * do. Re-resolving anywhere else would be polling, which the manifest
 * refetch already makes unnecessary — a session's grants are immutable
 * for its lifetime, and a revocation force-closes the socket rather than
 * narrowing what it holds.
 */
export function refreshGrantedScopes(): void {
  resolve();
}

/**
 * Test-only reset back to the pre-bootstrap answer. Production code
 * never calls it.
 */
export function __resetScopesForTest(): void {
  pageOnHost = false;
  snapshot = UNRESOLVED;
  notifyScopesChanged?.();
}
