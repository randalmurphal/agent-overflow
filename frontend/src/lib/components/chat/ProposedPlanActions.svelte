<script lang="ts">
  import Copy from 'lucide-svelte/icons/copy';
  import Check from 'lucide-svelte/icons/check';
  import Save from 'lucide-svelte/icons/save';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';

  interface Props {
    copied: boolean;
    onCopy: () => void | Promise<void>;
    onSave: () => void | Promise<void>;
    onOpenInSidebar?: () => void;
  }

  let {
    copied,
    onCopy,
    onSave,
    onOpenInSidebar,
  }: Props = $props();
</script>

<div class="flex items-center gap-1.5 text-xs text-text-secondary">
  <IconButton label={copied ? 'Copied' : 'Copy full plan'} size="sm" onClick={() => void onCopy()}>
    {#snippet children()}<Icon icon={copied ? Check : Copy} size={13} />{/snippet}
  </IconButton>
  <IconButton label="Save plan" size="sm" onClick={() => void onSave()}>
    {#snippet children()}<Icon icon={Save} size={13} />{/snippet}
  </IconButton>
  {#if onOpenInSidebar}
    <IconButton label="Open in plan sidebar" size="sm" onClick={onOpenInSidebar}>
      {#snippet children()}<Icon icon={PanelRightOpen} size={13} />{/snippet}
    </IconButton>
  {/if}
</div>
