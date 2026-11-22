<script lang="ts">
  // The pane-header maximize control: promotes the pane to fill the pane
  // area, and on the promoted pane restores it. Desktop only: compact
  // shows one pane at a time already. `size="toolbar"` is the same control
  // at a toolbar's hit target (the browser pane's address row).
  import Maximize2 from '@lucide/svelte/icons/maximize-2';
  import Minimize2 from '@lucide/svelte/icons/minimize-2';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import { getPromotedPaneId } from '../../stores/paneLayout.svelte';
  import { togglePanePromotion } from '../../stores/panes.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { chordHintSuffix } from '../../stores/keybindings.svelte';

  let {
    paneId,
    testId = 'pane-promote',
    size = 'header',
  }: {
    paneId: string;
    testId?: string;
    size?: 'header' | 'toolbar';
  } = $props();

  let promoted = $derived(getPromotedPaneId() === paneId);
  let label = $derived(promoted ? 'Restore Pane' : 'Maximize Pane');
</script>

{#if !isCompactLayout()}
  {#if size === 'toolbar'}
    <IconButton
      {label}
      title={`${label}${chordHintSuffix('pane.togglePromoted')}`}
      {testId}
      size="sm"
      onClick={() => togglePanePromotion(paneId)}
    >
      <Icon icon={promoted ? Minimize2 : Maximize2} size={13} />
    </IconButton>
  {:else}
    <PaneHeaderIconButton
      {label}
      title={`${label}${chordHintSuffix('pane.togglePromoted')}`}
      {testId}
      onclick={() => togglePanePromotion(paneId)}
    >
      <Icon icon={promoted ? Minimize2 : Maximize2} size={12} strokeWidth={2} />
    </PaneHeaderIconButton>
  {/if}
{/if}
