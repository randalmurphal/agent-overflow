<script lang="ts">
  import X from 'lucide-svelte/icons/x';
  import Rows3 from 'lucide-svelte/icons/rows-3';
  import Columns2 from 'lucide-svelte/icons/columns-2';
  import WrapText from 'lucide-svelte/icons/wrap-text';
  import Icon from '../primitives/Icon.svelte';
  import type { DiffViewMode } from '../../stores/diffPanel.svelte';

  interface Props {
    title: string;
    subtitle: string;
    insertions: number;
    deletions: number;
    viewMode: DiffViewMode;
    wordWrap: boolean;
    onChangeViewMode: (mode: DiffViewMode) => void;
    onToggleWordWrap: () => void;
    onClose: () => void;
  }

  let {
    title,
    subtitle,
    insertions,
    deletions,
    viewMode,
    wordWrap,
    onChangeViewMode,
    onToggleWordWrap,
    onClose,
  }: Props = $props();
</script>

<div class="flex items-center justify-between gap-2 px-3 pt-3 pb-2 shrink-0">
  <div class="flex min-w-0 items-center gap-1.5">
    <h3 class="truncate text-sm font-medium text-text-primary" data-testid="diff-sidebar-title">{title}</h3>
    {#if subtitle}
      <span class="shrink-0 text-[0.75rem] text-fg-muted truncate">· {subtitle}</span>
    {/if}
  </div>
  <div class="flex items-center gap-1 shrink-0">
    {#if insertions > 0 || deletions > 0}
      <span class="flex gap-2 text-[0.6875rem] tabular-nums px-1">
        {#if insertions > 0}<span class="text-success">+{insertions}</span>{/if}
        {#if deletions > 0}<span class="text-error">-{deletions}</span>{/if}
      </span>
    {/if}

    <div class="flex items-center rounded border border-border-subtle p-0.5" role="group" aria-label="View mode">
      <button
        type="button"
        onclick={() => onChangeViewMode('stacked')}
        title="Stacked view"
        aria-pressed={viewMode === 'stacked'}
        aria-label="Stacked view"
        data-testid="diff-sidebar-view-stacked"
        class={`rounded p-1 cursor-pointer ${viewMode === 'stacked' ? 'bg-surface-2 text-text-primary' : 'text-text-secondary hover:text-text-primary'}`}
      >
        <Icon icon={Rows3} size={13} />
      </button>
      <button
        type="button"
        onclick={() => onChangeViewMode('split')}
        title="Split view"
        aria-pressed={viewMode === 'split'}
        aria-label="Split view"
        data-testid="diff-sidebar-view-split"
        class={`rounded p-1 cursor-pointer ${viewMode === 'split' ? 'bg-surface-2 text-text-primary' : 'text-text-secondary hover:text-text-primary'}`}
      >
        <Icon icon={Columns2} size={13} />
      </button>
    </div>

    <button
      type="button"
      onclick={onToggleWordWrap}
      title={wordWrap ? 'Disable word wrap' : 'Enable word wrap'}
      aria-pressed={wordWrap}
      aria-label="Toggle word wrap"
      data-testid="diff-sidebar-toggle-wrap"
      class={`rounded p-1 cursor-pointer ${wordWrap ? 'bg-surface-2 text-text-primary' : 'text-text-secondary hover:text-text-primary'}`}
    >
      <Icon icon={WrapText} size={13} />
    </button>

    <button
      type="button"
      onclick={onClose}
      data-testid="diff-sidebar-close"
      aria-label="Close Diff Sidebar"
      class="rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      <Icon icon={X} size={14} />
    </button>
  </div>
</div>
