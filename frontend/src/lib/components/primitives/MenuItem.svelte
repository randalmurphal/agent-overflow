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

  interface BaseProps {
    label: string;
    description?: string;
    icon?: Snippet;
    indicator?: Snippet;
    kbd?: string;
    suffix?: string;
    checked?: boolean;
    disabled?: boolean;
    title?: string;
    onSelect?: () => void;
    actionLabel?: string;
    actionPressed?: boolean;
    actionPosition?: 'start' | 'end';
    actionDisabled?: boolean;
    actionTitle?: string;
    onAction?: () => void;
    variant?: 'default' | 'danger';
  }

  /**
   * The trailing action renders one of two ways — an icon snippet in the
   * w-5 square, or a compact labeled text button (`actionText`). They fill
   * the same slot, so the union makes passing both a type error rather
   * than a precedence rule a caller has to know.
   */
  type Props = BaseProps &
    ({ action?: Snippet; actionText?: never } | { actionText?: string; action?: never });

  let {
    label,
    description,
    icon,
    indicator,
    kbd,
    suffix,
    checked = false,
    disabled = false,
    title,
    onSelect,
    action,
    actionLabel,
    actionText,
    actionPressed,
    actionPosition = 'end',
    actionDisabled = false,
    actionTitle,
    onAction,
    variant = 'default',
  }: Props = $props();

  let buttonEl: HTMLElement | undefined = $state(undefined);

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

  // Deliberately independent of row `disabled`: the row select and the
  // action are separate affordances (e.g. EnvPicker blocks switching
  // workspaces while a turn runs but still allows removing idle
  // worktrees). Consumers that want both off pass both props. The action
  // buttons carry an explicit aria-disabled="false" when enabled for the
  // same reason — the row's aria-disabled="true" would otherwise be
  // inherited by descendants and announce the live action as disabled.
  function handleActionClick(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    if (actionDisabled) return;
    onAction?.();
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

  let hasAction = $derived(!!onAction && (!!action || !!actionText));
</script>

{#snippet actionButton(positionClass: string)}
  <button
    type="button"
    aria-label={actionLabel}
    aria-pressed={actionPressed}
    aria-disabled={actionDisabled ? 'true' : 'false'}
    disabled={actionDisabled}
    title={actionTitle}
    tabindex={-1}
    onclick={handleActionClick}
    class={[
      positionClass,
      'inline-flex h-5 items-center justify-center rounded-[var(--radius-field)]',
      actionText ? 'px-1.5 text-[0.625rem] font-medium whitespace-nowrap' : 'w-5',
      'text-fg-hint transition-colors',
      actionDisabled
        ? 'opacity-50 cursor-not-allowed'
        : 'hover:bg-surface-2/70 hover:text-fg',
      actionPressed ? 'text-warning' : '',
    ].join(' ')}
  >
    {#if actionText}{actionText}{:else if action}{@render action()}{/if}
  </button>
{/snippet}

<!-- svelte-ignore a11y_click_events_have_key_events — onkeydown is handled -->
<svelte:element
  this={hasAction ? 'div' : 'button'}
  bind:this={buttonEl}
  type={hasAction ? undefined : 'button'}
  role="menuitem"
  aria-disabled={disabled ? 'true' : undefined}
  data-menuitem
  data-disabled={disabled ? 'true' : undefined}
  tabindex={-1}
  title={title}
  onclick={handleClick}
  onkeydown={handleKeydown}
  class={[
    'w-full flex items-center gap-2 px-3 py-1.5 text-sm text-left compact:py-2.5',
    'cursor-pointer select-none',
    'focus-visible:outline-none',
    'hover:bg-surface-2/40 focus:bg-surface-2/40',
    VARIANT_TEXT[variant],
    disabled ? 'cursor-not-allowed hover:bg-transparent focus:bg-transparent' : '',
  ].join(' ')}
>
  {#if hasAction && actionPosition === 'start'}
    {@render actionButton('')}
  {/if}
  <!-- Row-disabled dimming lives on this wrapper (not the container) so an
       enabled action button keeps full contrast — CSS opacity on a parent
       cannot be undone by a child. -->
  <span class={['flex min-w-0 flex-1 items-center gap-2', disabled ? 'opacity-50' : ''].join(' ')}>
    {#if icon}
      <span class="flex h-4 w-4 items-center justify-center text-fg-subtle" aria-hidden="true">
        {@render icon()}
      </span>
    {/if}
    <span class="min-w-0 flex-1">
      <span class="block truncate">{label}</span>
      {#if description}
        <span class="block truncate text-[0.6875rem] leading-4 text-fg-hint">{description}</span>
      {/if}
    </span>
    {#if indicator}
      {@render indicator()}
    {:else if checked}
      <span class="text-accent/80" aria-hidden="true">&#10003;</span>
    {/if}
    {#if kbd}
      <span
        class="ml-auto text-[0.625rem] tracking-wide text-fg-hint"
        aria-hidden="true"
      >
        {kbd}
      </span>
    {/if}
    {#if suffix}
      <span class="ml-auto text-[0.625rem] text-fg-hint">
        {suffix}
      </span>
    {/if}
  </span>
  {#if hasAction && actionPosition === 'end'}
    {@render actionButton('ml-1')}
  {/if}
</svelte:element>
