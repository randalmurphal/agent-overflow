/**
 * Shared line-tint classes for the diff renderers.
 *
 * Inline diff blocks and the review pane render unified-diff lines.
 * Both use the same background + foreground tints to mark
 * add/del/meta/context lines. Centralizing the class
 * strings here:
 *   - keeps the surfaces visually consistent
 *   - puts a future palette tweak (e.g. when light-mode is wired
 *     in) in a single place
 *   - lets each surface compose its own structural classes
 *     (padding, block, grid cell) without duplicating the color rules.
 */

// `marker` is conflict-view only (`utils/conflictFile.ts`): visible,
// unnumbered rows for conflict markers and fold placeholders. Regular
// diff parsing never emits it.
export type LineTintType = 'add' | 'del' | 'meta' | 'context' | 'marker';

/**
 * Row tint: BACKGROUND only for add/del. Code text keeps the normal
 * foreground (or its syntax-token colors) — coloring the whole line's
 * text green/red made untokenized lines (worker batch pending or
 * failed) render as flat neon next to syntax-colored neighbors. The
 * add/del signal comes from the background, the gutter tint, and the
 * colored +/- prefix (`prefixTintClass`).
 */
export function lineTintClass(type: LineTintType): string {
  switch (type) {
    case 'add':
      return 'bg-success/12';
    case 'del':
      return 'bg-error/12';
    case 'meta':
      return 'text-accent/70';
    case 'marker':
      return 'bg-accent/10 text-accent';
    default:
      return '';
  }
}

/** Line-number gutter tint: a step stronger than the row wash (GitHub
 * pattern) so the change column reads at a glance while the code area
 * stays quiet; numbers pick up the change hue at reduced opacity. */
export function gutterTintClass(type: LineTintType): string {
  switch (type) {
    case 'add':
      return 'bg-success/20 text-success/75';
    case 'del':
      return 'bg-error/20 text-error/75';
    default:
      return 'text-fg-subtle';
  }
}

/** Diff prefix character (`+` / `-`) foreground. */
export function prefixTintClass(type: LineTintType): string {
  if (type === 'add') return 'text-success';
  if (type === 'del') return 'text-error';
  return '';
}
