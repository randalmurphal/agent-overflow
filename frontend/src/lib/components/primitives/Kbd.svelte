<script lang="ts">
  /*
   * Styled <kbd> for shortcut hints. Unifies the ad-hoc spans and
   * inlined pills used today (SidebarSearch's `⌘K`, PaletteResultRow's
   * trailing kbd, KeybindingsCheatSheet rows, MenuItem kbd column).
   *
   * Accepts children so callers can pass a string or a composite like
   * "⌘⇧K". The mono font here is deliberate — shortcut glyphs read
   * clearer at small sizes when they're not in the UI sans.
   */
  import type { Snippet } from 'svelte';

  interface Props {
    class?: string;
    children: Snippet;
  }

  let { class: className = '', children }: Props = $props();
</script>

<kbd
  class={[
    'inline-flex items-center justify-center',
    'rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/70',
    'px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.08em]',
    'text-fg-subtle shrink-0',
    className,
  ].join(' ')}
>
  {@render children()}
</kbd>
