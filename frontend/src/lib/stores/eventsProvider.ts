import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { threadMachine, getAttachedBackends } from './attachedBackends.svelte';
// Provider-lifecycle event domain: approvals, user-input requests, usage /
// rate-limit reporting, provider status + account probes, turn
// started/completed, session-died, subagent notifications, and todo
// updates. Fan-in target of events.ts's setupEventListeners.
import type {
  ApprovalEvent,
  ModelFallbackEvent,
  ProviderAccountEvent,
  ProviderSessionAccountEvent,
  TodoUpdateEvent,
  ProviderStatusEvent,
  SessionDiedEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
} from '../types/events';
import { clearProviderAccount, setProviderAccount } from './accountInfo.svelte';
import { asProviderID } from '../types/providers';
import { invalidateProviderModels, refreshProviderModels } from './providerModels.svelte';
import { loadProviderAccounts } from './providerAccounts.svelte';
import { iterPanes } from './panes.svelte';
import { recordProviderStatus } from './providerStatus.svelte';
import { clearProviderRateLimits, setProviderRateLimits } from './rateLimitsInfo.svelte';
import {
  projectApprovalRequest,
  projectApprovalResolution,
  projectSendResolved,
  projectTurnCompleted,
  projectTurnStarted,
  projectUserInputRequest,
  projectUserInputResolution,
} from './threadStatuses.svelte';
// Import parseTokenUsage from its leaf home, not via the thread.svelte barrel,
// so this module no longer depends on the 2800-line thread store at all.
import { parseTokenUsage } from './threadTurnProjection';
import {
  clearLiveUsageSnapshot,
  recordLiveUsageSnapshot,
  takeLiveUsageSnapshot,
} from './threadContextWindow';
import { bumpUsageRefresh } from './usageRefresh.svelte';
import { patchThreadDurableStatus, syncLatestTurnCompleted, syncThreadActivity, updateThreadUsageCache } from './eventsThreadRows';
import { adoptEventStamp } from './threadHistoryStamps';
import type { ErrorSurface, ThreadPaneIngest } from './threadPaneRoles';
import { getConnectionId } from '../transport/clientIdentity';

// The provider fan-out writes the ingest surface AND the slice of the
// pane's error surface its handlers own (session errors, the provider
// banner). The registry hands out whole ThreadPanes; they narrow here at
// the one acquisition point, so a new pane member use fails to compile
// until threadPaneRoles.ts lists it.
type ProviderEventPane = ThreadPaneIngest &
  Pick<
    ErrorSurface,
    | 'generalError'
    | 'setGeneralError'
    | 'setSessionError'
    | 'clearSessionError'
    | 'providerBanner'
    | 'setProviderBanner'
  >;

function ingestPanes(): Iterable<ProviderEventPane> {
  return iterPanes();
}

// A `fail` frame on either interactive channel reaches every client, but
// it describes ONE client's attempt: the prompt is still open for
// everybody else, so a sticky "Failed to respond to approval" banner on a
// screen that never answered is both wrong and unclearable. Only the
// connection that asked reacts.
//
// The CONNECTION and not the device: two tabs of one browser answer
// independently, and keying on the device id would put the losing tab's
// error on the other one.
//
// An UNSTAMPED frame is shown, which is the pre-stamp behaviour kept
// verbatim. The stamp is additive, so a bundle running against a backend
// too old to send it must degrade to what that backend has always done
// rather than silently swallowing the only surfacing this failure has:
// the RPC rejection reaches an event handler and goes nowhere. Every
// current backend stamps it, so the class is closed where the frame is
// produced.
function failureIsOurs(connectionId: string | undefined): boolean {
  return !connectionId || connectionId === getConnectionId();
}

export function applyModelFallback(evt: ModelFallbackEvent): void {
  if (!evt?.threadId) return;
  for (const pane of ingestPanes()) {
    if (pane.threadId === evt.threadId) {
      pane.applyEffectiveModel(evt.effectiveModel ?? '', evt.revision);
    }
  }
}

export function applyApprovalEvent(evt: ApprovalEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectApprovalRequest(
      evt.request.threadId,
      evt.request.requestId,
      evt.request.kind,
    );
    for (const pane of ingestPanes()) {
      if (pane.threadId === evt.request.threadId) {
        pane.addApproval(evt.request);
      }
    }
    // Approval requests are a sidebar-bump boundary: the agent is
    // paused waiting on the user. Resolutions ride on the user's
    // reply — no separate bump there. Use the wire-event timestamp
    // (matches the value MarkThreadActivity wrote on the backend) so
    // the cached activity doesn't drift on local clock skew.
    syncThreadActivity(evt.request.threadId, evt.requestedAt ?? Date.now());
    return;
  }

  if ((evt.action === 'resolve' || evt.action === 'fail') && evt.requestId) {
    projectApprovalResolution(evt.threadId, evt.requestId);
    for (const pane of ingestPanes()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadApproval = pane.pendingApprovals.some((approval) => approval.requestId === evt.requestId);
      pane.removeApproval(evt.requestId);
      if (hadApproval && evt.action === 'fail' && evt.detail && failureIsOurs(evt.connectionId)) {
        pane.setGeneralError(`Failed to respond to approval: ${evt.detail}`);
      }
    }
  }
}

export function applyUserInputEvent(evt: UserInputEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectUserInputRequest(evt.request.threadId, evt.request.requestId);
    for (const pane of ingestPanes()) {
      if (pane.threadId === evt.request.threadId) {
        pane.addUserInput(evt.request);
      }
    }
    // User-input requests are a sidebar-bump boundary alongside
    // approvals and turn complete. The user's submitted answer
    // arrives via a separate user_text path that bumps on its own.
    // Use the wire-event timestamp so the cached activity stays in
    // lockstep with the persisted threads.updated_at.
    syncThreadActivity(evt.request.threadId, evt.requestedAt ?? Date.now());
    return;
  }

  if ((evt.action === 'resolve' || evt.action === 'fail') && evt.requestId) {
    projectUserInputResolution(evt.threadId, evt.requestId);
    for (const pane of ingestPanes()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadRequest = pane.pendingUserInputs.some((request) => request.requestId === evt.requestId);
      pane.removeUserInput(evt.requestId);
      if (hadRequest && evt.action === 'fail' && evt.detail && failureIsOurs(evt.connectionId)) {
        pane.setGeneralError(`Failed to submit input: ${evt.detail}`);
      }
    }
  }
}

export function applyUsageEvent(evt: UsageEvent, backend: BackendKey = HOME_BACKEND): void {
  if (!evt) return;

  // `rate_limits` piggybacks on the same channel but doesn't touch the
  // context-window ring. Route to the provider-global store and bail
  // before the context-window update path so a rate-limit refresh
  // never clobbers the last known token-window snapshot.
  //
  // Rate limits are an account property, not a thread property — every
  // pane on the same provider sees the same value. The global store
  // also makes the rings persist across thread switches and turn
  // completions until the next non-empty event arrives. The Go-side
  // Claude probe (internal/provider/claude/ratelimits_probe.go) emits
  // these events with no threadId because the probe is account-wide;
  // wire-driven envelopes from a live session still carry one but the
  // rate-limits branch doesn't read it.
  if (evt.action === 'rate_limits') {
    if (!evt.rateLimits) return;
    setProviderRateLimits(evt.rateLimits, backend);
    return;
  }
  if (evt.action === 'rate_limits_removed') {
    const provider = asProviderID(evt.rateLimits?.provider);
    const accountId = evt.rateLimits?.accountId;
    if (provider && accountId) clearProviderRateLimits(provider, accountId, backend);
    return;
  }

  // Context-window updates require a threadId because they target a
  // specific pane's ring.
  if (!evt.threadId) return;

  const payload = evt.action === 'usage'
    ? {
        usedTokens: evt.usedTokens ?? 0,
        maxTokens: evt.maxTokens,
        usedPercentage: evt.contextPercent,
        ...(evt.autoCompactPercent ? { autoCompactPercent: evt.autoCompactPercent } : {}),
        ...(evt.autoCompactTokenLimit ? { autoCompactTokenLimit: evt.autoCompactTokenLimit } : {}),
        ...(evt.exceeded ? { exceeded: true } : {}),
      }
    : null;

  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    if (payload) {
      pane.setContextWindow(payload);
    } else {
      pane.clearContextWindow();
    }
  }

  if (payload) {
    // The visible pane's meter is already updated above at full cadence.
    // The snapshot for thread-switch seeding goes to the side cache
    // (threadContextWindow.ts) instead of rewriting Thread.lastTokenUsage
    // per event — that patchThreadEverywhere path rebuilt the whole
    // sidebar array and replaced pane.thread ~2Hz per streaming thread.
    // The row converges once per turn in applyTurnCompleted.
    recordLiveUsageSnapshot(
      evt.threadId,
      JSON.stringify({
        usedTokens: payload.usedTokens,
        maxTokens: payload.maxTokens,
        contextPercent: payload.usedPercentage,
        autoCompactPercent: payload.autoCompactPercent,
        autoCompactTokenLimit: payload.autoCompactTokenLimit,
        ...(payload.exceeded ? { exceeded: true } : {}),
      }),
    );
  } else {
    // 'reset' clears the persisted copy immediately (turn-boundary-rare,
    // so the row churn is not the hot path); the side cache entry must
    // go too or it would shadow the cleared row on the next seed.
    clearLiveUsageSnapshot(evt.threadId);
    updateThreadUsageCache(evt.threadId, '');
  }
}

// kindToLegacyStatus maps the chat-rewrite closed kind enum onto the legacy
// `status` vocabulary the ProviderStatusBanner already renders. Keeps the
// banner component untouched while the router adopts the new vocabulary —
// the two pipelines converge here rather than in the view.
//
// Retry vocabulary lives on `provider:item_event` (`api_retry` row) now,
// not on this banner channel; session-death drives `pane.generalError`
// via `provider:session_died`. So the legacy mapping only needs to cover
// the boot-time provider-presence states.
//
// `binary_stale` has no legacy counterpart — binary detection cannot know a
// session outlived the binary it started on — so it maps onto itself. Its
// WITHDRAWAL speaks the legacy vocabulary instead: a thread-scoped
// `status: 'ready'` with no kind, which takes the banner=null path below.
const KIND_TO_LEGACY_STATUS: Record<NonNullable<ProviderStatusEvent['kind']>, ProviderStatusEvent['status']> = {
  binary_missing: 'not_found',
  unauthenticated: 'unauthenticated',
  version_incompatible: 'version_too_old',
  binary_stale: 'binary_stale',
};

export function applyProviderStatus(evt: ProviderStatusEvent, backend: BackendKey = HOME_BACKEND): void {
  if (!evt) return;

  // Chat-rewrite emissions carry `kind` and optionally `threadId`. The
  // legacy binary-detect emissions carry `provider + status`. Derive a
  // unified shape before fanning out so downstream consumers don't have
  // to branch.
  let effectiveStatus = evt.status;
  if (evt.kind) {
    const mapped = KIND_TO_LEGACY_STATUS[evt.kind];
    if (!mapped) {
      // An unknown kind leaks the banner to the console so the gap is
      // visible in dev — the spec calls this out as "require updating the
      // frontend banner component in the same PR". Drop without rendering.
      console.warn(`provider:status: unknown kind "${evt.kind}" — dropped`);
      return;
    }
    effectiveStatus = mapped;
  }

  const provider = asProviderID(evt.provider);
  if (!provider || !effectiveStatus) return;

  if (!evt.threadId) {
    invalidateProviderModels(provider, backend);
  }

  const normalized: ProviderStatusEvent = { ...evt, provider, status: effectiveStatus };

  // Thread-scoped status belongs to matching panes only. Writing it into
  // the provider-global cache leaks one pane's auth/session failure into
  // every other pane using the same provider.
  if (!evt.threadId) {
    recordProviderStatus(normalized, backend);
  }

  // A thread-scoped clear WITHDRAWS the pane's own banner rather than
  // pinning it to "nothing": `undefined` lets the pane fall back to the
  // provider-global status, so a pane whose stale-binary banner was just
  // withdrawn still shows the provider-wide auth failure every other pane
  // shows. `null` here would make it the one pane showing nothing.
  const banner = effectiveStatus === 'ready' ? undefined : normalized;
  for (const pane of ingestPanes()) {
    if (pane.thread?.provider !== provider) continue;
    if (threadMachine(pane.threadId ?? '', pane.thread?.projectId) !== backend) continue;
    // Kind-bearing events can carry a threadId for per-pane scoping; when
    // present, only update the matching pane. Without a threadId the event
    // is provider-global (legacy behavior) and fans out to every matching
    // pane as before.
    if (evt.threadId && pane.threadId !== evt.threadId) continue;
    // A thread-scoped banner is owned by its thread: only a thread-scoped
    // withdrawal (or the session's disconnect) may take it down. Letting a
    // provider-global event reset the slot would drop it for good, because
    // the backend raises it on transitions only — the account probe emits
    // a global status on every recheck, including the refresh button.
    if (!evt.threadId && pane.providerBanner?.threadId) continue;
    pane.setProviderBanner(evt.threadId ? banner : undefined);
  }
}

// The `provider:account` push fires only when the backend's account probe
// misses its cache, so a webview that (re)connects after the startup probe
// completed never receives one. Pull the selection instead — the same
// first-connect race GetRateLimitsSnapshots closes for the rings' data, closed
// here for the account identity those rings are keyed by. setProviderAccount's
// generation guard keeps a concurrent live event ahead of this snapshot.
//
// The pull itself is providerAccounts', not ours: that store is the one place
// that projects a listing (selection AND per-account quotas) and it warms its
// own cache doing so, so a second projection here would be a copy that drifts.
// It reports its own failures as a toast rather than rejecting, so the callers'
// catch blocks are belt-and-braces.
export async function hydrateProviderAccounts(): Promise<void> {
  await Promise.all(getAttachedBackends().map((computer) => loadProviderAccounts(computer.id)));
}

export function applyProviderAccount(evt: ProviderAccountEvent, backend: BackendKey = HOME_BACKEND): void {
  if (!evt) return;
  const provider = asProviderID(evt.provider);
  if (!provider) return;
  // The model catalog is account-scoped on both providers — Claude's is
  // enriched from the very probe that emits this event, and Codex's list is
  // whatever the signed-in account may run. So an account transition is
  // exactly when the cached catalog stops being the right answer. Refresh
  // (load, then swap) rather than invalidate: the composer's context/effort
  // labels read the store synchronously, and an emptied cache would blank them
  // until something happened to re-fetch.
  void refreshProviderModels(provider, backend).catch((error) => {
    console.warn(`events: refresh ${provider} model catalog after account change`, error);
  });
  if (evt.cleared) {
    clearProviderAccount(provider, evt.generation, backend);
    return;
  }
  if (typeof evt.account !== 'object' || evt.account === null) return;
  setProviderAccount(provider, evt.account, evt.accountId, evt.generation, backend);
}

export function applyProviderSessionAccount(evt: ProviderSessionAccountEvent): void {
  if (!evt?.threadId) return;
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setProviderSessionAccount(evt.connected ? evt : null);
    // A `binary_stale` banner is a claim about the session that is running:
    // it is pinned to the binary it started on. A disconnect retires that
    // claim however it happened (restart, crash, idle sweep, user stop), and
    // the next session's own status event is what may raise it again. Every
    // other banner kind describes the INSTALL rather than this session, so
    // scope the clear to the one status whose premise just went away.
    if (!evt.connected && pane.providerBanner?.status === 'binary_stale') {
      pane.setProviderBanner(undefined);
    }
  }
}

/**
 * Route `provider:turn_started` to the global active-turn registry
 * (single source of truth — see threadStatuses.svelte.ts). Both the
 * sidebar pill and the chat working indicator read from there. This
 * is one of two live backend sources that can record a turn; the other
 * is `GetThreadLiveState` hydration after refresh. Neither path derives
 * turn activity from durable item history.
 */
export function applyTurnStarted(evt: TurnStartedEvent): void {
  if (!evt?.threadId || !evt.turnId) return;
  // Pass the full {turnIndex, startedAt} into the global registry so
  // the chat working indicator's self-ticking timer and the timeline
  // boundary projection can read both without a separate write path.
  projectTurnStarted(evt.threadId, evt.turnId, evt.turnIndex, evt.startedAt);
  patchThreadDurableStatus(evt.threadId, {
    hasActionableProposedPlan: false,
    hasIncompleteTurn: false,
  });
  // A wire turn-start is proof the provider session is alive and
  // serving — any stale session_died banner for this thread is now
  // contradicted by visible streaming. Scoped to session-kind so this
  // doesn't clobber an orthogonal error sharing the slot.
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.clearSessionError();
  }
}

/**
 * Route `provider:turn_completed` to the matching pane. Clears the
 * global active-turn registry entry (threadStatuses) and writes the
 * settled projection for read-state and trace/debug consumers.
 *
 * `tokenUsage` arrives as a JSON-encoded string on the wire because
 * triage round-trips it through the DB's `token_usage_json` column. We
 * parse it here via `parseTokenUsage` — the same helper the pane uses on
 * thread-switch rehydration — so malformed JSON degrades gracefully to
 * `tokenUsage: null` rather than crashing the listener.
 */
export function applyTurnCompleted(evt: TurnCompletedEvent): void {
  if (!evt?.threadId || !evt.turnId) return;
  // New usage_ledger rows may exist for this turn; nudge every usage
  // surface (composer chip, sidebar footer, usage modal) to refetch —
  // the composer chip is thread-scoped and only reacts to its own
  // thread's bump; see usageRefresh.svelte.ts.
  bumpUsageRefresh(evt.threadId);
  // In-memory only (docs/architecture/thread-replica-sync.md §3.4): the pair
  // lets a thread the user watched stream and then re-opened get a
  // `fresh` window sync instead of paying a convergence fetch, but it
  // never reaches the durable replica.
  adoptEventStamp(evt.threadId, evt.historyEpoch, evt.historyRev);
  const rawAssistantId = evt.assistantMessageId ?? '';
  const settled = {
    turnId: evt.turnId,
    turnIndex: evt.turnIndex,
    startedAt: evt.startedAt,
    completedAt: evt.completedAt,
    stopReason: evt.stopReason ?? '',
    assistantMessageId: rawAssistantId === '' ? null : rawAssistantId,
    tokenUsage: parseTokenUsage(evt.tokenUsage),
    aborted: Boolean(evt.aborted),
    errorMessage: evt.errorMessage ?? '',
  };
  // Clear the turn from the sidebar projection. Errored turns flip the
  // pill to Failed; clean aborts flip it to Interrupted UNLESS the
  // backend marked this as a revert-on-interrupt, in which case the
  // pill stays clean (nothing happened, so don't paint it like it did).
  projectTurnCompleted(evt.threadId, evt.turnId, {
    aborted: settled.aborted,
    errorMessage: settled.errorMessage,
    revertedUserMessage: Boolean(evt.revertedUserMessage),
  });
  patchThreadDurableStatus(evt.threadId, { hasIncompleteTurn: false });
  // Converge the durable row with the turn's usage ONCE, at the turn
  // boundary: mid-turn snapshots live in the side cache (consulted by
  // seedContextWindow, so mid-turn thread switches still seed fresh),
  // and this single row write replaces the per-usage-event
  // patchThreadEverywhere churn. take() drops the entry, so the row is
  // the authority again until the next turn's first usage event.
  const usageSnapshot = takeLiveUsageSnapshot(evt.threadId);
  if (usageSnapshot !== undefined) {
    updateThreadUsageCache(evt.threadId, usageSnapshot);
  }
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.settleTurn(settled);
  }
  // Top-level turn complete (clean, errored, or synthesized for
  // session_died) is a sidebar-bump boundary. The backend marks
  // nested/internal completions with countsAsActivity=false so subagent
  // turns update live turn state without changing read/sidebar state.
  if (evt.countsAsActivity !== false && Number.isFinite(evt.completedAt)) {
    syncLatestTurnCompleted(evt);
    syncThreadActivity(evt.threadId, evt.completedAt);
  }
  // Send-queue drain is owned by the backend. Triage flushes queued
  // messages at safe provider boundaries and the frontend mirrors that
  // state via `provider:queue_state_changed` / `provider:queue_flushed`.
  // Zone 2 clears only when a matching `provider:item_event` upsert
  // carries `provider_item_id`, proving the provider echo arrived.
}

/**
 * Route `provider:session_died` to the matching pane's banner slot.
 * The wire-side row in the timeline (kind `notification` with
 * `meta.kind = "session_died"`) provides the historical trace; this
 * listener flips `pane.generalError` so the existing
 * `ProviderStatusBanner` Reconnect-button banner fires. If the process
 * dies before `provider:turn_started`, this listener also clears the
 * optimistic pending-send bridge; the triage router still owns active
 * turn cleanup by synthesizing the truncated `provider:turn_completed`.
 */
export function applySessionDied(evt: SessionDiedEvent): void {
  if (!evt?.threadId) return;
  projectSendResolved(evt.threadId, { error: true });
  const message = sessionDiedBannerMessage(evt);
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setSessionError(message);
  }
}

function sessionDiedBannerMessage(evt: SessionDiedEvent): string {
  const reason = (evt.reason ?? '').trim();
  const signal = (evt.signal ?? '').trim();
  const stderrTail = (evt.stderrTail ?? '').trim();
  let base = 'Provider session exited unexpectedly';
  if (reason) base = reason;
  else if (signal) base = `Provider session terminated by signal ${signal}`;
  else if (evt.exitCode) base = `Provider session exited with code ${evt.exitCode}`;
  // The stderr tail is the actual failure text for a process that died
  // without wire output (bad CLI flag, missing module) — surface it so
  // the banner is self-diagnosing instead of just "exited with code 1".
  return stderrTail ? `${base}: ${stderrTail}` : base;
}

/**
 * Route `provider:todo_update` to the matching pane. Updates the
 * Todos segment of the activity rail. Empty step arrays clear the
 * snapshot; an all-completed snapshot starts the auto-hide timer
 * inside `setLiveTodo`. Todo updates do NOT add a timeline row — the
 * snapshot lives only in pane state.
 */
export function applyTodoUpdate(evt: TodoUpdateEvent): void {
  if (!evt?.threadId) return;
  const steps = Array.isArray(evt.steps) ? evt.steps : [];
  for (const pane of ingestPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setLiveTodo(steps);
  }
}

