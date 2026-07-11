<script lang="ts">
  // Icon-only button. Consistent sizing + hit area + focus ring so the
  // toolbar pickers (model, effort, mode, access) and sidebar affordances
  // share one visual language. Callers pass their icon markup through the
  // `children` snippet; we own chrome (size, border, hover, aria-label).
  //
  // `label` is both the aria-label (required for screen readers on
  // icon-only controls) AND the native title, so a hovering user sees the
  // same thing the AT user hears.

  import type { Snippet } from 'svelte';

  interface Props {
    label: string;
    disabled?: boolean;
    /** Optional `data-testid` stamp — mirrors Button's `testId` so icon-only
     *  controls can migrate to this primitive without losing their test hook. */
    testId?: string;
    onClick?: (e: MouseEvent) => void;
    /** Raw pointerdown passthrough. Close-the-pane affordances use it to
     *  stopPropagation so PaneHost's pointer-focus handler doesn't treat
     *  the click as a focus transition — which would smooth-scroll a pane
     *  that is about to be destroyed (see PaneCloseButton's rationale). */
    onPointerDown?: (e: PointerEvent) => void;
    size?: 'sm' | 'md';
    variant?: 'ghost' | 'subtle';
    ariaHaspopup?: 'dialog' | 'menu' | 'listbox' | 'tree' | 'grid' | boolean;
    ariaExpanded?: boolean;
    children: Snippet;
  }

  let {
    label,
    disabled = false,
    testId,
    onClick,
    onPointerDown,
    size = 'md',
    variant = 'ghost',
    ariaHaspopup,
    ariaExpanded,
    children,
  }: Props = $props();

  // Two fixed hit-area sizes keep vertical rhythm inside the toolbar. The
  // inner icon sizing is the caller's responsibility — the button just
  // guarantees the box is consistent.
  const SIZE_CLASSES: Record<NonNullable<Props['size']>, string> = {
    sm: 'h-7 w-7',
    md: 'h-8 w-8',
  };

  // Ghost = transparent until hover. Subtle = shows a low-contrast fill
  // at rest so it reads as a control rather than blank space. Both share
  // the same hover tint so hover feedback is identical.
  const VARIANT_CLASSES: Record<NonNullable<Props['variant']>, string> = {
    ghost: 'bg-transparent hover:bg-surface-2/60',
    subtle: 'bg-surface-2/40 hover:bg-surface-2/70',
  };

  function handleClick(e: MouseEvent) {
    if (disabled) return;
    onClick?.(e);
  }
</script>

<button
  type="button"
  {disabled}
  aria-label={label}
  aria-haspopup={ariaHaspopup}
  aria-expanded={ariaExpanded}
  title={label}
  onclick={handleClick}
  onpointerdown={onPointerDown}
  class={[
    'inline-flex items-center justify-center rounded-md text-text-secondary',
    'transition-colors cursor-pointer',
    'hover:text-text-primary',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
    'disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent disabled:hover:text-text-secondary',
    SIZE_CLASSES[size],
    VARIANT_CLASSES[variant],
  ].join(' ')}
  data-icon-button
  data-testid={testId}
>
  {@render children()}
</button>
