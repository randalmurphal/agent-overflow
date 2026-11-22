<script lang="ts">
  import { onDestroy } from 'svelte';
  import PatchLines from '../shared/PatchLines.svelte';
  import type { ReviewFile } from '../../utils/patchParser';
  import type { BackendKey } from '../../transport/backendKey';
  import { OpenCommitFileDiff } from '../../stores/bindings';
  import { ReviewDiffSource, type ReviewDiffRead } from '../../stores/reviewDiffStream';
  import type { CommitFile, WorkspaceRef } from '../../types/git';
  import { userFacingError } from '../../utils/userFacingError';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import type { ScopeGrant } from '../../transport/scopes';
  import { changeKey, rowPaths } from './commitFiles';

  // The uncommitted patch of the commit dialog's active row. One file is
  // read at a time, when it becomes active or its change moves, and the
  // read is disposed with it.

  interface Props {
    workspace: WorkspaceRef;
    /** The computer that serves the workspace's diffs. */
    backend: BackendKey;
    file: CommitFile | null;
    /** Whether this device may read workspace files. */
    grant: ScopeGrant;
  }
  let { workspace, backend, file, grant }: Props = $props();
  let blocked = $derived(grantBlockedTitle(grant));

  let read: ReviewDiffRead | null = null;
  let shown = $state.raw<ReviewFile | null>(null);
  let loading = $state(false);
  let loaded = $state(false);
  let error = $state('');
  let loadedKey = '';

  $effect(() => {
    const key = file && grant === 'granted'
      ? `${workspace.projectId}\n${workspace.workspacePath}\n${changeKey(file)}`
      : '';
    if (key === loadedKey) return;
    loadedKey = key;
    clear();
    error = '';
    if (key && file) void load(key, workspace, file);
  });

  onDestroy(() => {
    loadedKey = '';
    clear();
  });

  function clear(): void {
    read?.dispose();
    read = null;
    shown = null;
    loaded = false;
  }

  async function load(key: string, ws: WorkspaceRef, target: CommitFile): Promise<void> {
    loading = true;
    try {
      const source = new ReviewDiffSource(backend, () => OpenCommitFileDiff(ws, rowPaths(target)));
      const next = await source.read({ cancelled: () => loadedKey !== key });
      if (!next) return;
      if (loadedKey !== key) {
        next.dispose();
        return;
      }
      read = next;
      next.onLost(() => {
        if (read !== next) return;
        clear();
        void load(key, ws, target);
      });
      shown = next.parser.files[0] ?? null;
      loaded = true;
    } catch (err) {
      if (loadedKey === key) error = userFacingError(err, 'Could not load the changes.');
    } finally {
      if (loadedKey === key) loading = false;
    }
  }
</script>

<div
  class="flex h-full min-h-0 flex-col overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-0"
  data-testid="commit-file-preview"
>
  {#if shown && shown.body.lineCount > 0}
    <PatchLines file={shown} tint boxClass="h-full" />
  {:else}
    <p class="m-auto px-4 text-center text-[0.75rem] text-fg-muted" role={error ? 'alert' : undefined}>
      {#if !file}
        Select a file to see its changes.
      {:else if blocked}
        {blocked}
      {:else if error}
        {error}
      {:else if loading || !loaded}
        Loading changes…
      {:else if file.binary}
        Binary file
      {:else}
        No text changes to show.
      {/if}
    </p>
  {/if}
</div>
