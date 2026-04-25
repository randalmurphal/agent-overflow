<script lang="ts">
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import {
    setWorktreeBranchName,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let intent = $derived(worktreeIntentForThread(pane.thread));
  let visible = $derived(intent.mode === 'new-worktree');
  let inputId = $derived(`worktree-branch-name-${pane.thread?.id ?? 'none'}`);

  function updateBranchName(value: string): void {
    if (!pane.thread) return;
    setWorktreeBranchName(pane.thread, value);
  }
</script>

{#if visible}
  <label class="sr-only" for={inputId}>Worktree branch name</label>
  <input
    id={inputId}
    data-testid="worktree-branch-name-input"
    type="text"
    value={intent.branchName}
    placeholder="Branch Name"
    oninput={(e) => updateBranchName((e.target as HTMLInputElement).value)}
    class={[
      'h-6 w-[11rem] min-w-0 rounded border border-border bg-transparent',
      'px-2 text-[11px] text-text-primary placeholder:text-fg-hint',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    ].join(' ')}
  />
{/if}
