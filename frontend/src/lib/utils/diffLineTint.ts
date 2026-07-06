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

export type LineTintType = 'add' | 'del' | 'meta' | 'context';

export function lineTintClass(type: LineTintType): string {
  switch (type) {
    case 'add':
      return 'bg-success/20 text-success';
    case 'del':
      return 'bg-error/20 text-error';
    case 'meta':
      return 'text-accent/70';
    default:
      return 'text-text-secondary';
  }
}

/**
 * Bit-flag → Tailwind class lookup for Shiki's `fontStyle` field
 * (0-7 bitfield: 1=italic, 2=bold, 4=underline). Module-scope const
 * so the lookup table isn't re-allocated per component instance —
 * 50 file components hosting their own copy adds up otherwise.
 */
const FONT_STYLE_CLASSES = [
  '',
  'italic',
  'font-bold',
  'italic font-bold',
  'underline',
  'italic underline',
  'font-bold underline',
  'italic font-bold underline',
] as const;

export function fontStyleClass(fontStyle: number | undefined): string {
  if (!fontStyle) return '';
  return FONT_STYLE_CLASSES[fontStyle & 7] ?? '';
}
