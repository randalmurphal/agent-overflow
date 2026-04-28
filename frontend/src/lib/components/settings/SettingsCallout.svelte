<script lang="ts">
  // SettingsCallout: thin status banner used inside settings sections —
  // "tracing change requires restart", "this URL is plaintext", etc.
  //
  // Replaces the per-section `rounded-xl border border-warning/40
  // bg-warning/10` strings which had drifted to slightly different
  // border/bg/text combinations across files.

  import type { Snippet } from 'svelte';

  let {
    tone = 'info',
    children,
  }: {
    tone?: 'info' | 'warn' | 'error';
    children: Snippet;
  } = $props();

  const toneClass = $derived(
    tone === 'error'
      ? 'border-error/40 bg-error/10 text-fg'
      : tone === 'warn'
      ? 'border-warning/40 bg-warning/10 text-fg'
      : 'border-border-subtle bg-surface-1/50 text-fg-muted',
  );

  // Soft tones (info, warn) are still informational — `role="status"`
  // matches an aria-live="polite" region. `error` is the only tone that
  // belongs in an `alert` region.
  const role = $derived(tone === 'error' ? 'alert' : 'status');
</script>

<div
  {role}
  class="rounded-[var(--radius-field)] border px-3 py-2 text-[12px] leading-relaxed {toneClass}"
>
  {@render children()}
</div>
