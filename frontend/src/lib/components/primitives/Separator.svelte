<script lang="ts">
  /*
   * Hairline separator. Replaces scattered `<span class="h-px bg-border">`
   * and `<div class="border-t">` patterns, plus the pipe characters
   * ("|") used to visually split toolbar groups.
   *
   * Orientation "vertical" is intended for inline use inside a flex
   * row — the caller sizes the height via `class="h-4"` etc. since a
   * 1×N hairline is rarely exactly what's wanted by default.
   */

  interface Props {
    orientation?: 'horizontal' | 'vertical';
    opacity?: number;
    class?: string;
  }

  let {
    orientation = 'horizontal',
    opacity = 1,
    class: className = '',
  }: Props = $props();

  // color-mix blends the --border token with transparent so the
  // caller's opacity prop produces a predictable result regardless of
  // whether border is an opaque or semi-transparent color in the
  // current theme.
  const style = $derived(
    `background: color-mix(in oklab, var(--border) ${opacity * 100}%, transparent);`,
  );

  const dimension = $derived(
    orientation === 'horizontal' ? 'h-px w-full' : 'w-px h-full',
  );
</script>

<span
  role="separator"
  aria-orientation={orientation}
  class={[dimension, 'shrink-0', className].join(' ')}
  {style}
></span>
