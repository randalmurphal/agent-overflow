// Per-pane live git-status slot. Owns the single gitwatch subscription for a
// thread's workspace (subscribe + retry/backoff + the `git:status` push), holds
// the observed GitStatus reactively, and persists an observed branch change
// back to the thread row. Both GitActionsControl (commit/push/ship) and the
// chat header's workspace-diff / PR badges read `status` from here, so there is
// exactly one subscription per pane rather than one per consumer.
//
// Lifecycle is driven by the always-mounted ChatHeaderActions: it calls
// attach() from a $effect whose deps are the thread id, the workspace cwd, and
// the transport-connected flag (all $derived, so value-equality suppresses the
// per-token re-subscribe flicker), and the returned cleanup unsubscribes.

import type { GitStatus } from '../types/git';
import type { Thread } from '../types/models';
import {
  GetGitStatus,
  GitStatusSubscribe,
  GitStatusUnsubscribe,
  UpdateThreadBranch,
  type GitStatusSubscriptionResult,
} from './bindings';
import { syncThread } from './panes.svelte';
import { wailsEventOn } from './wailsEvents';
import { errString } from '../utils/errors';

// Wire payload shape for "git:status" events. Wails doesn't generate a TS type
// for event payloads, so the shape is declared locally and kept in sync with
// GitStatusEvent in app_gitwatch.go. The slot subscribes to this event itself
// (wailsEventOn, from the leaf module) once a subscription is active.
interface GitStatusEvent {
  subscriptionId: string;
  status: GitStatus;
}

export interface GitStatusAttachOptions {
  /** Thread to subscribe / refresh against. */
  threadId: string | null;
  /** Workspace cwd (worktree dir for worktree threads); null disqualifies. */
  cwd: string | null;
  /** WS transport gate — subscription only runs while connected. */
  connected: boolean;
  /** Live thread snapshot, read at observe-time for branch reconciliation. */
  getThread: () => Thread | null;
  /** Live pane thread id, used to drop branch-persist errors after a switch. */
  getLiveThreadId: () => string | null;
  /** Surfaces a user-facing error (pane.setGeneralError). */
  reportError: (message: string) => void;
}

export interface GitStatusSlot {
  /** Latest observed status, or null before the first observe / when detached. */
  readonly status: GitStatus | null;
  /** True when the subscription is in a failed/retrying state. */
  readonly statusError: boolean;
  /** Pure setter — assigns status with no side effects (rendering, tests). */
  set(status: GitStatus | null): void;
  /** Pure setter for the error flag. */
  setError(value: boolean): void;
  /** Clears status + error (thread switch). */
  reset(): void;
  /** One-shot refresh used after git actions; reconciles the observed branch. */
  refreshNow(): Promise<void>;
  /** Starts/keeps the subscription for the given context; returns cleanup. */
  attach(options: GitStatusAttachOptions): () => void;
}

const INITIAL_RETRY_DELAY_MS = 3_000;
const MAX_RETRY_DELAY_MS = 30_000;

export function createGitStatusSlot(): GitStatusSlot {
  let status = $state<GitStatus | null>(null);
  let statusError = $state(false);

  // The most recent qualifying attach context. The git:status listener,
  // refreshNow(), and the branch-persist drain all reconcile against it; it is
  // cleared when the slot detaches or the workspace/thread/transport
  // disqualifies the subscription.
  let ctx: GitStatusAttachOptions | null = null;

  // Branch-persist queue. Observing a branch change writes it back to the
  // thread row so the workspace strip + sidebar reflect the real checkout.
  // Queued (not awaited inline) so a burst of branch flips collapses to the
  // latest value and never blocks status application.
  let queuedBranchPersist: { threadId: string; branch: string } | null = null;
  let branchPersistRunning = false;

  async function drainBranchPersistQueue(): Promise<void> {
    while (queuedBranchPersist !== null) {
      const next = queuedBranchPersist;
      queuedBranchPersist = null;
      try {
        await UpdateThreadBranch(next.threadId, next.branch);
      } catch (err) {
        console.error('Failed to persist observed git branch:', err);
        if (ctx?.getLiveThreadId() === next.threadId) {
          ctx.reportError(`Failed to update thread branch: ${errString(err)}`);
        }
      }
    }
    branchPersistRunning = false;
  }

  function persistObservedBranch(threadId: string, branch: string): void {
    queuedBranchPersist = { threadId, branch };
    if (branchPersistRunning) return;
    branchPersistRunning = true;
    void drainBranchPersistQueue();
  }

  // Applies an observed status and reconciles the thread's branch. Called only
  // from non-reactive paths (the git:status listener, attemptSubscribe,
  // refreshNow), so reading the live thread here never registers a $effect
  // dependency and cannot trigger a re-subscribe.
  function applyObservedStatus(nextStatus: GitStatus): void {
    status = nextStatus;
    if (!nextStatus.isRepo) return;
    const thread = ctx?.getThread() ?? null;
    if (!thread) return;
    const branch = nextStatus.branch ?? '';
    if ((thread.branch ?? '') === branch) return;

    syncThread({ ...thread, branch });
    persistObservedBranch(thread.id, branch);
  }

  return {
    get status() {
      return status;
    },
    get statusError() {
      return statusError;
    },

    set(next) {
      status = next;
    },
    setError(value) {
      statusError = value;
    },
    reset() {
      status = null;
      statusError = false;
    },

    async refreshNow() {
      const id = ctx?.threadId;
      if (!id) return;
      try {
        const result = (await GetGitStatus(id)) as GitStatus;
        // The await is a thread-switch window. If the slot detached or
        // re-pointed at another thread meanwhile, this result is stale:
        // applying it would paint the other thread's status and — since a
        // worktree thread's branch differs — persist THIS thread's branch onto
        // the new thread's row (durable corruption). Drop it. The subscribe
        // and git:status-listener paths guard the same way (cancelled flag /
        // subscriptionId match); refreshNow is the only other apply path.
        if (ctx?.threadId !== id) return;
        applyObservedStatus(result);
        statusError = false;
      } catch (err) {
        if (ctx?.threadId !== id) return;
        console.error('Failed to refresh git status:', err);
        statusError = true;
      }
    },

    attach(options) {
      const { threadId, cwd, connected } = options;
      if (!threadId || !cwd || !connected) {
        // Real disqualifiers: thread cleared, workspace gone, or transport
        // down. Nothing to subscribe; drop any stale status + context.
        ctx = null;
        status = null;
        statusError = false;
        return () => {};
      }
      ctx = options;

      let cancelled = false;
      let cancelEvent: (() => void) | null = null;
      let activeId: string | null = null;
      let retryTimer: ReturnType<typeof setTimeout> | null = null;
      let retryDelayMs = INITIAL_RETRY_DELAY_MS;

      const attemptSubscribe = async (): Promise<void> => {
        try {
          const result = (await GitStatusSubscribe(threadId)) as GitStatusSubscriptionResult;
          if (cancelled) {
            void GitStatusUnsubscribe(result.id).catch(() => undefined);
            return;
          }
          activeId = result.id;
          applyObservedStatus(result.status);
          statusError = false;
          retryDelayMs = INITIAL_RETRY_DELAY_MS;

          cancelEvent = wailsEventOn<GitStatusEvent>('git:status', (payload) => {
            if (!payload || payload.subscriptionId !== activeId) return;
            applyObservedStatus(payload.status);
          });
        } catch (err) {
          if (cancelled) return;
          console.error('GitStatusSubscribe failed:', err);
          statusError = true;
          retryTimer = setTimeout(() => {
            if (cancelled) return;
            retryTimer = null;
            void attemptSubscribe();
          }, retryDelayMs);
          retryDelayMs = Math.min(retryDelayMs * 2, MAX_RETRY_DELAY_MS);
        }
      };

      void attemptSubscribe();

      return () => {
        cancelled = true;
        if (retryTimer !== null) clearTimeout(retryTimer);
        if (cancelEvent) cancelEvent();
        if (activeId) {
          void GitStatusUnsubscribe(activeId).catch(() => undefined);
        }
        if (ctx === options) ctx = null;
      };
    },
  };
}
