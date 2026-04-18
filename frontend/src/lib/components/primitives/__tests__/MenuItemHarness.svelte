<!--
  Minimal wrapper so tests can pass icon snippets through props. The
  `showIcon` flag controls whether the icon slot is filled.
-->
<script lang="ts">
  import MenuItem from '../MenuItem.svelte';

  let {
    label = 'Apple',
    kbd = undefined as string | undefined,
    checked = false,
    disabled = false,
    onSelect = undefined as (() => void) | undefined,
    variant = 'default' as 'default' | 'danger',
    showIcon = false,
  }: {
    label?: string;
    kbd?: string;
    checked?: boolean;
    disabled?: boolean;
    onSelect?: () => void;
    variant?: 'default' | 'danger';
    showIcon?: boolean;
  } = $props();
</script>

{#if showIcon}
  <MenuItem {label} {kbd} {checked} {disabled} {onSelect} {variant}>
    {#snippet icon()}
      <span data-testid="menuitem-icon">*</span>
    {/snippet}
  </MenuItem>
{:else}
  <MenuItem {label} {kbd} {checked} {disabled} {onSelect} {variant} />
{/if}
