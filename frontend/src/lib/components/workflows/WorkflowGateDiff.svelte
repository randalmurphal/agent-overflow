<script lang="ts">
  // Gate / done evidence: the changed-file list, expanding hunks in place, and
  // the hand-off to the real ReviewPane (UI-SPEC §4.3, §4.7). There is no
  // parallel diff renderer here — the file list reuses WorkflowDiff, and
  // "Open full review" opens the review companion on the phase's own thread,
  // which closes the overlay (R3).
  //
  // The whole patch is fetched once and each file's hunks are sliced out of it
  // on expand, so opening three files costs one RPC, not four.

  import WorkflowDiff from './WorkflowDiff.svelte';
  import type { PatchFile } from '../../utils/patchFiles';
  import { extractPatchFile, parsePatchFileSummaries, parsePatchFiles } from '../../utils/patchFiles';
  import { GetBranchBaseDiff } from '../../stores/bindings';
  import type { WorkspaceRef } from '../../types/git';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { hasScope } from '../../transport/scopes';
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
  let ungranted = $derived(!hasScope('files:read'));
  let patch = $state('');
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
    patch = '';
    files = [];
    loaded = false;
    error = '';
  });

  async function load(): Promise<void> {
    if (loading || ungranted || !workspace) return;
    loading = true;
    error = '';
    try {
      // Never ignore whitespace here: a gate decision is made against the
      // exact change, and this surface has no toggle to say otherwise.
      const raw = String((await GetBranchBaseDiff(workspace, baseBranch, false)) ?? '');
      patch = raw;
      files = parsePatchFileSummaries(raw);
      loaded = true;
    } catch (err) {
      error = userFacingError(err, 'Could not load the changes.');
    } finally {
      loading = false;
    }
  }

  async function loadFile(path: string): Promise<PatchFile> {
    const single = extractPatchFile(patch, path);
    const parsed = single ? parsePatchFiles(single) : [];
    const file = parsed.find((entry) => entry.path === path) ?? parsed[0];
    if (!file) throw new Error(`No hunks for ${path}`);
    return file;
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
