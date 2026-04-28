<script lang="ts">
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../../primitives/Icon.svelte';
  import EditorLink from '../../common/EditorLink.svelte';
  import { buildSplitRows, type PatchFile, type SplitDiffRow } from '../../../utils/patchFiles';
  import { lineTintClass } from '../../../utils/diffLineTint';

  interface Props {
    file: PatchFile;
    open: boolean;
    viewMode: 'stacked' | 'split';
    wordWrap: boolean;
    onToggle: () => void;
  }

  let { file, open, viewMode, wordWrap, onToggle }: Props = $props();

  // buildSplitRows is pure over file.lines, so $derived gives us a
  // per-instance cache that invalidates when the diff text changes.
  const splitRows = $derived(viewMode === 'split' && open ? buildSplitRows(file.lines) : null);

  function splitCellClass(line: PatchFile['lines'][number] | null): string {
    if (!line) return 'text-fg-muted/40';
    return lineTintClass(line.type);
  }
</script>

<section class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-card/30">
  <!--
    Header: button toggles open/closed, EditorLink sibling opens the
    file in the user's editor. Same dual-control layout used by
    DiffPreview to avoid nested interactives.
  -->
  <div class="group/diff-panel-file flex w-full items-center gap-2 px-3 py-2 hover:bg-surface-2/40">
    <button
      class="flex flex-1 min-w-0 items-center gap-2 text-left bg-transparent border-0 p-0 cursor-pointer"
      onclick={onToggle}
      data-testid="diff-panel-file-toggle"
      data-path={file.path}
    >
      <Icon icon={ChevronDown} size={14} class={open ? '' : '-rotate-90'} />
      <span class="rounded bg-accent/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.08em] text-accent">FileChange</span>
      <span class="min-w-0 flex-1 truncate font-mono text-[12px] text-fg">{file.path}</span>
      <span class="text-[11px] text-success">+{file.additions}</span>
      <span class="text-[11px] text-error">-{file.deletions}</span>
    </button>
    <EditorLink
      path={file.path}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/diff-panel-file:opacity-100 focus-visible:opacity-100"
    />
  </div>
  {#if open}
    {#if viewMode === 'split' && splitRows}
      <div class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
        {#each splitRows as row}
          <div class="grid grid-cols-2 border-b border-border-subtle/40 last:border-b-0">
            <pre class="min-w-0 border-r border-border-subtle/50 px-3 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'} {splitCellClass(row.left)}">{row.left?.content ?? ''}</pre>
            <pre class="min-w-0 px-3 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'} {splitCellClass(row.right)}">{row.right?.content ?? ''}</pre>
          </div>
        {/each}
      </div>
    {:else}
    <pre class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 p-3 font-mono text-[12px] leading-relaxed {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'}">{#each file.lines as line}<span class="block {lineTintClass(line.type)}">{line.content}
</span>{/each}</pre>
    {/if}
  {/if}
</section>
