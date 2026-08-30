// Whether a WORKSPACE can be changed right now.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): state is keyed by its
// ENTITY. The store is keyed by DIRECTORY, and it answers TWO questions off
// the one fetch, because the affordances it gates mutate two different
// entities:
//
//   - `locked` / `reason`: "would mutating THIS CHECKOUT break something
//     running in it?" Remove Worktree runs `git worktree remove` on the
//     directory; a local branch create moves HEAD under every thread in it.
//     Two threads sharing one worktree is first-class (project-root threads
//     default to it, and "implement this plan in a new thread" inherits the
//     source worktree), so a thread-keyed lock left the destructive action
//     live while a SIBLING thread's agent was writing into the very directory
//     being deleted. Any busy thread in the directory locks this.
//   - `threadLocked` / `threadReason`: "is THIS THREAD running?" Moving a
//     thread to another checkout (env picker, new-worktree confirm) rewrites
//     only that thread's row and transcript slug; a sibling working in the
//     directory it leaves is unaffected. Gating this on the directory answer
//     froze every idle thread at the project root for as long as any one
//     thread was responding. The backend's own gate for these RPCs is
//     ensureWorkspaceChangeAllowed(threadID), thread-keyed, and the
//     affordance must not be stricter than the refusal. Only this pane's
//     thread locks this, resolved from the same payload's busyThreads.
//
// Error posture is FAIL-SAFE. These gate irreversible actions. A failed
// GetWorkspaceActivity means we do not KNOW whether anything is running, and
// "we do not know" must read as locked with a visible reason — the original
// code logged the failure and resolved to zero running tasks, which reads as
// "safe to delete".
//
// The backend answer is deliberately the same computation the removal gate
// performs while holding the thread locks (App.GetWorkspaceActivity and
// removeProjectWorktree share threadsReferencingWorkspace's path matching and
// threadActivityBlockReason's activity reads), so the affordance and the
// refusal cannot disagree. It covers BOTH legs of "busy" — open turns and
// live background tasks — across every thread in the directory.
//
// EVENT ROUTING: refresh every live key on any activity event, rather than
// enriching the wire payload with a workspacePath and routing precisely. The
// events are correctly THREAD-keyed (a background task belongs to a thread;
// the workspace is a derived fact), and the frontend cannot map an arbitrary
// thread id to a workspace — the busy thread need not be mounted in any pane.
// Enriching at the emit site would mean a store read on a hot triage emit
// path, and its failure mode is silent: a payload that lost its workspacePath
// routes nowhere and the lock reads UNLOCKED over running work, which is the
// bug this file exists to close. A blind refresh has no such mode, and the
// cost is bounded by the number of distinct workspaces on screen — a handful
// — each behind its own rate-bounded scheduler.
//
// That scheduler (`utils/refreshScheduler`) replaced a trailing debounce, and
// the difference is a safety property here rather than a tuning one: a trailing
// debounce restarts its timer on every event, so a workspace under a streaming
// thread — precisely the workspace whose lock matters — postponed its re-check
// indefinitely and the destructive affordances kept reading whatever the last
// answer said. The absolute deadline is what makes "the lock is at most
// REFRESH_MAX_WAIT_MS stale" a statement that survives a flood.

import type { ThreadPane } from './thread.svelte';
import { GetWorkspaceActivity, type WorkspaceActivity } from './bindings';
// Imported from the leaf that OWNS the fan-out, not from the `events.ts`
// composition root: this module is loaded by the test setup (it holds a
// module-level store that has to be reset between tests), and pulling the
// whole event graph in there would evaluate every handler module before a
// suite's own `vi.mock` calls register.
import { onItemUpsert } from './eventsItemStream';
import { wailsEventOn } from './wailsEvents';
import { getActiveTurn } from './threadStatuses.svelte';
import { createEntityStore } from './entityStore.svelte';
import { isMethodUnavailableError } from './transportStatus.svelte';
import { createRefreshScheduler } from '../utils/refreshScheduler';
import { workspaceKeyForThread } from '../utils/workspaceKey';

// Item-stream events fire per wire round — several per second while any pane
// streams — and every live key answers each one. 100ms collapses that burst;
// 400ms is the hard ceiling on how stale a lock gating an irreversible action
// may be, and unlike the debounce it replaced it holds under a stream that
// never pauses.
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

const TURN_REASON = 'Workspace changes are unavailable while the agent is responding.';
const TASKS_REASON = 'Workspace changes are unavailable while background tasks are running.';
const CHECKING_REASON = 'Checking workspace availability...';
// GetWorkspaceActivity is loopback-only, so a remote session can never
// verify a workspace and never will — the refusal is a permanent property
// of the session, not a failure to report. Unverified stays LOCKED; only
// the reason changes, because "method not registered" reads as a broken
// app rather than as the remote posture every other local-only affordance
// already states (workflows UI-SPEC §10).
const LOCAL_ONLY_REASON = 'Workspace changes are only available on the local machine.';

function activityError(err: unknown): unknown {
  return isMethodUnavailableError(err) ? new Error(LOCAL_ONLY_REASON) : err;
}

// Value is the workspace's live activity. `null` (no observation yet) and an
// error both mean "unverified", which is locked.
const store = createEntityStore<WorkspaceActivity, void>({
  name: 'workspaceChangeLock',
  source: async ({ key, apply, fail, signal }) => {
    // Responses used to be able to overtake each other: the initial load and
    // every debounced event refresh were separate RPCs on ONE entity
    // generation, and an older IDLE answer landing after a newer BUSY one
    // briefly unlocks `rm -rf` over a sibling thread's live agent. The
    // scheduler removes the race rather than stamping it — runs are
    // serialized, so two answers to this question are never in flight
    // together — and its token still covers the remaining window (a run
    // superseded by a teardown must not apply, in either direction: a stale
    // FAILURE overwriting a fresh answer is the same defect, fail-safe or not).
    const refresh = createRefreshScheduler({
      name: `workspaceChangeLock(${key})`,
      delayMs: REFRESH_DELAY_MS,
      maxWaitMs: REFRESH_MAX_WAIT_MS,
      run: async (token) => {
        try {
          const activity = (await GetWorkspaceActivity(key)) as WorkspaceActivity;
          if (token.isCurrent()) apply(activity);
        } catch (err) {
          // fail() is the whole recovery: the lock reads as blocked
          // immediately and the primitive schedules a backed-off re-source.
          // Adding an invalidate() here reset that curve on every inbound
          // event, so a broken backend under a streaming thread never backed
          // off — it just re-polled forever at the event rate.
          if (token.isCurrent()) fail(activityError(err));
        }
      },
    });

    // None of these filter on the event's threadId: the busy thread may be
    // one this client has never mounted, so there is nothing to compare a
    // workspace key against. See EVENT ROUTING in the header.
    const cancels = [
      onItemUpsert((item) => {
        if (item.isBackground || item.completionOf) refresh.request();
      }),
      // A turn opening or closing in ANY thread can flip a workspace's lock:
      // the local pane's own turn is covered synchronously below, but a
      // sibling thread's is only visible through these.
      wailsEventOn('provider:turn_started', () => refresh.request()),
      wailsEventOn('provider:turn_completed', () => refresh.request()),
      wailsEventOn('provider:background_tasks_changed', () => refresh.request()),
      // Background-task state events fire on host-process exit
      // (state=exited) and on agent-observation drain (state=drained). Both
      // transitions can flip the lock if a backgrounded task drops out of
      // the live set.
      wailsEventOn('provider:background_task_state', () => refresh.request()),
    ];
    let released = false;
    const cleanup = (): void => {
      if (released) return;
      released = true;
      refresh.dispose();
      for (const cancel of cancels) cancel();
    };
    // The primitive has nothing to release until this run RETURNS a cleanup,
    // so the abort hook is what frees the listeners when the entry dies in
    // that window — otherwise a dead run keeps refreshing beside its
    // replacement's.
    signal.addEventListener('abort', cleanup);

    // The initial check goes through the SAME scheduler as every event
    // refresh, so it cannot race one, and the source hands back its cleanup
    // immediately rather than parking on the first RPC. A failed initial check
    // therefore arrives as fail() instead of as a throw — which is the better
    // of the two here: fail() keeps the acquired listeners and gets the same
    // backed-off re-source, where a throw had to release them first and left
    // the retry curve as the only way back to a live subscription.
    refresh.request({ immediate: true });
    return cleanup;
  },
});

// The primitive's transport edge does the right thing here for free: a
// disconnect suspends, which leaves every lock in its unverified — locked —
// state, and nothing can be verified over a dead wire anyway.

export interface WorkspaceChangeLockState {
  /** The DIRECTORY is busy: any thread in it is responding or has live
   *  background tasks. Gates destructive in-place operations. */
  readonly locked: boolean;
  readonly reason: string;
  /** THIS pane's thread is busy. Gates moving the thread to another
   *  checkout. Implied by `locked` being false; stricter never. */
  readonly threadLocked: boolean;
  readonly threadReason: string;
  readonly runningBackgroundCount: number;
  /** Re-check now. No-op when the pane holds no thread row. */
  refresh(): void;
}

/**
 * A read view over the shared per-workspace lock, resolving its key on every
 * read so a thread switch re-points it with no bookkeeping. Attaching is
 * refcounted, so both consumers on a pane — and every other pane on the same
 * worktree — share one fetch and one debounce.
 */
export function createWorkspaceChangeLockState(
  getPane: () => ThreadPane,
): WorkspaceChangeLockState {
  // $derived, not a plain function, and that is load-bearing: it memoizes by
  // VALUE, so a pane switching to another thread in the same worktree
  // produces the same string and the attach effect below is never
  // invalidated. Reading `pane.thread` inside the effect directly would drop
  // the shared entry to refcount zero and re-acquire it on every thread
  // switch — the exact anti-pattern frontend/CLAUDE.md names under "Key an
  // attach $effect on the ENTITY KEY alone".
  const workspaceKey = $derived.by((): string => {
    const pane = getPane();
    // DRAFT PLACEHOLDERS DO NOT ATTACH, and that is about what they can do,
    // not about the synthetic `draft:…` id — a placeholder carries a real
    // workspacePath, so keying on the path alone WOULD attach it. Its env /
    // worktree-name pickers only STAGE where a not-yet-created thread will
    // run: nothing is written, no session exists, and no directory is
    // touched, so a busy sibling in the project root is no reason to refuse
    // the choice. The one destructive affordance a draft reaches — the
    // picker's per-row worktree trash — is gated by the backend's own
    // `deleteBlocked` per row and by RemoveOtherWorktreeForProject's
    // refusal, never by this lock.
    if (!pane.threadId) return '';
    return workspaceKeyForThread(pane.thread) ?? '';
  });

  $effect(() => {
    if (!workspaceKey) return;
    const handle = store.attach(workspaceKey, undefined);
    return () => handle.release();
  });

  // The pane's OWN turn, read synchronously from local state. The backend
  // answer covers it too (and covers every sibling thread's), but only after
  // an event round trip plus the debounce — a window in which the user could
  // still click a destructive action on a turn they just started. Locking on
  // local state closes it at zero cost; the backend leg is what makes the
  // lock truthful about threads this pane cannot see.
  const localTurnActive = (): boolean => getActiveTurn(getPane().threadId) !== null;

  // Both views share the unverified / failed / local-only legs: a fetch that
  // has not answered (or cannot) says nothing about either entity, and
  // fail-safe means locked for both. Only the verified branch diverges.
  const unverifiedReason = (key: string): string | null => {
    const error = store.peekError(key);
    if (error === LOCAL_ONLY_REASON) return error;
    if (error !== null) return `Cannot check for running background tasks: ${error}`;
    if (store.peek(key) === null) return CHECKING_REASON;
    return null;
  };
  const ownThreadReason = (activity: WorkspaceActivity): string => {
    const threadId = getPane().threadId;
    const own = activity.busyThreads.find((t) => t.threadId === threadId);
    if (!own) return '';
    if (own.activeTurn) return TURN_REASON;
    return own.runningBackgroundTasks > 0 ? TASKS_REASON : '';
  };

  const reason = (): string => {
    if (localTurnActive()) return TURN_REASON;
    const key = workspaceKey;
    if (!key) return '';
    const unverified = unverifiedReason(key);
    if (unverified !== null) return unverified;
    const activity = store.peek(key)!;
    if (activity.activeTurnThreads > 0) return TURN_REASON;
    return activity.runningBackgroundTasks > 0 ? TASKS_REASON : '';
  };
  const threadReason = (): string => {
    if (localTurnActive()) return TURN_REASON;
    const key = workspaceKey;
    if (!key) return '';
    const unverified = unverifiedReason(key);
    if (unverified !== null) return unverified;
    return ownThreadReason(store.peek(key)!);
  };

  return {
    get locked() {
      return reason() !== '';
    },
    get reason() {
      return reason();
    },
    get threadLocked() {
      return threadReason() !== '';
    },
    get threadReason() {
      return threadReason();
    },
    get runningBackgroundCount() {
      return workspaceKey ? (store.peek(workspaceKey)?.runningBackgroundTasks ?? 0) : 0;
    },
    refresh() {
      if (workspaceKey) store.invalidate(workspaceKey);
    },
  };
}

/**
 * Diagnostics / tests: the workspaces currently held. Both consumers create
 * their view inside a component, so an unmount that failed to release would
 * pin a workspace's listeners and its debounce for the app's lifetime — the
 * leak is only visible from here.
 */
export function workspaceChangeLockKeys(): string[] {
  return store.keys();
}

/** Test seam: drop every entry, as a fresh module load would. */
export function __resetWorkspaceChangeLockForTest(): void {
  store.suspend();
  store.resetAll();
}
