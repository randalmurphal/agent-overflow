<script lang="ts">
  /*
   * Pure file renderer for the diff sidebar. No singleton access:
   * tokenization is dispatched by DiffSidebarBody, theme arrives as
   * a prop, the file just reads tokens out of the shared cache.
   *
   * Two visibility tiers:
   *   - File-level: registered with the parent's IntersectionObserver
   *     virtualizer. Out-of-viewport (+ overscan) files render a
   *     placeholder sized via the last measured height.
   *   - Line-level: lines render with their line-tint background as
   *     soon as the file becomes visible; once the body's dispatch
   *     coordinator has populated the cache, tokens fade in (a
   *     module-level cache-generation counter makes the cache reads
   *     reactive).
   */
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import EditorLink from '../common/EditorLink.svelte';
  import Icon from '../primitives/Icon.svelte';
  import type { PatchFile, PatchLine, SplitDiffRow } from '../../utils/patchFiles';
  import { buildSplitRows, stripPatchLinePrefix } from '../../utils/patchFiles';
  import { fontStyleClass, lineTintClass } from '../../utils/diffLineTint';
  import type { FileVirtualizerHandle } from '../../utils/diffSidebarVirtualizer.svelte';
  import { languageFromPath } from '../../utils/diffLanguage';
  import type { DiffTheme } from '../../utils/diffHighlighterPool';
  import type { DiffViewMode } from '../../stores/diffPanel.svelte';
  import { tokenCacheKeyFromSig, type LineToken, TOKENIZE_MAX_LINE_LENGTH } from '../../utils/tokenCache';
  import {
    getSharedTokenCache,
    getSharedTokenCacheGeneration,
  } from '../../utils/tokenCacheReactive.svelte';
  import { patchLineSourceKey } from '../../utils/patchLineHash';

  interface Props {
    file: PatchFile;
    expanded: boolean;
    threadId: string;
    viewMode: DiffViewMode;
    wordWrap: boolean;
    /** Resolved Shiki theme name from the parent. */
    theme: DiffTheme;
    virtualizer: FileVirtualizerHandle;
    onToggle: (path: string) => void;
  }

  let { file, expanded, threadId, viewMode, wordWrap, theme, virtualizer, onToggle }: Props = $props();

  let containerEl: HTMLElement | undefined = $state(undefined);

  $effect(() => {
    if (!containerEl) return;
    const path = file.path;
    virtualizer.register(path, containerEl);
    return () => virtualizer.unregister(path);
  });

  // File-level visibility gate. `isVisible(unregistered) === false`
  // until the IntersectionObserver fires its first batch — meaning
  // a freshly-registered file shows the placeholder skeleton
  // briefly until the observer catches up (typically <16ms).
  let inViewport = $derived(virtualizer.isVisible(file.path));
  let cachedHeight = $derived(virtualizer.height(file.path));
  let shouldRender = $derived(expanded && inViewport);

  let wrapClass = $derived(wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');

  let kindBadgeClasses = $derived.by(() => {
    switch (file.kind) {
      case 'added': return 'bg-success/20 text-success';
      case 'deleted': return 'bg-error/20 text-error';
      case 'renamed': return 'bg-accent/30 text-accent';
      default: return 'bg-warning/20 text-warning';
    }
  });

  // Filter out the `meta` lines (diff/index/+++/--- headers) so the
  // body shows hunks + content. Hunk headers (`@@`) are kept — they
  // are useful navigational anchors.
  let displayLines = $derived(file.lines.filter((line) => {
    if (line.type !== 'meta') return true;
    return line.content.startsWith('@@');
  }));

  let splitRows: SplitDiffRow[] = $derived(viewMode === 'split' ? buildSplitRows(displayLines) : []);

  const cache = getSharedTokenCache();
  let lang = $derived(languageFromPath(file.path));

  function getTokens(line: PatchLine): LineToken[] | null {
    // Reactive dep on the module-level generation counter — when
    // DiffSidebarBody's dispatch lands new tokens (or the diffTheme
    // store evicts a prior theme), every visible line re-evaluates
    // its lookup against the now-populated cache.
    getSharedTokenCacheGeneration();
    if (line.type === 'meta') return null;
    // Filter out lines we'd never tokenize before paying the
    // (memoized) hash cost.
    const text = stripPatchLinePrefix(line);
    if (text.length === 0 || text.length > TOKENIZE_MAX_LINE_LENGTH) return null;
    return cache.get(tokenCacheKeyFromSig(threadId, theme, lang, patchLineSourceKey(line))) ?? null;
  }

  // HTML5 forbids whitespace in id values, and file paths can carry
  // spaces or other awkward characters. Normalize to a stable
  // identifier the aria-controls attribute (and Svelte test queries)
  // can rely on.
  let safeId = $derived(file.path.replace(/[^A-Za-z0-9_-]/g, '_'));
</script>

<section
  bind:this={containerEl}
  data-file-path={file.path}
  data-testid="diff-sidebar-file"
  class="rounded-[var(--radius-control)] border border-border-subtle bg-card/15 mb-2"
>
  <header class="group/diff-sidebar-file flex items-center gap-2 px-2.5 py-1.5 text-[13px] hover:bg-surface-2/25 transition-colors">
    <button
      type="button"
      onclick={() => onToggle(file.path)}
      aria-expanded={expanded}
      aria-controls="diff-sidebar-file-{safeId}"
      class="flex flex-1 min-w-0 items-center gap-2 text-left cursor-pointer bg-transparent border-0 p-0"
    >
      <span class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150" class:rotate-90={expanded}>
        <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
      </span>
      <span class="font-mono text-[12px] text-fg-muted truncate">{file.path}</span>
      <span class="px-1.5 py-0.5 rounded-[var(--radius-field)] text-[10px] font-medium {kindBadgeClasses}">{file.kind}</span>
      <span class="ml-auto flex gap-2 text-[11px] shrink-0 tabular-nums">
        {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
        {#if file.deletions > 0}<span class="text-error">-{file.deletions}</span>{/if}
      </span>
    </button>
    <EditorLink
      path={file.path}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/diff-sidebar-file:opacity-100 focus-visible:opacity-100"
    />
  </header>

  {#snippet lineContent(line: PatchLine)}
    {@const tokens = getTokens(line)}
    {#if line.type === 'add' || line.type === 'del'}{line.content.charAt(0)}{/if}{#if tokens && tokens.length > 0 && line.type !== 'meta'}{#each tokens as token, ti (ti)}<span style:color={token.color} class={fontStyleClass(token.fontStyle)}>{token.content}</span>{/each}{:else}{line.type === 'add' || line.type === 'del' ? line.content.slice(1) : line.content}{/if}
  {/snippet}

  {#if expanded}
    <div id="diff-sidebar-file-{safeId}" class="border-t border-border-subtle bg-surface-0/50">
      {#if shouldRender}
        {#if viewMode === 'split'}
          <div class="grid grid-cols-2 gap-px bg-border-subtle font-mono text-[11px] leading-tight {wrapClass}">
            {#each splitRows as row, i (i)}
              <div class="px-2 py-px {row.left ? lineTintClass(row.left.type) : 'bg-surface-0/40'} {row.left?.type === 'context' ? 'bg-surface-0' : ''}">
                {#if row.left}{@render lineContent(row.left)}{:else}{' '}{/if}
              </div>
              <div class="px-2 py-px {row.right ? lineTintClass(row.right.type) : 'bg-surface-0/40'} {row.right?.type === 'context' ? 'bg-surface-0' : ''}">
                {#if row.right}{@render lineContent(row.right)}{:else}{' '}{/if}
              </div>
            {/each}
          </div>
        {:else}
          <pre class="overflow-auto px-3 py-2 font-mono text-[11px] leading-tight {wrapClass}">{#each displayLines as line, i (i)}<span
              class="block {lineTintClass(line.type)}"
            >{@render lineContent(line)}
</span>{/each}</pre>
        {/if}
      {:else if cachedHeight !== undefined}
        <!-- Out-of-viewport placeholder preserves layout via measured height. -->
        <div aria-hidden="true" style="height: {cachedHeight}px" data-testid="diff-sidebar-file-placeholder"></div>
      {:else}
        <!-- First render before the IntersectionObserver tick. Use a
             modest skeleton so the layout doesn't pop. -->
        <div aria-hidden="true" class="h-12" data-testid="diff-sidebar-file-skeleton"></div>
      {/if}
    </div>
  {/if}
</section>
