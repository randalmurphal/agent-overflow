<script lang="ts">
  import X from 'lucide-svelte/icons/x';
  import Columns2 from 'lucide-svelte/icons/columns-2';
  import Rows3 from 'lucide-svelte/icons/rows-3';
  import WrapText from 'lucide-svelte/icons/wrap-text';
  import Icon from '../../primitives/Icon.svelte';
  import type { DiffPanelTab } from '../../../types/checkpoint';
  import type { DiffViewMode } from '../../../stores/diffPanel.svelte';

  interface Props {
    totals: { files: number; additions: number; deletions: number };
    viewMode: DiffViewMode;
    setViewMode: (mode: DiffViewMode) => void;
    wordWrap: boolean;
    setWordWrap: (next: boolean) => void;
    tabMode: DiffPanelTab;
    setTabMode: (mode: DiffPanelTab) => void;
    onClose: () => void;
  }

  let {
    totals,
    viewMode,
    setViewMode,
    wordWrap,
    setWordWrap,
    tabMode,
    setTabMode,
    onClose,
  }: Props = $props();
</script>

<div class="flex items-center gap-2 px-3 py-2">
  <div class="min-w-0 flex-1">
    <div class="text-[12px] font-semibold uppercase tracking-[0.08em] text-fg-muted">Checkpoint Diff</div>
    <div class="mt-0.5 flex items-center gap-2 text-[12px] text-fg-muted">
      <span>{totals.files} files</span>
      <span class="text-success">+{totals.additions}</span>
      <span class="text-error">-{totals.deletions}</span>
    </div>
  </div>
  <button
    class="rounded p-1.5 hover:bg-surface-2 {viewMode === 'stacked' ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
    title="Stacked View"
    aria-pressed={viewMode === 'stacked'}
    onclick={() => setViewMode('stacked')}
  >
    <Icon icon={Rows3} size={15} />
  </button>
  <button
    class="rounded p-1.5 hover:bg-surface-2 {viewMode === 'split' ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
    title="Split View"
    aria-pressed={viewMode === 'split'}
    onclick={() => setViewMode('split')}
  >
    <Icon icon={Columns2} size={15} />
  </button>
  <button
    class="rounded p-1.5 hover:bg-surface-2 {wordWrap ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
    title="Wrap Lines"
    aria-pressed={wordWrap}
    onclick={() => setWordWrap(!wordWrap)}
  >
    <Icon icon={WrapText} size={15} />
  </button>
  <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2" aria-label="Close Diff Panel" data-testid="diff-panel-close" onclick={onClose}>
    <Icon icon={X} size={15} />
  </button>
</div>

<div class="flex items-center gap-1 border-t border-border-subtle px-3 py-2" role="tablist" aria-label="Diff scope">
  {@render TabButton('per-turn', 'Per-turn', 'diff-tab-per-turn')}
  {@render TabButton('session', 'Session', 'diff-tab-session')}
  {@render TabButton('workspace', 'Workspace', 'diff-tab-workspace')}
</div>

{#snippet TabButton(mode: DiffPanelTab, label: string, testId: string)}
  <button
    role="tab"
    aria-selected={tabMode === mode}
    class="rounded px-2.5 py-1 text-[12px] {tabMode === mode ? 'bg-surface-2 text-fg' : 'text-fg-muted hover:bg-surface-2/60'}"
    onclick={() => setTabMode(mode)}
    data-testid={testId}
  >
    {label}
  </button>
{/snippet}
