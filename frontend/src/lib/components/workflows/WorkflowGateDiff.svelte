<script lang="ts">
  import { projectHasScope } from '../../transport/entityScopes';
  // Gate / done evidence: the changed-file list, expanding hunks in place, and
  // the hand-off to the real ReviewPane (UI-SPEC §4.3, §4.7). There is no
  // parallel diff renderer here — the file list reuses WorkflowDiff, and
  // "Open full review" opens the review companion on the phase's own thread,
  // which closes the overlay (R3).
  //
  // The whole patch is read once into compact storage and each file's lines
  // are materialized on expand, so opening three files costs one read, not
  // four. The read can hold the diff open to read evicted text again; it is
  // disposed with its files, and read again when that text is lost.

  import { onDestroy } from 'svelte';
  import WorkflowDiff from './WorkflowDiff.svelte';
  import type { PatchFile } from '../../utils/patchFiles';
  import { patchFileFromReviewFile, type ReviewFile } from '../../utils/patchStore';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { OpenBranchBaseDiff } from '../../stores/bindings';
  import { ReviewDiffSource, type ReviewDiffRead } from '../../stores/reviewDiffStream';
  import type { WorkspaceRef } from '../../types/git';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { openWorkflowFullReview } from '../../stores/workflowThreads';

  interface Props {
    /** The run's checkout — its worktree, or the project root when it cut
     *  none. The subject of the diff, and it exists whether or not a phase
     *  thread survived. */
    workspace: WorkspaceRef | null;
    /** The phase thread the full-review companion mounts on. Empty when no
     *  attempt ran, which is the one thing "Open full review" needs. */
    threadId: string;
    baseBranch: string;
    expandFirst: boolean;
  }
  let { workspace, threadId, baseBranch, expandFirst }: Props = $props();

  // Both controls read workspace content — the branch-base diff, and the
  // review companion opened over it.
  let ungranted = $derived(!projectHasScope('files:read', workspace?.projectId));
  // The parsed files; `files` is their summaries for the list.
  let parsed: ReviewFile[] = [];
  let read: ReviewDiffRead | null = null;
  let files = $state<PatchFile[]>([]);
  let loading = $state(false);
  let loaded = $state(false);
  let error = $state('');
  let loadedKey = '';

  // A run whose detail reloads under a live event must not keep another run's
  // patch on screen; the key is the exact input the patch was fetched for.
  $effect(() => {
    const key = `${workspace?.projectId ?? ''}\n${workspace?.workspacePath ?? ''}\n${baseBranch}`;
    if (key === loadedKey) return;
    loadedKey = key;
    clear();
    error = '';
  });

  onDestroy(() => {
    loadedKey = '';
    clear();
  });

  function clear(): void {
    read?.dispose();
    read = null;
    parsed = [];
    files = [];
    loaded = false;
  }

  async function load(): Promise<void> {
    if (loading || ungranted || !workspace) return;
    loading = true;
    error = '';
    const key = loadedKey;
    const ws = workspace;
    const base = baseBranch;
    try {
      // Never ignore whitespace here: a gate decision is made against the
      // exact change, and this surface has no toggle to say otherwise.
      const source = new ReviewDiffSource(
        threadMachine(threadId, ws.projectId),
        () => OpenBranchBaseDiff(ws, base, false),
      );
      const next = await source.read({ cancelled: () => loadedKey !== key });
      if (!next) return;
      if (loadedKey !== key) {
        next.dispose();
        return;
      }
      read?.dispose();
      read = next;
      next.onLost(() => {
        if (read !== next) return;
        clear();
        void load();
      });
      parsed = next.parser.files;
      files = parsed.map((file) => ({
        path: file.path,
        kind: file.kind,
        additions: file.additions,
        deletions: file.deletions,
        lines: [],
      }));
      loaded = true;
    } catch (err) {
      if (loadedKey === key) error = userFacingError(err, 'Could not load the changes.');
    } finally {
      loading = false;
    }
  }

  async function loadFile(path: string): Promise<PatchFile> {
    const file = parsed.find((entry) => entry.path === path);
    if (!file) throw new Error(`No hunks for ${path}`);
    return file.body.whenResident(() => patchFileFromReviewFile(file));
  }

  async function openFullReview(): Promise<void> {
    if (ungranted || !threadId) return;
    try {
      await openWorkflowFullReview(threadId);
    } catch (err) {
      addToast('error', userFacingError(err, 'Could not open the review pane.'));
    }
  }
</script>

<section class="space-y-2" data-testid="workflow-gate-diff">
  {#if files.length > 0}
    <WorkflowDiff {files} {expandFirst} onLoadFile={loadFile} />
  {:else if loading}
    <p class="text-xs text-fg-muted" data-testid="workflow-diff-loading">Loading changes…</p>
  {:else if error}
    <button
      class="text-xs text-error hover:underline disabled:cursor-not-allowed disabled:opacity-50"
      onclick={() => { void load(); }}
      disabled={ungranted}
      title={ungranted ? 'Not granted to this device' : undefined}
      data-testid="workflow-diff-retry"
    >{error} · retry</button>
  {:else if loaded}
    <p class="text-xs text-fg-muted" data-testid="workflow-diff-empty">No changes.</p>
  {:else}
    <button
      class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
      onclick={() => { void load(); }}
      disabled={ungranted || !workspace}
      title={ungranted ? 'Not granted to this device' : undefined}
      data-testid="workflow-diff-load"
    >Load changes</button>
  {/if}

  {#if threadId}
    <button
      class="rounded-md border border-border-subtle px-2.5 py-1.5 text-xs text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
      onclick={() => { void openFullReview(); }}
      disabled={ungranted}
      title={ungranted ? 'Not granted to this device' : undefined}
      data-testid="workflow-open-full-review"
    >Open full review</button>
  {/if}
</section>
