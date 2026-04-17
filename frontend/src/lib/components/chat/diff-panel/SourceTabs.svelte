<script lang="ts">
  import type { DiffPanelSource } from '../../../stores/diffPanel.svelte';

  interface Props {
    source: DiffPanelSource;
    turnTabVisible: boolean;
    onSelect: (next: DiffPanelSource) => void;
  }

  let { source, turnTabVisible, onSelect }: Props = $props();

  const TABS: Array<{ id: DiffPanelSource; label: string }> = [
    { id: 'turn', label: 'Turn diffs' },
    { id: 'worktree', label: 'Working tree' },
    { id: 'cumulative', label: 'Agent cumulative' },
  ];

  let visibleTabs = $derived(TABS.filter((t) => t.id !== 'turn' || turnTabVisible));
</script>

<div role="tablist" aria-label="Diff source" class="flex gap-1 p-1 bg-surface-1 rounded border border-border">
  {#each visibleTabs as tab (tab.id)}
    <button
      role="tab"
      aria-selected={source === tab.id}
      aria-controls="diff-panel-pane"
      data-testid={`diff-source-tab-${tab.id}`}
      class={[
        'px-3 py-1.5 text-xs font-medium rounded transition-colors cursor-pointer',
        source === tab.id
          ? 'bg-accent/20 text-accent'
          : 'text-text-secondary hover:text-text-primary hover:bg-surface-2/50',
      ].join(' ')}
      onclick={() => onSelect(tab.id)}
    >
      {tab.label}
    </button>
  {/each}
</div>
