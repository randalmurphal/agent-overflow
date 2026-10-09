<script lang="ts">
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import { stripPatchLinePrefix, type PatchDisplayRow } from '../../utils/patchFiles';
  import { gutterTintClass, lineTintClass, type LineTintType } from '../../utils/diffLineTint';
  import type { ReviewFile } from '../../utils/patchStore';
  import { threadContextRows } from '../../utils/reviewThreadContext';
  import Icon from '../primitives/Icon.svelte';
  import type { ReviewThread } from '../../types/models';

  // The code a thread was written against, folded under the card header:
  // the anchored line and the rows above it, read from the loaded diff.
  // Closed by default; opening reads the rows, and a row over evicted
  // text is read again through the body's residency pin.

  interface Props {
    file: ReviewFile;
    thread: ReviewThread;
  }

  let { file, thread }: Props = $props();

  let open = $state(false);
  let rows: PatchDisplayRow[] = $state([]);
  let lost = $state(false);

  async function load(): Promise<void> {
    const first = threadContextRows(file, thread);
    if (!first) {
      rows = [];
      return;
    }
    if (first.complete) {
      rows = first.rows;
      return;
    }
    try {
      rows = await file.body.whenResident(
        () => threadContextRows(file, thread)?.rows ?? [],
        first.lines.start,
        first.lines.end,
      );
    } catch {
      // The text is gone for good (PatchTextLost): say so instead of
      // showing blank lines.
      lost = true;
    }
  }

  function toggle(): void {
    open = !open;
    if (open && rows.length === 0) void load();
  }

  function tintOf(row: PatchDisplayRow): LineTintType {
    if (row.line.type === 'add') return 'add';
    if (row.line.type === 'del') return 'del';
    return 'context';
  }

  function lineNumber(row: PatchDisplayRow): number {
    return row.newLine > 0 ? row.newLine : row.oldLine;
  }

  const span = $derived.by(() => {
    const first = rows[0];
    const last = rows[rows.length - 1];
    if (!first || !last) return '';
    const a = lineNumber(first);
    const b = lineNumber(last);
    return a === b ? `line ${a}` : `lines ${a}–${b}`;
  });
</script>

<div class="mx-3.5 mt-3 overflow-hidden rounded-[var(--radius-field)] border border-border-subtle bg-surface-0" data-testid="review-thread-context">
  <button
    type="button"
    class="flex w-full items-center gap-1.5 px-2.5 py-1.5 text-left text-[0.6875rem] text-fg-muted hover:text-fg"
    aria-expanded={open}
    title={open ? 'Hide the code' : 'Show the code this thread is on'}
    onclick={toggle}
  >
    <Icon icon={ChevronRight} size={11} class="shrink-0 transition-transform duration-100 {open ? 'rotate-90' : ''}" />
    <span class="min-w-0 truncate font-mono">{thread.path}</span>
    {#if open && span}
      <span class="shrink-0">· {span}</span>
    {/if}
  </button>
  {#if open}
    <div class="border-t border-border-subtle py-1 font-mono text-[0.75rem] leading-5">
      {#if lost}
        <div class="px-2.5 py-1 text-fg-subtle">The diff text is no longer available.</div>
      {:else if rows.length === 0}
        <div class="px-2.5 py-1 text-fg-subtle">Reading…</div>
      {:else}
        {#each rows as row (row.id)}
          {@const tint = tintOf(row)}
          {@const anchored = row === rows[rows.length - 1]}
          <div class="flex {lineTintClass(tint)} {anchored ? 'shadow-[inset_2px_0_0_var(--color-success)]' : ''}">
            <span class="w-11 shrink-0 select-none pr-2 text-right tabular-nums {gutterTintClass(tint)}">{lineNumber(row) || ''}</span>
            <span class="w-4 shrink-0 select-none {gutterTintClass(tint)}">{row.line.type === 'add' ? '+' : row.line.type === 'del' ? '-' : ' '}</span>
            <span class="min-w-0 flex-1 overflow-hidden text-ellipsis whitespace-pre pr-2 text-fg">{row.pending ? '' : stripPatchLinePrefix(row.line)}</span>
          </div>
        {/each}
      {/if}
    </div>
  {/if}
</div>
