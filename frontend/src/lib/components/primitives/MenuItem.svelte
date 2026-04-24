<script lang="ts">
  // A single row inside a <Menu>. Consumes roving tabindex from Menu —
  // the parent sets `tabindex` on these via the DOM, so we don't render
  // a baseline tabindex here (Menu takes care of it on mount + keyboard
  // nav). We DO render `role="menuitem"` so Menu's querySelector finds
  // us.
  //
  // The `onSelect` callback is what Menu consumers actually care about;
  // after firing it we also bubble a `menuitem-select` CustomEvent so an
  // outer Popover / Menu can trigger `onClose` without wiring a prop on
  // every item.

  import type { Snippet } from 'svelte';

  interface Props {
    label: string;
    description?: string;
    icon?: Snippet;
    kbd?: string;
    suffix?: string;
    checked?: boolean;
    disabled?: boolean;
    title?: string;
    onSelect?: () => void;
    variant?: 'default' | 'danger';
  }

  let {
    label,
    description,
    icon,
    kbd,
    suffix,
    checked = false,
    disabled = false,
    title,
    onSelect,
    variant = 'default',
  }: Props = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);

  function activate(): void {
    if (disabled) return;
    onSelect?.();
    // Dispatch after the select callback so listeners can rely on any
    // state changes the callback performed.
    buttonEl?.dispatchEvent(
      new CustomEvent('menuitem-select', {
        bubbles: true,
        detail: { label },
      }),
    );
  }

  function handleClick(): void {
    activate();
  }

  function handleKeydown(e: KeyboardEvent): void {
    // Enter / Space activate the item. Arrow nav is handled by the
    // parent Menu — this handler only claims the activation keys.
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      activate();
    }
  }

  // Variant determines label color at rest + hover emphasis. Danger
  // items still keep the default check column so they can be used in
  // radio groups (e.g. destructive selection in a confirmation menu).
  const VARIANT_TEXT: Record<NonNullable<Props['variant']>, string> = {
    default: 'text-fg',
    danger: 'text-error',
  };
</script>

<!-- svelte-ignore a11y_click_events_have_key_events — onkeydown is handled -->
<button
  bind:this={buttonEl}
  type="button"
  role="menuitem"
  aria-disabled={disabled ? 'true' : undefined}
  data-menuitem
  data-disabled={disabled ? 'true' : undefined}
  tabindex={-1}
  title={title}
  onclick={handleClick}
  onkeydown={handleKeydown}
  class={[
    'w-full flex items-center gap-2 px-3 py-1.5 text-sm text-left',
    'cursor-pointer select-none',
    'focus-visible:outline-none',
    'hover:bg-surface-2/40 focus:bg-surface-2/40',
    VARIANT_TEXT[variant],
    disabled ? 'opacity-50 cursor-not-allowed hover:bg-transparent focus:bg-transparent' : '',
  ].join(' ')}
>
  {#if icon}
    <span class="flex h-4 w-4 items-center justify-center text-fg-subtle" aria-hidden="true">
      {@render icon()}
    </span>
  {/if}
  <span class="min-w-0 flex-1">
    <span class="block truncate">{label}</span>
    {#if description}
      <span class="block truncate text-[11px] leading-4 text-fg-hint">{description}</span>
    {/if}
  </span>
  {#if checked}
    <span class="text-accent/80" aria-hidden="true">&#10003;</span>
  {/if}
  {#if kbd}
    <span
      class="ml-auto text-[10px] tracking-wide text-fg-hint"
      aria-hidden="true"
    >
      {kbd}
    </span>
  {/if}
  {#if suffix}
    <span class="ml-auto text-[10px] text-fg-hint">
      {suffix}
    </span>
  {/if}
</button>
