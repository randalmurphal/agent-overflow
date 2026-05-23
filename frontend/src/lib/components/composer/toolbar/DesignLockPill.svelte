<script lang="ts">
  // Composer toolbar pill shown on design threads in place of the
  // chat/plan agent toggle. Design mode locks the agent surface — the
  // tools available to the model are fixed (render_design,
  // present_options) and the thread's type cannot be changed mid-flight,
  // so an interactive toggle would be misleading. The pill exists to
  // make the locked state legible at a glance, not to act on it.

  import Palette from 'lucide-svelte/icons/palette';
  import Icon from '../../primitives/Icon.svelte';
</script>

<span
  data-testid="composer-design-lock-pill"
  aria-label="Design mode (locked)"
  title="Design mode — type is locked for the life of this thread"
  class={[
    'inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
    'px-1.5 py-1 text-[11px] text-fg-muted',
    'border border-border-subtle bg-surface-2/30',
    'shrink-0 select-none',
  ].join(' ')}
>
  <Icon icon={Palette} size={12} strokeWidth={1.75} class="opacity-80" />
  <span>Design</span>
</span>
