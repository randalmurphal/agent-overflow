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
   * compact segmented controls).
   *
   * The classes come from `buttonClasses.ts`, shared with controls that
   * must not be a native button.
   */
  import type { Snippet } from 'svelte';
  import { buttonClasses, type ButtonSize, type ButtonVariant } from './buttonClasses';

  type ButtonType = 'button' | 'submit' | 'reset';

  interface Props {
    variant?: ButtonVariant;
    size?: ButtonSize;
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
  class={buttonClasses({ variant, size, pressed, class: className })}
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
