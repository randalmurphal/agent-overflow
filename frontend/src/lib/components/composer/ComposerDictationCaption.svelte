<script lang="ts">
  // What a live recording has heard so far, floated above the composer
  // card while the user speaks. It is positioned out of flow and never
  // inside the card, so the input, the toolbar, Send and the mic keep
  // their geometry; its text column is the input's own (the card's border
  // plus the input row's inset), and it shows the tail of the text, at
  // most two lines.

  import type { DictationSession } from '../../dictation/dictationSession.svelte';

  let { session }: { session: DictationSession } = $props();
</script>

{#if session.heard !== ''}
  <div
    data-testid="composer-dictation-caption"
    aria-live="polite"
    class="pointer-events-none absolute inset-x-0 bottom-full mb-1.5 flex max-h-[calc(2*1.5*0.75rem+0.75rem)] items-end overflow-hidden rounded-[var(--radius-field)] border border-border-subtle bg-card px-5 py-1.5 shadow-sheet"
  >
    <p class="flex items-baseline gap-2 text-[0.75rem] leading-normal text-fg-muted">
      <span aria-hidden="true" class="size-1.5 shrink-0 translate-y-[-1px] rounded-full bg-error"></span>
      <span data-testid="composer-dictation-caption-text">{session.heard}</span>
    </p>
  </div>
{/if}
