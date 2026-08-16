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
    showAction = false,
    actionText = undefined as string | undefined,
    actionPosition = 'end' as 'start' | 'end',
    actionDisabled = false,
    onAction = undefined as (() => void) | undefined,
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
    showAction?: boolean;
    actionText?: string;
    actionPosition?: 'start' | 'end';
    actionDisabled?: boolean;
    onAction?: () => void;
  } = $props();
</script>

{#if actionText !== undefined}
  <MenuItem
    {label}
    {disabled}
    {onSelect}
    actionLabel="Row action"
    {actionText}
    {actionPosition}
    {actionDisabled}
    {onAction}
  />
{:else if showAction}
  <MenuItem
    {label}
    {disabled}
    {onSelect}
    actionLabel="Row action"
    {actionPosition}
    {actionDisabled}
    {onAction}
  >
    {#snippet action()}
      <span data-testid="menuitem-action-icon">x</span>
    {/snippet}
  </MenuItem>
{:else if showIndicator}
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
