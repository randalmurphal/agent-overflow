<script lang="ts">
  /*
   * Shared button primitive. Replaces ~40 hand-rolled
   * `inline-flex rounded-md border ...` patterns across the codebase
   * so hover/focus/disabled states are consistent and tunable in one
   * place.
   *
   * Variants:
   *   primary        — filled accent, for the single hero action per
   *                    surface
   *   secondary      — outlined, for non-destructive alternate actions
   *   ghost          — transparent text button with hover tint; the
   *                    toolbar + picker language
   *   tinted         — soft-primary: bg-accent/15 text-accent. For
   *                    accent-colored actions that should read as
   *                    "important but not the hero" (e.g. Implement
   *                    plan, where Review/Dismiss are ghost siblings)
   *   danger         — filled error color, for destructive
   *                    confirmations
   *   danger-outline — outlined error: error border + error text +
   *                    subtle error hover fill. For destructive actions
   *                    that live next to neutral ones (tool-approval
   *                    Deny, permission Deny, MCP Decline, git error
   *                    retry — none should dominate like filled danger)
   *   danger-ghost   — ghost error: just error text + subtle error
   *                    hover. For destructive row-level actions
   *                    (Remove Participant)
   *
   * Sizes calibrated to the project's density: xs for thread-row
   * affordances, sm for toolbars, md for modal CTAs.
   *
   * `pressed` toggles the aria-pressed state and applies a visual
   * depressed treatment (used for ChatHeader Diffs/Plans toggles and
   * DesignPreviewPanel viewport selector).
   */
  import type { Snippet } from 'svelte';

  type Variant =
    | 'primary'
    | 'secondary'
    | 'ghost'
    | 'tinted'
    | 'danger'
    | 'danger-outline'
    | 'danger-ghost';
  type Size = 'xs' | 'sm' | 'md';
  type ButtonType = 'button' | 'submit' | 'reset';

  interface Props {
    variant?: Variant;
    size?: Size;
    type?: ButtonType;
    disabled?: boolean;
    loading?: boolean;
    pressed?: boolean;
    /**
     * Add `data-autofocus` to the native button. Modal primitive's
     * `focusTrap` action looks for this attribute to decide which
     * element receives focus when the dialog opens. Passing `true`
     * here lets consumers migrate their footer CTA to Button without
     * losing the "autofocus the primary confirmation" UX.
     */
    autofocus?: boolean;
    /**
     * Stamp a `data-testid` on the native button. Kept as a first-
     * class prop so the Button migration doesn't break integration
     * tests that already key off a stable testid.
     */
    testId?: string;
    title?: string;
    ariaLabel?: string;
    class?: string;
    onclick?: (e: MouseEvent) => void;
    children?: Snippet;
    leading?: Snippet;
    trailing?: Snippet;
  }

  let {
    variant = 'secondary',
    size = 'sm',
    type = 'button',
    disabled = false,
    loading = false,
    pressed,
    autofocus = false,
    testId,
    title,
    ariaLabel,
    class: className = '',
    onclick,
    children,
    leading,
    trailing,
  }: Props = $props();

  const SIZE: Record<Size, string> = {
    xs: 'h-6 px-2 text-[11px] gap-1',
    sm: 'h-7 px-2.5 text-xs gap-1.5',
    md: 'h-8 px-3 text-sm gap-2',
  };

  const VARIANT: Record<Variant, string> = {
    primary:
      'bg-accent text-surface-0 hover:opacity-90 active:opacity-80 ' +
      'focus-visible:ring-accent/40',
    secondary:
      'border border-border-subtle bg-transparent text-fg-muted ' +
      'hover:text-fg hover:border-border hover:bg-surface-2/40 ' +
      'focus-visible:ring-accent/40',
    ghost:
      'bg-transparent text-fg-muted hover:text-fg hover:bg-surface-2/40 ' +
      'focus-visible:ring-accent/30',
    tinted:
      'bg-accent/15 text-accent hover:bg-accent/25 active:bg-accent/30 ' +
      'focus-visible:ring-accent/40',
    danger:
      'bg-error text-surface-0 hover:opacity-90 active:opacity-80 ' +
      'focus-visible:ring-error/40',
    'danger-outline':
      'border border-error/40 bg-transparent text-error/90 ' +
      'hover:text-error hover:border-error/70 hover:bg-error/10 ' +
      'focus-visible:ring-error/40',
    'danger-ghost':
      'bg-transparent text-error/80 hover:text-error hover:bg-error/10 ' +
      'focus-visible:ring-error/40',
  };

  // Pressed-state styling is shared across every variant. The subtle
  // inset ring + surface tint reads as "currently engaged" without
  // overriding the variant's rest color identity.
  const PRESSED_CLASSES =
    'bg-surface-2/60 text-fg ring-1 ring-inset ring-border-subtle';

  function handleClick(e: MouseEvent): void {
    if (disabled || loading) return;
    onclick?.(e);
  }
</script>

<!-- svelte-ignore a11y_consider_explicit_label — label is passed via ariaLabel prop -->
<button
  {type}
  {title}
  aria-label={ariaLabel}
  aria-busy={loading ? 'true' : undefined}
  aria-pressed={pressed === undefined ? undefined : pressed ? 'true' : 'false'}
  data-autofocus={autofocus ? '' : undefined}
  data-testid={testId}
  disabled={disabled || loading}
  onclick={handleClick}
  class={[
    'inline-flex items-center justify-center rounded-[var(--radius-control)]',
    'font-medium cursor-pointer select-none',
    'transition-[color,background-color,border-color,opacity,transform] duration-150',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-0',
    'disabled:cursor-not-allowed disabled:opacity-50',
    'active:scale-[0.98]',
    SIZE[size],
    // Pressed takes precedence so the toggle-state visual isn't
    // fighting the variant's rest palette.
    pressed ? PRESSED_CLASSES : VARIANT[variant],
    className,
  ].join(' ')}
>
  {#if loading}
    <span
      class="inline-block size-3 animate-spin rounded-full border border-current border-r-transparent"
      aria-hidden="true"
    ></span>
  {:else if leading}
    {@render leading()}
  {/if}
  {#if children}
    {@render children()}
  {/if}
  {#if trailing && !loading}
    {@render trailing()}
  {/if}
</button>
