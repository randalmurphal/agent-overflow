<script lang="ts">
  import type { DiffPanelSource } from '../../../stores/diffPanel.svelte';
  import SourceTabs from './SourceTabs.svelte';

  interface Props {
    source: DiffPanelSource;
    turnTabVisible: boolean;
    wordWrap: boolean;
    onSelectSource: (next: DiffPanelSource) => void;
    onToggleWordWrap: () => void;
    onClose: () => void;
  }

  let {
    source,
    turnTabVisible,
    wordWrap,
    onSelectSource,
    onToggleWordWrap,
    onClose,
  }: Props = $props();
</script>

<div class="flex items-center gap-3 border-b border-border bg-surface-1 px-3 py-2">
  <span class="text-xs font-semibold text-text-primary shrink-0">Diffs</span>
  <SourceTabs {source} {turnTabVisible} onSelect={onSelectSource} />
  <div class="ml-auto flex items-center gap-2 shrink-0">
    <label
      class="flex items-center gap-1 text-xs text-text-secondary cursor-pointer select-none"
      title="Wrap long lines"
    >
      <input
        type="checkbox"
        checked={wordWrap}
        data-testid="diff-panel-wrap"
        onchange={onToggleWordWrap}
        class="accent-accent"
      />
      Wrap
    </label>
    <button
      type="button"
      class="rounded px-2 py-1 text-xs text-text-secondary hover:bg-surface-2/50 cursor-pointer"
      aria-label="Close diff panel"
      data-testid="diff-panel-close"
      onclick={onClose}
    >
      ✕
    </button>
  </div>
</div>
