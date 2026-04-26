<script lang="ts">
  import Copy from 'lucide-svelte/icons/copy';
  import Check from 'lucide-svelte/icons/check';
  import Save from 'lucide-svelte/icons/save';
  import Play from 'lucide-svelte/icons/play';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';

  interface Props {
    copied: boolean;
    implemented: boolean;
    implementing: boolean;
    onImplement: () => void | Promise<void>;
    onCopy: () => void | Promise<void>;
    onSave: () => void | Promise<void>;
    onOpenInSidebar?: () => void;
  }

  let {
    copied,
    implemented,
    implementing,
    onImplement,
    onCopy,
    onSave,
    onOpenInSidebar,
  }: Props = $props();
</script>

<div class="flex items-center gap-1.5 text-xs text-text-secondary">
  <Button
    variant="tinted"
    size="xs"
    disabled={implemented || implementing}
    loading={implementing}
    onclick={() => void onImplement()}
  >
    {#snippet children()}
      <span class="inline-flex items-center gap-1"><Icon icon={Play} size={12} />Implement</span>
    {/snippet}
  </Button>
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
