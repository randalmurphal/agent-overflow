<script lang="ts">
  import Save from '@lucide/svelte/icons/save';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import CopyButton from '../primitives/CopyButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyMarkdownToClipboard } from '../../utils/markdownClipboard';

  interface Props {
    getCopyText: () => Promise<string>;
    onSave: () => void | Promise<void>;
    onOpenInSidebar?: () => void;
  }

  let {
    getCopyText,
    onSave,
    onOpenInSidebar,
  }: Props = $props();
</script>

<div class="flex items-center gap-1.5 text-xs text-text-secondary">
  <CopyButton
    text={getCopyText}
    write={copyMarkdownToClipboard}
    label="Copy full plan"
    onError={() => addToast('error', 'Failed to copy plan')}
  />
  <IconButton label="Save plan" size="sm" onClick={() => void onSave()}>
    {#snippet children()}<Icon icon={Save} size={13} />{/snippet}
  </IconButton>
  {#if onOpenInSidebar}
    <IconButton label="Open in plan sidebar" size="sm" onClick={onOpenInSidebar}>
      {#snippet children()}<Icon icon={PanelRightOpen} size={13} />{/snippet}
    </IconButton>
  {/if}
</div>
