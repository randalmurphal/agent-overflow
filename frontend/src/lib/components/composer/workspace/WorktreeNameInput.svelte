<script lang="ts">
  // Inline new-branch slot. Renders one of three states based on the
  // thread's worktree intent:
  //
  //   creatingBranch=true                                  → text input
  //   mode='new-worktree' && !creatingBranch               → "+ new branch" button
  //   otherwise                                            → nothing
  //
  // The button's only job is to call enterCreateBranchMode — the
  // resulting state then re-renders this component into the text-input
  // branch. In local mode the entry point is the BranchPicker
  // dropdown's "+ New branch…" entry instead, so no button shows here.

  import Plus from 'lucide-svelte/icons/plus';
  import Icon from '../../primitives/Icon.svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import {
    enterCreateBranchMode,
    setNewBranchName,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';

  interface Props {
    pane: ThreadPane;
    workspaceDirty: boolean;
    onSubmit?: () => void;
    onCancel?: () => void;
  }

  let { pane, workspaceDirty, onSubmit, onCancel }: Props = $props();

  let intent = $derived(worktreeIntentForThread(pane.thread));
  let inputId = $derived(`new-branch-name-${pane.thread?.id ?? 'none'}`);
  let currentBranch = $derived(pane.thread?.branch ?? '');
  let showInput = $derived(intent.creatingBranch);
  let showButton = $derived(!intent.creatingBranch && intent.mode === 'new-worktree');

  function updateBranchName(value: string): void {
    if (!pane.thread) return;
    setNewBranchName(pane.thread, value);
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault();
      onSubmit?.();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      onCancel?.();
    }
  }

  function startCreate(): void {
    if (!pane.thread) return;
    enterCreateBranchMode(pane.thread, { workspaceDirty, currentBranch });
  }
</script>

{#if showInput}
  <label class="sr-only" for={inputId}>New branch name</label>
  <input
    id={inputId}
    data-testid="worktree-branch-name-input"
    type="text"
    value={intent.newBranchName}
    placeholder="Branch Name"
    oninput={(e) => updateBranchName((e.target as HTMLInputElement).value)}
    onkeydown={handleKeydown}
    class={[
      'h-6 w-[11rem] min-w-0 rounded border border-border bg-transparent',
      'px-2 text-[11px] text-text-primary placeholder:text-fg-hint',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    ].join(' ')}
  />
{:else if showButton}
  <button
    type="button"
    onclick={startCreate}
    data-testid="new-branch-toggle"
    class={[
      'inline-flex h-6 items-center gap-1 rounded border border-border bg-transparent',
      'px-2 text-[11px] text-fg-muted hover:text-fg hover:border-border-strong',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    ].join(' ')}
  >
    <Icon icon={Plus} size={10} strokeWidth={2} />
    new branch
  </button>
{/if}
