// Pure state machine for the "Ship Changes" drawer: a 3-step wizard that
// walks a user through Commit -> Push -> Create PR without leaving the chat
// surface.
//
// Design notes:
//   * The store holds *only* transitions and fields. All side effects
//     (GitCommit / GitPush / GitCreatePR calls, toast emission, status
//     refreshes) live in the drawer component. This keeps the store
//     trivially unit-testable.
//   * A single `phase` string drives UI. The test suite walks every
//     reachable transition plus every failure path, so invariants stay
//     encoded in tests, not in ad hoc UI branches.
//
// Phase map:
//   idle              initial; drawer closed
//   commit.review     user is editing subject/body before commit
//   commit.busy       GitCommit is in flight
//   commit.error      GitCommit rejected; surfaces .error, stays on review
//   commit.done       commit succeeded (or skipped because nothing to commit)
//   push.review       user is about to push; shows ahead count + branch
//   push.busy         GitPush is in flight
//   push.error        GitPush rejected; user can retry or skip ahead
//   push.done         push finished; upstream now tracks local branch
//   pr.review         user is editing PR title/body
//   pr.busy           GitCreatePR is in flight
//   pr.error          GitCreatePR rejected; user can retry
//   pr.done           PR URL available; wizard is finished
//
// Legal forward transitions are enforced by the mutation methods — bad
// sequences throw so the UI can't silently wander off the rail.
//
// `setStatus()` is called by the drawer whenever GetGitStatus returns a
// fresh snapshot. Based on that snapshot the machine will auto-advance past
// stages that have nothing to do (nothing to commit => skip commit phase,
// nothing to push => skip push phase, etc). This is tested explicitly in the
// "auto advance" test block.

import type { GitStatus, WorkspaceRef } from '../types/git';

export type ShipChangesPhase =
  | 'idle'
  | 'commit.review'
  | 'commit.busy'
  | 'commit.error'
  | 'commit.done'
  | 'push.review'
  | 'push.busy'
  | 'push.error'
  | 'push.done'
  | 'pr.review'
  | 'pr.busy'
  | 'pr.error'
  | 'pr.done';

export interface ShipChangesState {
  readonly phase: ShipChangesPhase;
  readonly status: GitStatus | null;
  /**
   * The pane's thread identity the wizard was opened for — a persisted row's
   * id, or a draft placeholder's synthetic one. Used ONLY to detect that the
   * pane moved under an open drawer; nothing is sent to the backend with it.
   */
  readonly identity: string | null;
  /** The checkout every step acts on. */
  readonly workspace: WorkspaceRef | null;
  readonly commitSubject: string;
  readonly commitBody: string;
  readonly commitSha: string | null;
  readonly prTitle: string;
  readonly prBody: string;
  readonly prDraft: boolean;
  readonly prUrl: string | null;
  readonly error: string | null;

  /** True when Commit is a valid next action for the current state. */
  readonly canCommit: boolean;
  /** True when Push is a valid next action for the current state. */
  readonly canPush: boolean;
  /** True when Create PR is a valid next action for the current state. */
  readonly canCreatePR: boolean;
  /** True when the wizard has produced a successful PR. */
  readonly finished: boolean;

  /**
   * Monotonic counter that increments each time the wizard is opened or
   * closed. Async operations (GitCommit/GitPush/GitCreatePR) capture this
   * value at start and compare against the current value before mutating
   * state; mismatches mean the user closed the drawer (or opened it on a
   * different thread) mid-flight and the stale result must be dropped
   * silently instead of throwing on an illegal transition.
   */
  readonly generation: number;

  /** Open the drawer for a pane's thread identity and checkout. Resets
   *  everything. */
  open(identity: string, workspace: WorkspaceRef): void;
  /** Close the drawer and reset to idle. */
  close(): void;
  /** Update the cached GitStatus and auto-advance past skippable stages. */
  setStatus(status: GitStatus): void;
  /** Seed commit subject + body from an external source (e.g. LLM draft). */
  seedCommit(subject: string, body: string): void;
  setCommitSubject(subject: string): void;
  setCommitBody(body: string): void;
  /** Start the Commit side effect. */
  beginCommit(): void;
  completeCommit(sha: string): void;
  failCommit(error: string): void;
  /** User clicked "Skip commit" — treated as nothing-to-commit. */
  skipCommit(): void;
  /** Start the Push side effect. */
  beginPush(): void;
  completePush(): void;
  failPush(error: string): void;
  skipPush(): void;
  /** Seed PR title + body (typically from commit subject/body). */
  seedPR(title: string, body: string): void;
  setPRTitle(title: string): void;
  setPRBody(body: string): void;
  setPRDraft(draft: boolean): void;
  /** Start the Create PR side effect. */
  beginCreatePR(): void;
  completeCreatePR(url: string): void;
  failCreatePR(error: string): void;
  /** Clear the current error and return to the matching review phase. */
  retry(): void;
}

function assertPhase(actual: ShipChangesPhase, allowed: ShipChangesPhase[]): void {
  if (!allowed.includes(actual)) {
    throw new Error(
      `shipChanges: illegal transition from ${actual}; expected one of ${allowed.join(', ')}`,
    );
  }
}

/**
 * Decide which review phase to start on given the current git status. If
 * there are uncommitted changes we start at commit.review; otherwise if the
 * branch is ahead of upstream we jump to push.review; otherwise if the
 * branch has an upstream and no open PR we jump to pr.review. If the lookup
 * failed, pr.review shows the failure instead of offering creation. If all
 * three are already done there's nothing to ship and we land on pr.done.
 */
function initialPhaseForStatus(status: GitStatus): ShipChangesPhase {
  if (status.hasChanges) return 'commit.review';
  if (status.aheadCount > 0) return 'push.review';
  if (status.hasUpstream && !status.openPrUrl) return 'pr.review';
  return 'pr.done';
}

export function createShipChangesState(): ShipChangesState {
  let phase: ShipChangesPhase = $state('idle');
  let status: GitStatus | null = $state(null);
  let identity: string | null = $state(null);
  let workspace: WorkspaceRef | null = $state(null);
  let commitSubject = $state('');
  let commitBody = $state('');
  let commitSha: string | null = $state(null);
  let prTitle = $state('');
  let prBody = $state('');
  let prDraft = $state(false);
  let prUrl: string | null = $state(null);
  let error: string | null = $state(null);
  // `generation` is intentionally NOT $state: it's only read in async
  // branches (never in templates or derived reactivity) so making it
  // reactive would cause spurious re-runs of any $effect that reads
  // wizard.generation. Bumping it must be synchronous and side-effect-free.
  let generation = 0;

  function reset(): void {
    phase = 'idle';
    status = null;
    identity = null;
    workspace = null;
    commitSubject = '';
    commitBody = '';
    commitSha = null;
    prTitle = '';
    prBody = '';
    prDraft = false;
    prUrl = null;
    error = null;
  }

  return {
    get phase() { return phase; },
    get status() { return status; },
    get identity() { return identity; },
    get workspace() { return workspace; },
    get commitSubject() { return commitSubject; },
    get commitBody() { return commitBody; },
    get commitSha() { return commitSha; },
    get prTitle() { return prTitle; },
    get prBody() { return prBody; },
    get prDraft() { return prDraft; },
    get prUrl() { return prUrl; },
    get error() { return error; },
    get generation() { return generation; },
    get canCommit() {
      // Commits are blocked while a merge/rebase/bisect is in progress —
      // forcing a new commit on top would compound the mess. The user
      // must finish or abort the pending op first.
      const pending = status?.pendingOperation ?? '';
      return phase === 'commit.review'
        && commitSubject.trim().length > 0
        && pending === '';
    },
    get canPush() {
      return phase === 'push.review';
    },
    get canCreatePR() {
      return phase === 'pr.review'
        && prTitle.trim().length > 0
        && (status?.openPrLookupError?.trim() ?? '') === '';
    },
    get finished() { return phase === 'pr.done'; },

    open(id, ws) {
      reset();
      identity = id;
      workspace = ws;
      // Bump the generation so any in-flight operation from a previous
      // open()/close() cycle is classified as stale when it resolves.
      generation += 1;
      // Caller will follow up with setStatus() once GetGitStatus returns;
      // we stay on idle until then so the UI can show a spinner.
    },

    close() {
      reset();
      // Same generation bump as open(): every async op captured the
      // pre-close value and must see a different value now.
      generation += 1;
    },

    setStatus(next) {
      status = next;
      // Only auto-advance from idle — once we're inside a flow the status
      // feed is informational and must not yank the user between stages.
      if (phase === 'idle') {
        phase = initialPhaseForStatus(next);
      }
    },

    seedCommit(subject, body) {
      commitSubject = subject;
      commitBody = body;
    },

    setCommitSubject(subject) { commitSubject = subject; },
    setCommitBody(body) { commitBody = body; },

    beginCommit() {
      assertPhase(phase, ['commit.review']);
      if (commitSubject.trim() === '') {
        throw new Error('shipChanges: cannot commit with an empty subject');
      }
      error = null;
      phase = 'commit.busy';
    },

    completeCommit(sha) {
      assertPhase(phase, ['commit.busy']);
      commitSha = sha;
      error = null;
      phase = 'commit.done';
      // Commit always flows into the push review screen. The drawer will
      // call setStatus() after a refresh so aheadCount reflects the new
      // commit, but even without that refresh "push.review" is safe because
      // completing a commit always advances the branch by >=1.
      phase = 'push.review';
    },

    failCommit(err) {
      assertPhase(phase, ['commit.busy']);
      error = err;
      phase = 'commit.error';
    },

    skipCommit() {
      assertPhase(phase, ['commit.review']);
      error = null;
      phase = 'push.review';
    },

    beginPush() {
      assertPhase(phase, ['push.review']);
      error = null;
      phase = 'push.busy';
    },

    completePush() {
      assertPhase(phase, ['push.busy']);
      error = null;
      phase = 'push.done';
      phase = 'pr.review';
    },

    failPush(err) {
      assertPhase(phase, ['push.busy']);
      error = err;
      phase = 'push.error';
    },

    skipPush() {
      assertPhase(phase, ['push.review']);
      error = null;
      phase = 'pr.review';
    },

    seedPR(title, body) {
      prTitle = title;
      prBody = body;
    },

    setPRTitle(title) { prTitle = title; },
    setPRBody(body) { prBody = body; },
    setPRDraft(draft) { prDraft = draft; },

    beginCreatePR() {
      assertPhase(phase, ['pr.review']);
      if (prTitle.trim() === '') {
        throw new Error('shipChanges: title is required');
      }
      if ((status?.openPrLookupError?.trim() ?? '') !== '') {
        throw new Error('shipChanges: cannot create PR while existing PR lookup failed');
      }
      error = null;
      phase = 'pr.busy';
    },

    completeCreatePR(url) {
      assertPhase(phase, ['pr.busy']);
      prUrl = url;
      error = null;
      phase = 'pr.done';
    },

    failCreatePR(err) {
      assertPhase(phase, ['pr.busy']);
      error = err;
      phase = 'pr.error';
    },

    retry() {
      switch (phase) {
        case 'commit.error':
          phase = 'commit.review';
          break;
        case 'push.error':
          phase = 'push.review';
          break;
        case 'pr.error':
          phase = 'pr.review';
          break;
        default:
          throw new Error(`shipChanges: retry() called from ${phase}; expected *.error`);
      }
      error = null;
    },
  };
}

// Exposed for tests that want to exercise the auto-advance logic directly
// without constructing a store.
export const _testing = { initialPhaseForStatus };
