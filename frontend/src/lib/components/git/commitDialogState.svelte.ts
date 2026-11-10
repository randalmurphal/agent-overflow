import type { CommitFile, CommitSelection, GitActionResult, WorkspaceRef } from '../../types/git';
import { GenerateCommitMessage, GitCommit, ListCommitFiles } from '../../stores/bindings';
import { createRefreshScheduler, type RefreshToken } from '../../utils/refreshScheduler';
import { userFacingError } from '../../utils/userFacingError';
import { isMethodUnavailableError } from '../../stores/transportStatus.svelte';
import { carryActive, carrySelection, commitSelection, toggleAll, toggleOne } from './commitFiles';

// The state of one opening of the commit dialog on one checkout: the file
// list as the backend last answered it, which rows are selected, which row
// is active, and the commit and generate calls made against them. The
// dialog makes one per opening and disposes it on close, so nothing here
// outlives the dialog and no field needs resetting.
//
// Selection is by path and starts empty: nothing is committed that was not
// picked, and a file that appears while the dialog is open arrives
// unselected.

export interface CommitDialogState {
  readonly files: readonly CommitFile[];
  /** Changed files before the list's bound; more than `files` when it was cut. */
  readonly total: number;
  /** Git records only a commit of everything: every row is in, none toggles. */
  readonly wholeOnly: boolean;
  /** The list has been answered at least once. */
  readonly loaded: boolean;
  readonly listError: string;
  readonly selected: ReadonlySet<string>;
  readonly active: string | null;
  /** What Commit and Generate cover, or null while nothing is selected. */
  readonly selection: CommitSelection | null;
  readonly committing: boolean;
  readonly generating: boolean;
  /** Why the last commit attempt was refused. */
  readonly commitError: string | null;

  /** Lists the files again. Flood-safe; the first call runs at once. */
  refresh(): void;
  activate(path: string): void;
  toggle(path: string): void;
  toggleAll(): void;
  /** Commits the selection. Resolves to the new commit's SHA, or null when
   * nothing was committed (`commitError` says why). */
  commit(subject: string, body: string): Promise<string | null>;
  /** Drafts a message for the selection. Rejects when the draft fails. */
  generate(): Promise<{ subject: string; body: string } | null>;
  dispose(): void;
}

/**
 * @param onEmptied runs when a commit made here left nothing to commit.
 */
export function createCommitDialogState(workspace: WorkspaceRef, onEmptied: () => void): CommitDialogState {
  let files = $state.raw<readonly CommitFile[]>([]);
  let total = $state(0);
  let wholeOnly = $state(false);
  let loaded = $state(false);
  let listError = $state('');
  let selected = $state.raw<ReadonlySet<string>>(new Set());
  let active = $state<string | null>(null);
  let committing = $state(false);
  let generating = $state(false);
  let commitError = $state<string | null>(null);
  let committedHere = false;
  let requested = false;

  const selection = $derived(commitSelection(files, selected, wholeOnly));

  const scheduler = createRefreshScheduler({
    name: 'CommitDialog',
    delayMs: 250,
    maxWaitMs: 1_000,
    // A listing snapshots the working tree, which costs with the checkout's
    // size; an agent editing while the dialog is open asks for one per edit.
    minIntervalMs: 1_000,
    run: list,
  });

  async function list(token: RefreshToken): Promise<void> {
    try {
      const listed = await ListCommitFiles(workspace);
      if (!token.isCurrent()) return;
      const next = listed.files ?? [];
      active = carryActive(active, files, next);
      selected = carrySelection(selected, files, next);
      files = next;
      total = listed.total;
      wholeOnly = listed.wholeOnly;
      listError = '';
      loaded = true;
      if (committedHere && next.length === 0) onEmptied();
    } catch (err) {
      if (!token.isCurrent()) return;
      // A connected computer on a build older than the file list.
      listError = isMethodUnavailableError(err)
        ? 'Update Agent Overflow on this computer to commit from here.'
        : userFacingError(err, 'Could not list the changed files.');
    }
  }

  function refresh(immediate = false): void {
    scheduler.request({ immediate: immediate || !requested });
    requested = true;
  }

  return {
    get files() { return files; },
    get total() { return total; },
    get wholeOnly() { return wholeOnly; },
    get loaded() { return loaded; },
    get listError() { return listError; },
    get selected() { return selected; },
    get active() { return active; },
    get selection() { return selection; },
    get committing() { return committing; },
    get generating() { return generating; },
    get commitError() { return commitError; },

    refresh: () => refresh(),
    activate(path) { active = path; },
    toggle(path) {
      if (!wholeOnly) selected = toggleOne(selected, path);
    },
    toggleAll() {
      if (!wholeOnly) selected = toggleAll(selected, files);
    },

    async commit(subject, body) {
      const covered = selection;
      if (!covered || committing || subject.trim() === '') return null;
      committing = true;
      commitError = null;
      try {
        const result = await GitCommit(workspace, subject.trim(), body.trim(), covered) as GitActionResult;
        if (result.error) {
          commitError = result.error;
          return null;
        }
        selected = new Set();
        committedHere = true;
        return result.commitSha ?? '';
      } catch (err) {
        commitError = err instanceof Error ? err.message : String(err);
        return null;
      } finally {
        committing = false;
        // The commit, or its refusal, changed what there is to list.
        refresh(true);
      }
    },

    async generate() {
      const covered = selection;
      if (!covered || generating || committing) return null;
      generating = true;
      try {
        const message = await GenerateCommitMessage(workspace, covered);
        return { subject: message.subject ?? '', body: message.body ?? '' };
      } finally {
        generating = false;
      }
    },

    dispose: () => scheduler.dispose(),
  };
}
