import type { Thread } from '../types/models';
import { getSettings } from './settings.svelte';
import { generateWorktreeBranchName } from '../utils/worktreeBranchName';

export type WorktreeIntentMode = 'local' | 'new-worktree';

// Sentinel `newBranchBase` value meaning "branch off the current branch
// AND carry the source workspace's uncommitted changes into the new
// worktree". Distinct from picking the current branch by name (which
// is a clean checkout from that branch's tip).
//
// Prefer the `isLocalBase` / `resolveBaseForWire` helpers below over
// raw string comparisons so the comparison and the wire mapping stay
// centralized — drift is the failure mode this constant is most
// vulnerable to.
export const LOCAL_BASE_SENTINEL = '__LOCAL__';

// Typed predicate for the sentinel.
export function isLocalBase(value: string | undefined | null): boolean {
  return value === LOCAL_BASE_SENTINEL;
}

// Maps a UI base selection to the (baseBranch, carryLocalChanges) pair
// the backend bindings expect. The sentinel resolves to the thread's
// current branch with carry=true; anything else passes through with
// carry=false.
export function resolveBaseForWire(
  base: string,
  currentBranch: string,
): { baseBranch: string; carryLocalChanges: boolean } {
  if (isLocalBase(base)) {
    return { baseBranch: currentBranch, carryLocalChanges: true };
  }
  return { baseBranch: base, carryLocalChanges: false };
}

// Per-thread quadrant: (workspace mode) × (creating a new branch?).
//
//   mode='local'         creatingBranch=false → no special intent (checkout existing in current workspace)
//   mode='local'         creatingBranch=true  → create branch off newBranchBase, checkout in current workspace
//   mode='new-worktree'  creatingBranch=false → create worktree pointing at attachBranch (existing branch)
//   mode='new-worktree'  creatingBranch=true  → create branch off newBranchBase, then worktree pointing at it
//
// `carryLocalChanges` is intentionally NOT a stored field. It is
// always `isLocalBase(newBranchBase)` — derived at the wire boundary
// via resolveBaseForWire so the (sentinel ↔ carry=true) invariant is
// structurally impossible to drift.
export interface WorktreeIntent {
  mode: WorktreeIntentMode;
  creatingBranch: boolean;
  newBranchName: string;
  newBranchBase: string;
  attachBranch: string;
  /**
   * What the staged choice actually produced on disk, once it has been
   * applied. `worktreePath` is empty for a branch-only apply (the branch
   * was checked out in the workspace the pane is already in).
   *
   * Its presence is what makes an apply IDEMPOTENT: a second confirm
   * click, or a send racing the confirm button, returns this instead of
   * cutting a second worktree. It is also what the pane's workspace/branch
   * surfaces read through the `effective*` helpers below while the choice
   * is applied but not yet bound to a thread row — a draft placeholder has
   * no row to write to, and a materialized row is not moved until the send
   * commits to it.
   */
  applied: AppliedWorkspace | null;
}

/** What a project-scoped workspace RPC hands back (ProjectWorkspaceResult). */
export interface AppliedWorkspace {
  worktreePath: string;
  branch: string;
}

const LOCAL_INTENT: WorktreeIntent = {
  mode: 'local',
  creatingBranch: false,
  newBranchName: '',
  newBranchBase: '',
  attachBranch: '',
  applied: null,
};

// The (mode × creatingBranch) pair is the QUADRANT — the thing an apply
// answers. Editing the branch name or the base within a quadrant refines
// the same request; moving between quadrants asks for something else, and
// whatever a previous apply produced no longer describes it.
function carryApplied(
  current: WorktreeIntent,
  mode: WorktreeIntentMode,
  creatingBranch: boolean,
): AppliedWorkspace | null {
  if (current.mode !== mode || current.creatingBranch !== creatingBranch) return null;
  return current.applied;
}

let intents: Map<string, WorktreeIntent> = $state(new Map());

export function worktreeIntentForThread(thread: Thread | null | undefined): WorktreeIntent {
  if (!thread) return LOCAL_INTENT;
  return intents.get(thread.id) ?? LOCAL_INTENT;
}

// Seeded once at draft creation time. When the user has the
// "default to worktree" setting we pre-stage a new branch so the
// workflow continues unchanged — the user can still toggle off
// before sending.
export function seedDefaultWorktreeIntentForDraft(thread: Thread): void {
  if (thread.worktreePath || intents.has(thread.id)) return;
  if (getSettings().defaultThreadEnvMode !== 'worktree') return;
  intents = new Map(intents).set(thread.id, {
    mode: 'new-worktree',
    creatingBranch: true,
    newBranchName: generateWorktreeBranchName(getSettings().worktreeBranchPrefix),
    newBranchBase: thread.branch ?? '',
    attachBranch: '',
    applied: null,
  });
}

// User flipped the workspace selector. Toggling INTO new-worktree
// leaves creatingBranch=false: the user picks an existing branch from
// BranchPicker, or clicks "+ new branch" to opt into creating one.
export function setThreadEnvMode(thread: Thread, mode: WorktreeIntentMode): void {
  const current = worktreeIntentForThread(thread);
  const next = new Map(intents);
  if (mode === 'local') {
    next.set(thread.id, {
      ...LOCAL_INTENT,
      applied: carryApplied(current, 'local', false),
    });
  } else {
    next.set(thread.id, {
      mode: 'new-worktree',
      creatingBranch: false,
      newBranchName: '',
      newBranchBase: '',
      attachBranch: '',
      applied: carryApplied(current, 'new-worktree', false),
    });
  }
  intents = next;
}

// User clicked "+ new branch" (either inline button in new-worktree
// mode, or "+ New branch…" inside the BranchPicker dropdown). Default
// base mirrors the destructive-default convention: dirty workspace
// pre-selects the LOCAL sentinel so checkout-style data loss is opt-in.
export function enterCreateBranchMode(
  thread: Thread,
  opts: { workspaceDirty: boolean; currentBranch: string },
): void {
  const current = worktreeIntentForThread(thread);
  const baseFallback = opts.workspaceDirty ? LOCAL_BASE_SENTINEL : opts.currentBranch;
  // Auto-fill the name in worktree mode (matches today's seed flow);
  // leave it blank in local mode so the input starts empty for the
  // user to type.
  const seedName =
    current.mode === 'new-worktree'
      ? generateWorktreeBranchName(getSettings().worktreeBranchPrefix)
      : '';
  intents = new Map(intents).set(thread.id, {
    ...current,
    creatingBranch: true,
    newBranchName: current.newBranchName || seedName,
    newBranchBase: current.newBranchBase || baseFallback,
    attachBranch: '',
    applied: carryApplied(current, current.mode, true),
  });
}

// User cancelled the inline new-branch UI. Drops creatingBranch + the
// associated fields. Workspace mode (local vs new-worktree) survives
// so the user is back to "pick existing branch" in whichever mode they
// were in.
export function exitCreateBranchMode(thread: Thread): void {
  const current = worktreeIntentForThread(thread);
  intents = new Map(intents).set(thread.id, {
    ...current,
    creatingBranch: false,
    newBranchName: '',
    newBranchBase: '',
    applied: carryApplied(current, current.mode, false),
  });
}

export function setNewBranchName(thread: Thread, name: string): void {
  const current = worktreeIntentForThread(thread);
  if (!current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    newBranchName: name,
  });
}

export function setNewBranchBase(thread: Thread, base: string): void {
  const current = worktreeIntentForThread(thread);
  if (!current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    newBranchBase: base,
  });
}

// User picked an existing branch from BranchPicker while in
// new-worktree + !creatingBranch mode — stages it as the worktree's
// target. No-op outside that quadrant; callers handle dedup separately
// (a branch that already has a worktree should flip mode='local'
// instead of staging an attach).
export function setAttachBranch(thread: Thread, branch: string): void {
  const current = worktreeIntentForThread(thread);
  if (current.mode !== 'new-worktree' || current.creatingBranch) return;
  intents = new Map(intents).set(thread.id, {
    ...current,
    attachBranch: branch,
  });
}

/** Is there a workspace choice staged at all? The apply's whole precondition. */
export function isStagedWorktreeIntent(intent: WorktreeIntent): boolean {
  return intent.mode === 'new-worktree' || intent.creatingBranch;
}

/**
 * Synchronous "would a send have to apply anything?" read. The send path uses
 * it as a gate so an unstaged send does not pick up an extra microtask on the
 * way to the wire — the mid-turn queue path is ordering-sensitive.
 */
export function hasStagedWorktreeIntent(thread: Thread | null | undefined): boolean {
  return isStagedWorktreeIntent(worktreeIntentForThread(thread));
}

/**
 * Record what a project-scoped apply produced. The staged quadrant is left
 * alone: the pickers keep showing the choice the user made, and the apply
 * path short-circuits on this instead of running a second time.
 */
export function markWorktreeIntentApplied(threadId: string, applied: AppliedWorkspace): void {
  const current = intents.get(threadId);
  if (!current) return;
  intents = new Map(intents).set(threadId, { ...current, applied });
}

/**
 * The workspace the pane is effectively parked in — the applied worktree
 * when one exists, the thread's own field otherwise.
 *
 * A draft placeholder has its own object stamped at apply time, so this
 * agrees with `thread.workspacePath` there. A MATERIALIZED row deliberately
 * is not moved until the send binds it (backend row syncs would fight a
 * local stamp), and this is what keeps the workspace strip, the branch
 * picker and anything else reading the pane's location truthful in the
 * window between the confirm click and the send.
 */
export function effectiveWorkspacePathForThread(thread: Thread | null | undefined): string {
  if (!thread) return '';
  const applied = worktreeIntentForThread(thread).applied;
  return applied?.worktreePath || thread.workspacePath || '';
}

/** Companion of `effectiveWorkspacePathForThread` for the branch. */
export function effectiveBranchForThread(thread: Thread | null | undefined): string {
  if (!thread) return '';
  const applied = worktreeIntentForThread(thread).applied;
  return applied?.branch || thread.branch || '';
}

export function clearWorktreeIntent(threadId: string): void {
  if (!intents.has(threadId)) return;
  const next = new Map(intents);
  next.delete(threadId);
  intents = next;
}

// Threads with a workspace RPC in flight against their REAL row (the
// bind step: UpdateThreadWorkspace, or the thread-scoped engine the
// proposed-plan flow still runs). Reactive so the composer's empty-draft
// cleanup treats it as active work — deleting the row mid-RPC is what used
// to surface as "no rows in result set".
//
// Deliberately NOT set on the draft-placeholder path: a placeholder has no
// row for the cleanup to delete, and the whole point of the project-scoped
// apply is that nothing has to be materialized to run it. The mark must be
// set synchronously, before the first await of the function that brackets
// it, or the cleanup gets a window in which the RPC is live and the flag
// is not.
let applyingThreadIds: ReadonlySet<string> = $state(new Set());

export function markWorktreeIntentApplying(threadId: string, applying: boolean): void {
  if (threadId === '' || applyingThreadIds.has(threadId) === applying) return;
  const next = new Set(applyingThreadIds);
  if (applying) next.add(threadId);
  else next.delete(threadId);
  applyingThreadIds = next;
}

export function isWorktreeIntentApplying(threadId: string | null | undefined): boolean {
  return threadId != null && applyingThreadIds.has(threadId);
}

/**
 * Anything else keyed by the same thread id that has to follow a re-key.
 *
 * The apply engine's in-flight promise maps are the users
 * (worktreeIntentMaterialize): an apply that outlives a migration must still be
 * FOUND under the id the pane holds now, or the next send starts a second one
 * and cuts the same branch twice. Those maps cannot live here — the engine
 * imports this store, not the other way round — so the direction is inverted
 * into a hook the engine arms at module init. Nothing to arm means nothing in
 * flight: a migration reaching this module before the engine was ever imported
 * has no promise to re-key.
 */
type WorktreeIntentMigrationHook = (fromThreadId: string, toThreadId: string) => void;

const migrationHooks: WorktreeIntentMigrationHook[] = [];

export function onWorktreeIntentMigrated(hook: WorktreeIntentMigrationHook): void {
  migrationHooks.push(hook);
}

// Re-key a draft placeholder's intent under its newly-materialized
// thread id. Called from ThreadPane.ensureMaterializedThread after
// CreateThread returns so worktree/branch picks made on the placeholder
// (keyed by the synthetic placeholder id) survive into the real row, and from
// dematerializeEmptyDraftThread in the other direction.
export function migrateWorktreeIntent(fromThreadId: string, toThreadId: string): void {
  if (fromThreadId === toThreadId) return;
  // Hooks run unconditionally: an in-flight apply outlives the intent entry it
  // was launched from (an intent can be cleared mid-RPC), and it is exactly
  // that promise the next send must still find.
  for (const hook of migrationHooks) hook(fromThreadId, toThreadId);
  const existing = intents.get(fromThreadId);
  if (!existing) return;
  const next = new Map(intents);
  next.delete(fromThreadId);
  next.set(toThreadId, existing);
  intents = next;
}

export function resetForTest(): void {
  intents = new Map();
  applyingThreadIds = new Set();
}
