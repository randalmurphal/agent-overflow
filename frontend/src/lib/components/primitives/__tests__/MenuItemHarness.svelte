<!--
  Minimal wrapper so tests can pass icon snippets through props. The
  `showIcon` flag controls whether the icon slot is filled.
-->
<script lang="ts">
  import MenuItem from '../MenuItem.svelte';

  let {
    label = 'Apple',
    kbd = undefined as string | undefined,
    suffix = undefined as string | undefined,
    title = undefined as string | undefined,
    checked = false,
    disabled = false,
    onSelect = undefined as (() => void) | undefined,
    variant = 'default' as 'default' | 'danger',
    showIcon = false,
    showIndicator = false,
  }: {
    label?: string;
    kbd?: string;
    suffix?: string;
    title?: string;
    checked?: boolean;
    disabled?: boolean;
    onSelect?: () => void;
    variant?: 'default' | 'danger';
    showIcon?: boolean;
    showIndicator?: boolean;
  } = $props();
</script>

{#if showIndicator}
  <MenuItem {label} {kbd} {suffix} {title} {checked} {disabled} {onSelect} {variant}>
    {#snippet indicator()}
      <span data-testid="menuitem-indicator">toggle</span>
    {/snippet}
  </MenuItem>
{:else if showIcon}
  <MenuItem {label} {kbd} {suffix} {title} {checked} {disabled} {onSelect} {variant}>
    {#snippet icon()}
      <span data-testid="menuitem-icon">*</span>
    {/snippet}
  </MenuItem>
{:else}
  <MenuItem {label} {kbd} {suffix} {title} {checked} {disabled} {onSelect} {variant} />
{/if}
