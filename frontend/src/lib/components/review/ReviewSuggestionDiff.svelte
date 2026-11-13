<script lang="ts">
  import type { Snippet } from 'svelte';
  import { gutterTintClass, lineTintClass } from '../../utils/diffLineTint';

  // A suggested change as the small diff it proposes, in the pane's diff
  // tints: the lines it replaces (when known) removed, its lines added.
  // No file header and no line numbers: it sits in its comment, under
  // the line or range the thread is on. `removed` null shows the
  // additions alone (the replaced text is not known). The footer holds
  // its state (Apply, Applied, Outdated) and a refusal's sentence. Find
  // does not read it (`data-find-skip`): the comment's searchable text is
  // the markdown around it.

  interface Props {
    removed: readonly string[] | null;
    added: readonly string[];
    /** Quiet text before the lines: why the replaced lines are not shown. */
    note?: string | null;
    footer?: Snippet;
  }

  let { removed, added, note = null, footer }: Props = $props();
</script>

<div class="my-1.5 overflow-hidden rounded-[var(--radius-field)] border border-border-subtle bg-surface-0" data-testid="review-suggestion" data-find-skip>
  <div class="py-0.5 font-mono text-[0.75rem] leading-5">
    {#each removed ?? [] as line}
      <div class="flex {lineTintClass('del')}" data-testid="review-suggestion-removed">
        <span class="w-5 shrink-0 select-none text-center {gutterTintClass('del')}">-</span>
        <span class="min-w-0 flex-1 whitespace-pre-wrap break-all pr-2 text-fg">{line}</span>
      </div>
    {/each}
    {#each added as line}
      <div class="flex {lineTintClass('add')}" data-testid="review-suggestion-added">
        <span class="w-5 shrink-0 select-none text-center {gutterTintClass('add')}">+</span>
        <span class="min-w-0 flex-1 whitespace-pre-wrap break-all pr-2 text-fg">{line}</span>
      </div>
    {/each}
    {#if added.length === 0 && (removed?.length ?? 0) === 0}
      <div class="px-2.5 text-fg-subtle">Deletes the lines.</div>
    {/if}
  </div>
  {#if note || footer}
    <div class="flex min-h-7 flex-wrap items-center gap-x-2 border-t border-border-subtle px-2 py-0.5 text-[0.6875rem] text-fg-muted">
      {#if note}<span class="min-w-0 truncate" title={note}>{note}</span>{/if}
      <span class="min-w-0 flex-1"></span>
      {@render footer?.()}
    </div>
  {/if}
</div>
