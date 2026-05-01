<script lang="ts">
  /*
   * Body of the diff sidebar — owns the file-level virtualizer
   * (IntersectionObserver root) and the cross-file tokenization
   * coordinator. State (expandedFiles, scrollTop) is hoisted to the
   * parent `DiffSidebar` so it can be persisted via the pane's
   * per-thread snapshot map; this body is otherwise a pure
   * orchestrator.
   *
   * Why dispatch lives here, not in DiffSidebarFile:
   *   - one tokenize call per (visible-set × language), instead of
   *     N parallel calls — fewer worker round-trips for multi-file
   *     diffs
   *   - DiffSidebarFile becomes a pure renderer with no singleton
   *     access (testable in isolation)
   *   - the visible set is the natural input for "what to tokenize
   *     next," which the virtualizer already tracks
   */
  import { onDestroy, onMount, untrack } from 'svelte';
  import { createFileVirtualizer } from '../../utils/diffSidebarVirtualizer.svelte';
  import { stripPatchLinePrefix, type PatchFile, type PatchLine } from '../../utils/patchFiles';
  import { languageFromPath } from '../../utils/diffLanguage';
  import { getSharedDiffHighlighterPool, type DiffTheme } from '../../utils/diffHighlighterPool';
  import { tokenCacheKeyFromSig, TOKENIZE_MAX_LINE_LENGTH } from '../../utils/tokenCache';
  import { getSharedReactiveTokenCache } from '../../utils/tokenCacheReactive.svelte';
  import { getDiffTheme } from '../../stores/diffTheme.svelte';
  import { patchLineSourceKey } from '../../utils/patchLineHash';
  import { addToast } from '../../stores/toast.svelte';
  import type { DiffViewMode } from '../../stores/diffPanel.svelte';
  import DiffSidebarFile from './DiffSidebarFile.svelte';

  // App-wide one-shot guard for tokenize-failure surfacing — once a
  // language has shown a degraded-highlight toast, we don't keep
  // popping it for each subsequent file with the same language.
  // Module-scope so all DiffSidebarBody instances share the set.
  const warnedTokenizeLanguages = new Set<string>();

  interface Props {
    files: PatchFile[];
    focusFilePath: string | undefined;
    threadId: string;
    workspacePath: string;
    viewMode: DiffViewMode;
    wordWrap: boolean;
    expandedFiles: string[];
    initialScrollTop: number;
    onToggleFile: (path: string) => void;
    onExpandAll: () => void;
    onCollapseAll: () => void;
    onScroll: (scrollTop: number) => void;
  }

  let {
    files,
    focusFilePath,
    threadId,
    workspacePath,
    viewMode,
    wordWrap,
    expandedFiles,
    initialScrollTop,
    onToggleFile,
    onExpandAll,
    onCollapseAll,
    onScroll,
  }: Props = $props();

  const virtualizer = createFileVirtualizer();
  const pool = getSharedDiffHighlighterPool();
  const cache = getSharedReactiveTokenCache();
  let theme: DiffTheme = $derived(getDiffTheme());

  // Concurrent-dispatch dedupe. Fast scrolling can re-fire the
  // dispatch effect while an earlier dispatch is still awaiting
  // the worker. Without this guard, the second dispatch sees an
  // empty cache for lines the first is already tokenizing and
  // re-queues them — duplicate work behind the worker's serial
  // queue. Keyed `(theme, lang, sourceKey)` so a theme flip
  // doesn't dedupe across themes.
  const inFlightKeys = new Set<string>();

  // Theme transition: evict the prior theme's tokens so the cache's
  // working set is the active theme only. Run as a side-effect
  // (not inside the `$derived(getDiffTheme())` read) — Svelte 5
  // forbids state mutations during derived recomputation, so the
  // theme store deliberately keeps `getDiffTheme()` pure and we
  // own the eviction here.
  let lastSeenTheme: DiffTheme | null = null;
  $effect(() => {
    const t = theme;
    if (lastSeenTheme !== null && lastSeenTheme !== t) {
      const prev = lastSeenTheme;
      untrack(() => cache.evictTheme(prev));
    }
    lastSeenTheme = t;
  });

  let scrollRoot: HTMLElement | undefined = $state(undefined);
  let initialScrollApplied = false;

  let expandedSet = $derived(new Set(expandedFiles));
  let filesByPath = $derived(new Map(files.map((file) => [file.path, file])));

  onMount(() => {
    if (scrollRoot) virtualizer.init(scrollRoot);
  });

  onDestroy(() => {
    virtualizer.destroy();
  });

  // Apply initial scroll position once after the body mounts. Needs
  // to wait one frame so file children commit their measured heights.
  $effect(() => {
    const root = scrollRoot;
    if (!root || initialScrollApplied) return;
    untrack(() => {
      requestAnimationFrame(() => {
        if (!scrollRoot || initialScrollApplied) return;
        if (initialScrollTop > 0) {
          scrollRoot.scrollTop = initialScrollTop;
        } else if (focusFilePath) {
          const target = scrollRoot.querySelector(
            `[data-file-path="${CSS.escape(focusFilePath)}"]`,
          );
          if (target instanceof HTMLElement) {
            target.scrollIntoView({ block: 'start', behavior: 'auto' });
          }
        }
        initialScrollApplied = true;
      });
    });
  });

  let scrollDispatchScheduled = false;
  function handleScroll(): void {
    if (!scrollRoot || scrollDispatchScheduled) return;
    scrollDispatchScheduled = true;
    requestAnimationFrame(() => {
      scrollDispatchScheduled = false;
      if (!scrollRoot) return;
      onScroll(scrollRoot.scrollTop);
    });
  }

  let allExpanded = $derived(expandedSet.size === files.length && files.length > 0);

  // ── Cross-file tokenization coordinator ──
  //
  // Re-runs whenever the visible set, theme, or files change. Walks
  // visible & expanded files (only those actually rendering content
  // need tokens), groups uncached lines by language, and dispatches
  // one tokenize call per language. Unknown / failed languages
  // skip the dispatch and render plain text.
  $effect(() => {
    const visiblePaths = virtualizer.visiblePaths;
    const t = theme;
    const expandedNow = expandedSet;
    const fileMap = filesByPath;
    if (visiblePaths.size === 0 || fileMap.size === 0) return;
    untrack(() => {
      void dispatchVisibleFileTokens(visiblePaths, expandedNow, fileMap, t);
    });
  });

  async function dispatchVisibleFileTokens(
    visiblePaths: ReadonlySet<string>,
    expanded: ReadonlySet<string>,
    fileMap: ReadonlyMap<string, PatchFile>,
    targetTheme: DiffTheme,
  ): Promise<void> {
    // Walk visible-and-expanded files. For each line, compute its
    // memoized source key, check the cache + the in-flight set, and
    // queue if it's new work. Each entry in `byLang` maps a sourceKey
    // to its source text — the cache writeback below uses the source
    // key, not the text, so we don't pay re-hash cost on writes.
    const byLang = new Map<string, Map<string, string>>();
    const claimed: string[] = [];

    for (const path of visiblePaths) {
      if (!expanded.has(path)) continue;
      const file = fileMap.get(path);
      if (!file) continue;
      const lang = languageFromPath(file.path);
      if (lang === 'plaintext') continue;

      for (const line of file.lines) {
        if (line.type === 'meta') continue;
        const text = stripPatchLinePrefix(line);
        if (text.length === 0 || text.length > TOKENIZE_MAX_LINE_LENGTH) continue;
        const sourceKey = patchLineSourceKey(line);
        const cacheKey = tokenCacheKeyFromSig(threadId, targetTheme, lang, sourceKey);
        if (cache.get(cacheKey) !== undefined) continue;
        if (inFlightKeys.has(cacheKey)) continue;

        let langLines = byLang.get(lang);
        if (!langLines) {
          langLines = new Map<string, string>();
          byLang.set(lang, langLines);
        }
        if (langLines.has(sourceKey)) continue;
        langLines.set(sourceKey, text);
        inFlightKeys.add(cacheKey);
        claimed.push(cacheKey);
      }
    }
    if (byLang.size === 0) return;

    // Sequential dispatch by language: the worker is single-threaded
    // so parallel posts would just queue. Sequential keeps stack
    // traces and error attribution clean if a grammar fails.
    try {
      for (const [lang, langLines] of byLang) {
        const sourceKeys = Array.from(langLines.keys());
        const lines = Array.from(langLines.values());
        try {
          const tokens = await pool.tokenize({ lines, lang, theme: targetTheme });
          for (let i = 0; i < sourceKeys.length; i += 1) {
            const sk = sourceKeys[i];
            const lineTokens = tokens[i];
            if (sk !== undefined && lineTokens !== undefined) {
              cache.set(tokenCacheKeyFromSig(threadId, targetTheme, lang, sk), lineTokens);
            }
          }
        } catch (err) {
          reportTokenizeFailure(lang, err);
        }
      }
    } finally {
      // Always release in-flight claims so a follow-up dispatch
      // (e.g. theme flip while the previous batch was awaiting)
      // can re-queue these keys for the new theme.
      for (const key of claimed) inFlightKeys.delete(key);
    }
  }

  function reportTokenizeFailure(lang: string, err: unknown): void {
    console.warn(`Diff tokenize failed for lang=${lang}:`, err);
    if (warnedTokenizeLanguages.has(lang)) return;
    warnedTokenizeLanguages.add(lang);
    addToast('warning', `Syntax highlighting unavailable for ${lang}`);
  }
</script>

<div class="flex items-center justify-between px-3 py-1.5 border-b border-border-subtle text-[11px] text-text-secondary shrink-0">
  <span data-testid="diff-sidebar-file-count">{files.length} {files.length === 1 ? 'file' : 'files'}</span>
  {#if files.length > 1}
    <button
      type="button"
      onclick={() => (allExpanded ? onCollapseAll() : onExpandAll())}
      data-testid="diff-sidebar-toggle-all"
      class="rounded px-1.5 py-0.5 hover:bg-surface-2/40 hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {allExpanded ? 'Collapse all' : 'Expand all'}
    </button>
  {/if}
</div>

<div
  bind:this={scrollRoot}
  data-testid="diff-sidebar-body"
  onscroll={handleScroll}
  class="flex-1 overflow-y-auto px-2 py-2"
>
  {#each files as file (file.path)}
    <DiffSidebarFile
      {file}
      expanded={expandedSet.has(file.path)}
      {threadId}
      {workspacePath}
      {viewMode}
      {wordWrap}
      {theme}
      {virtualizer}
      onToggle={onToggleFile}
    />
  {/each}
</div>
