<script lang="ts">
  // DirectoryBrowser: path input + listbox + keyboard nav.
  //
  // The browse / debounce / typeahead browser lives in
  // `directoryBrowser.svelte.ts`. This shell owns the markup + the
  // listbox keyboard handler. Behaviours:
  //
  //  - ArrowUp / ArrowDown move the highlight.
  //  - Enter on a directory row drills in.
  //  - Backspace (outside the path input) goes to the parent directory.
  //  - Typing in the path input debounces 120ms before calling
  //    BrowseDirectory with the raw string — the backend handles "~",
  //    relative, and absolute forms.
  //  - `onSelect(path)` fires whenever the browser commits to a path
  //    (either via drill-in or direct path-input text); the parent uses
  //    the latest value to drive its Add button.

  import { onDestroy, untrack } from 'svelte';
  import { createDirectoryBrowser } from './directoryBrowserState.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  interface Props {
    initialPath?: string;
    /** Fires every time the current listed path changes (including when
     * the user types a path directly). Parent is expected to use the
     * latest value as the "pending add" target. */
    onSelect?: (path: string) => void;
    /** Fires when the user commits a file row. Omit to retain directory-only behavior. */
    onSelectFile?: (path: string) => void;
  }

  let { initialPath = '~', onSelect, onSelectFile }: Props = $props();

  // Snapshot the initial path once. `untrack` tells Svelte we don't want
  // this $state init to re-fire if the parent passes a new initialPath —
  // after mount the path is user-driven.
  const startingPath = untrack(() => initialPath);

  const browser = createDirectoryBrowser({
    initialPath: startingPath,
    // Forward to the prop via a closure so Svelte doesn't flag the prop
    // reference as a one-shot capture. The wrapper is cheap; the alternative
    // is stashing the reference in a local $state, which just moves the
    // complaint to a derived.
    onSelect: (path) => onSelect?.(path),
  });

  let listboxEl: HTMLUListElement | undefined = $state(undefined);

  $effect(() => {
    browser.mount();
    // onDestroy fires too late for $effect teardown in some test setups;
    // we still register it below for component unmount.
  });

  onDestroy(() => {
    browser.destroy();
  });

  function handlePathInput(e: Event): void {
    browser.handlePathInput((e.target as HTMLInputElement).value);
  }

  function handlePathKeydown(e: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing a CJK path segment;
    // navigating here would use the pre-composition path.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      browser.handlePathEnter();
    }
  }

  function scrollHighlightIntoView(): void {
    if (!listboxEl) return;
    const row = listboxEl.querySelector<HTMLLIElement>(
      `[data-index="${browser.highlight}"]`,
    );
    row?.scrollIntoView({ block: 'nearest' });
  }

  function selectFile(name: string): void {
    const listing = browser.listing;
    if (!listing || !onSelectFile) return;
    const separator = listing.separator || '/';
    const prefix = listing.path.endsWith(separator) ? listing.path : listing.path + separator;
    onSelectFile(prefix + name);
  }

  function handleListKeydown(e: KeyboardEvent): void {
    const listing = browser.listing;
    if (!listing) return;
    const entries = listing.entries;
    if (entries.length === 0) {
      if (e.key === 'Backspace') {
        e.preventDefault();
        void browser.goToParent();
      }
      return;
    }

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        browser.setHighlight((browser.highlight + 1) % entries.length);
        scrollHighlightIntoView();
        return;
      case 'ArrowUp':
        e.preventDefault();
        browser.setHighlight(
          (browser.highlight - 1 + entries.length) % entries.length,
        );
        scrollHighlightIntoView();
        return;
      case 'Home':
        e.preventDefault();
        browser.setHighlight(0);
        scrollHighlightIntoView();
        return;
      case 'End':
        e.preventDefault();
        browser.setHighlight(entries.length - 1);
        scrollHighlightIntoView();
        return;
      case 'Enter': {
        const entry = entries[browser.highlight];
        if (entry) {
          e.preventDefault();
          if (entry.isDir) void browser.drillInto(entry);
          else selectFile(entry.name);
        }
        return;
      }
      case 'Backspace':
        e.preventDefault();
        void browser.goToParent();
        return;
    }
  }
</script>

<div class="flex flex-col gap-2 min-h-0">
  <label class="flex flex-col gap-1 text-xs text-text-secondary">
    <span class="sr-only">Path</span>
    <input
      type="text"
      value={browser.pathText}
      oninput={handlePathInput}
      onkeydown={handlePathKeydown}
      aria-label="Path"
      data-testid="directory-browser-path"
      data-autofocus
      autocomplete="off"
      spellcheck="false"
      class="w-full rounded-md border border-border bg-surface-0 px-3 py-1.5 text-xs font-mono text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50"
    />
  </label>

  {#if browser.error}
    <div
      role="alert"
      data-testid="directory-browser-error"
      class="rounded-md border border-error/40 bg-error/10 px-3 py-2 text-xs text-error"
    >
      {browser.error}
    </div>
  {/if}

  <ul
    bind:this={listboxEl}
    role="listbox"
    aria-label="Directory Entries"
    tabindex={0}
    onkeydown={handleListKeydown}
    data-testid="directory-browser-list"
    class="flex-1 min-h-[220px] max-h-[360px] overflow-y-auto rounded-md border border-border/60 bg-surface-0/60 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    {#if browser.loading && !browser.listing}
      <li class="px-3 py-2 text-xs text-text-secondary/70" aria-hidden="true">
        Loading…
      </li>
    {:else if browser.noMatches}
      <li
        class="px-3 py-2 text-xs text-text-secondary/70"
        data-testid="directory-browser-no-matches"
        aria-hidden="true"
      >
        No Matches
      </li>
    {:else if browser.listing && browser.listing.entries.length === 0}
      <li class="px-3 py-2 text-xs text-text-secondary/70" aria-hidden="true">
        Empty Directory
      </li>
    {:else if browser.listing}
      {#each browser.listing.entries as entry, i (entry.name)}
        <!-- svelte-ignore a11y_click_events_have_key_events — parent listbox handles keyboard -->
        <li
          role="option"
          aria-selected={browser.highlight === i}
          data-index={i}
          data-testid="directory-browser-entry"
          data-is-dir={entry.isDir ? 'true' : 'false'}
          onclick={() => {
            browser.setHighlight(i);
            if (entry.isDir) void browser.drillInto(entry);
            else selectFile(entry.name);
          }}
          class="flex items-center gap-2 px-3 py-1 text-xs cursor-pointer select-none
            {browser.highlight === i
              ? 'bg-accent/15 text-text-primary'
              : 'text-text-secondary hover:bg-surface-2/40 hover:text-text-primary'}"
        >
          <svg
            class="h-3.5 w-3.5 shrink-0 {entry.isDir
              ? 'text-accent/80'
              : 'text-text-secondary/60'}"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
            aria-hidden="true"
          >
            {#if entry.isDir}
              <path d="M20 19a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h7a2 2 0 0 1 2 2z" />
            {:else}
              <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
              <polyline points="14 2 14 8 20 8" />
            {/if}
          </svg>
          <span class="truncate flex-1 {entry.hidden ? 'opacity-60' : ''}">
            {entry.name}
          </span>
          {#if entry.isRepo}
            <span
              class="shrink-0 text-[0.5625rem] font-semibold uppercase tracking-wide text-accent/80"
              title="Git Repository"
              aria-label="Git Repository"
            >
              git
            </span>
          {/if}
        </li>
      {/each}
      {#if browser.listing.truncated}
        <li
          class="px-3 py-2 text-[0.6875rem] text-text-secondary/60 border-t border-border/50"
          aria-hidden="true"
        >
          …more entries hidden. Refine the path above to narrow the listing.
        </li>
      {/if}
    {/if}
  </ul>

  <p
    class="text-[0.625rem] text-text-secondary/60 flex flex-wrap gap-x-3 gap-y-1"
    aria-hidden="true"
  >
    <span><kbd class="font-mono">↑↓</kbd> Navigate</span>
    <span><kbd class="font-mono">Enter</kbd> {onSelectFile ? 'Open / select' : 'Drill in'}</span>
    <span><kbd class="font-mono">Backspace</kbd> Back</span>
    <span><kbd class="font-mono">Esc</kbd> Close</span>
  </p>
</div>
