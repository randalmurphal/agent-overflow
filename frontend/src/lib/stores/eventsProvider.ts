// Provider-lifecycle event domain: approvals, user-input requests, usage /
// rate-limit reporting, provider status + account probes, turn
// started/completed, session-died, subagent notifications, and todo
// updates. Fan-in target of events.ts's setupEventListeners.
import type {
  ApprovalEvent,
  ModelFallbackEvent,
  ProviderAccountEvent,
  TodoUpdateEvent,
  ProviderStatusEvent,
  SessionDiedEvent,
  SubagentNotificationEvent,
  TurnCompletedEvent,
  TurnStartedEvent,
  UsageEvent,
  UserInputEvent,
} from '../types/events';
import { setProviderAccount } from './accountInfo.svelte';
import { asProviderID } from '../types/providers';
import { invalidateProviderModels } from './providerModels.svelte';
import { iterPanes } from './panes.svelte';
import { recordProviderStatus } from './providerStatus.svelte';
import { setProviderRateLimits } from './rateLimitsInfo.svelte';
import { addToast } from './toast.svelte';
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
import { bumpUsageRefresh } from './usageRefresh.svelte';
import { patchThreadDurableStatus, syncLatestTurnCompleted, syncThreadActivity, updateThreadUsageCache } from './eventsThreadRows';

export function applyModelFallback(evt: ModelFallbackEvent): void {
  if (!evt?.threadId) return;
  for (const pane of iterPanes()) {
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
    for (const pane of iterPanes()) {
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
    for (const pane of iterPanes()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadApproval = pane.pendingApprovals.some((approval) => approval.requestId === evt.requestId);
      pane.removeApproval(evt.requestId);
      if (hadApproval && evt.action === 'fail' && evt.detail) {
        pane.setGeneralError(`Failed to respond to approval: ${evt.detail}`);
      }
    }
  }
}

export function applyUserInputEvent(evt: UserInputEvent): void {
  if (!evt) return;

  if (evt.action === 'request' && evt.request?.threadId) {
    projectUserInputRequest(evt.request.threadId, evt.request.requestId);
    for (const pane of iterPanes()) {
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
    for (const pane of iterPanes()) {
      if (evt.threadId && pane.threadId !== evt.threadId) continue;
      const hadRequest = pane.pendingUserInputs.some((request) => request.requestId === evt.requestId);
      pane.removeUserInput(evt.requestId);
      if (hadRequest && evt.action === 'fail' && evt.detail) {
        pane.setGeneralError(`Failed to submit input: ${evt.detail}`);
      }
    }
  }
}

export function applyUsageEvent(evt: UsageEvent): void {
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
    setProviderRateLimits(evt.rateLimits);
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

  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    if (payload) {
      pane.setContextWindow(payload);
    } else {
      pane.clearContextWindow();
    }
  }

  updateThreadUsageCache(
    evt.threadId,
    payload
      ? JSON.stringify({
          usedTokens: payload.usedTokens,
          maxTokens: payload.maxTokens,
          contextPercent: payload.usedPercentage,
          autoCompactPercent: payload.autoCompactPercent,
          autoCompactTokenLimit: payload.autoCompactTokenLimit,
          ...(payload.exceeded ? { exceeded: true } : {}),
        })
      : '',
  );
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
const KIND_TO_LEGACY_STATUS: Record<NonNullable<ProviderStatusEvent['kind']>, ProviderStatusEvent['status']> = {
  binary_missing: 'not_found',
  unauthenticated: 'unauthenticated',
  version_incompatible: 'version_too_old',
};

export function applyProviderStatus(evt: ProviderStatusEvent): void {
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
    invalidateProviderModels(provider);
  }

  const normalized: ProviderStatusEvent = { ...evt, provider, status: effectiveStatus };

  // Thread-scoped status belongs to matching panes only. Writing it into
  // the provider-global cache leaks one pane's auth/session failure into
  // every other pane using the same provider.
  if (!evt.threadId) {
    recordProviderStatus(normalized);
  }

  const banner = effectiveStatus === 'ready' ? null : normalized;
  for (const pane of iterPanes()) {
    if (pane.thread?.provider !== provider) continue;
    // Kind-bearing events can carry a threadId for per-pane scoping; when
    // present, only update the matching pane. Without a threadId the event
    // is provider-global (legacy behavior) and fans out to every matching
    // pane as before.
    if (evt.threadId && pane.threadId !== evt.threadId) continue;
    pane.setProviderBanner(evt.threadId ? banner : undefined);
  }
}

export function applyProviderAccount(evt: ProviderAccountEvent): void {
  if (!evt || typeof evt.account !== 'object' || evt.account === null) return;
  const provider = asProviderID(evt.provider);
  if (!provider) return;
  setProviderAccount(provider, evt.account, evt.accountId);
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
  for (const pane of iterPanes()) {
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
  for (const pane of iterPanes()) {
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
  for (const pane of iterPanes()) {
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
 * Route `provider:subagent_notification` to the matching pane. No UI
 * consumes this today; the pane records it in a bounded log so a future
 * tray / toast surface can subscribe without re-wiring the channel.
 */
export function applySubagentNotification(evt: SubagentNotificationEvent): void {
  if (!evt?.threadId) return;
  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.appendSubagentNotification(evt);
  }
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
  for (const pane of iterPanes()) {
    if (pane.threadId !== evt.threadId) continue;
    pane.setLiveTodo(steps);
  }
}

export interface DefaultSwappedPayload {
  from?: string;
  to?: string;
  fromCli?: string;
  otherCli?: string;
  reason?: string;
}

export function applyDefaultSwapped(payload: DefaultSwappedPayload): void {
  if (!payload || !payload.to) return;
  const next = payload.otherCli || payload.to;
  const prev = payload.fromCli || payload.from || 'previous default';
  addToast(
    'info',
    `Default provider switched to ${next} — ${prev} CLI not detected.`,
  );
}
