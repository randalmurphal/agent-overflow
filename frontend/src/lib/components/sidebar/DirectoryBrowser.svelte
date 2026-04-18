<script lang="ts">
  // DirectoryBrowser: path input + listbox + keyboard nav.
  //
  // Owned state:
  //  - current listing (path, parent, entries)
  //  - highlighted index inside the listing
  //  - the path-input string
  //
  // Behaviours:
  //  - ArrowUp / ArrowDown move the highlight.
  //  - Enter on a directory row drills in.
  //  - Backspace (from anywhere except the path input when it has content)
  //    goes to the parent directory.
  //  - Typing in the path input debounces 120ms before calling
  //    BrowseDirectory with the raw string — the backend handles "~",
  //    relative, and absolute forms.
  //  - `onSelect(path)` fires whenever the browser commits to a path
  //    (either via drill-in or direct path-input text); the parent uses
  //    the latest value to drive its Add button.

  import { untrack } from 'svelte';
  import { BrowseDirectory } from '../../stores/bindings';
  import type { DirectoryEntry, DirectoryListing } from '../../types/models';

  interface Props {
    initialPath?: string;
    /** Fires every time the current listed path changes (including when
     * the user types a path directly). Parent is expected to use the
     * latest value as the "pending add" target. */
    onSelect?: (path: string) => void;
  }

  let { initialPath = '~', onSelect }: Props = $props();

  // Snapshot the initial path once. `untrack` tells Svelte we don't want
  // this $state init to re-fire if the parent ever passes a new
  // initialPath — after mount the path is user-driven.
  const startingPath = untrack(() => initialPath);

  let listing: DirectoryListing | null = $state(null);
  let highlight = $state(0);
  let pathText = $state(startingPath);
  let loading = $state(false);
  let error: string | null = $state(null);
  // noMatches goes true when the user is typing and the current input
  // doesn't resolve to a real directory. The listbox uses it to show a
  // muted "No matches" hint instead of the last-valid listing.
  let noMatches = $state(false);
  let listboxEl: HTMLUListElement | undefined = $state(undefined);

  // Debounce handle for path-text changes. Typing in the input shouldn't
  // fire one RPC per keystroke. 120ms feels snappy but quiets burst input.
  let debounceHandle: ReturnType<typeof setTimeout> | null = null;
  // Track the latest in-flight browse so a slow response from an earlier
  // path can't overwrite a newer listing.
  let browseToken = 0;

  // Pull a readable message out of whatever the binding threw. Wails
  // serializes its server-side errors as objects with {message, kind,
  // cause}; a naive String(err) prints "[object Object]" or the full
  // JSON dump. We want the human-readable message only.
  function extractErrorMessage(err: unknown): string {
    if (err instanceof Error) return err.message;
    if (typeof err === 'string') return err;
    if (err && typeof err === 'object') {
      const maybeMessage = (err as { message?: unknown }).message;
      if (typeof maybeMessage === 'string') return maybeMessage;
    }
    return 'Unknown error';
  }

  async function browse(
    path: string,
    opts: { fromTyping?: boolean } = {},
  ): Promise<void> {
    const fromTyping = opts.fromTyping ?? false;
    const token = ++browseToken;
    loading = true;
    if (!fromTyping) {
      // Only the explicit nav path (drill-in, goToParent, Enter,
      // initial mount) clears a prior error. Typing shouldn't flicker
      // the banner on or off between keystrokes.
      error = null;
    }
    try {
      const result = (await BrowseDirectory(path)) as DirectoryListing;
      if (token !== browseToken) return;
      listing = result;
      noMatches = false;
      error = null;
      // On an explicit nav (drill-in, parent, Enter, mount) we adopt
      // the server's canonical path so the input mirrors the listing.
      // While the user is still typing we leave pathText alone — the
      // server may have normalised away a trailing slash or expanded
      // "~", and clobbering the input would erase the user's cursor
      // position and the character they just pressed.
      if (!fromTyping) {
        pathText = result.path;
      }
      highlight = 0;
      onSelect?.(result.path);
    } catch (err) {
      if (token !== browseToken) return;
      if (fromTyping) {
        // Direct browse of the typed path failed. Fall back to
        // Finder-style typeahead: split the typed path into parent +
        // prefix, browse the parent, and filter entries by prefix.
        // So typing "/Users/randy/rep" shows "repos" in /Users/randy/.
        const filtered = await tryPrefixFilter(path, token);
        if (token !== browseToken) return;
        if (filtered) {
          listing = filtered;
          noMatches = filtered.entries.length === 0;
        } else {
          listing = null;
          noMatches = true;
        }
        // The typed path itself isn't committable — disable Add until
        // the user drills into a real entry. onSelect('') tells the
        // parent modal to disable its Add button.
        onSelect?.('');
        return;
      }
      error = extractErrorMessage(err);
      listing = null;
      noMatches = false;
    } finally {
      if (token === browseToken) {
        loading = false;
      }
    }
  }

  // Prefix-filter fallback used when the typed path can't be browsed
  // directly (incomplete / non-existent). Splits on the last separator:
  // the parent half is browsed, the suffix becomes a case-insensitive
  // prefix match against the parent's entries. Returns null when the
  // split isn't meaningful (no separator, empty prefix) or the parent
  // browse itself fails — the caller then shows "No matches".
  async function tryPrefixFilter(
    path: string,
    token: number,
  ): Promise<DirectoryListing | null> {
    const trimmed = path.replace(/\/+$/, '');
    const lastSep = trimmed.lastIndexOf('/');
    if (lastSep < 0) return null;
    const parent = trimmed.slice(0, lastSep) || '/';
    const prefix = trimmed.slice(lastSep + 1);
    if (!prefix) return null;
    try {
      const parentListing = (await BrowseDirectory(parent)) as DirectoryListing;
      if (token !== browseToken) return null;
      const prefixLower = prefix.toLowerCase();
      return {
        ...parentListing,
        entries: parentListing.entries.filter((e) =>
          e.name.toLowerCase().startsWith(prefixLower),
        ),
      };
    } catch {
      return null;
    }
  }

  // Initial load: fire once on mount with the starting path.
  $effect(() => {
    void browse(startingPath);
    return () => {
      if (debounceHandle) clearTimeout(debounceHandle);
    };
  });

  function handlePathInput(e: Event): void {
    pathText = (e.target as HTMLInputElement).value;
    if (debounceHandle) clearTimeout(debounceHandle);
    debounceHandle = setTimeout(() => {
      debounceHandle = null;
      void browse(pathText, { fromTyping: true });
    }, 120);
  }

  async function drillInto(entry: DirectoryEntry): Promise<void> {
    if (!entry.isDir || !listing) return;
    const sep = listing.separator || '/';
    const next = listing.path.endsWith(sep)
      ? listing.path + entry.name
      : `${listing.path}${sep}${entry.name}`;
    await browse(next);
  }

  async function goToParent(): Promise<void> {
    if (!listing || !listing.parent) return;
    await browse(listing.parent);
  }

  function handleListKeydown(e: KeyboardEvent): void {
    if (!listing) return;
    const entries = listing.entries;
    if (entries.length === 0) {
      if (e.key === 'Backspace') {
        e.preventDefault();
        void goToParent();
      }
      return;
    }

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        highlight = (highlight + 1) % entries.length;
        scrollHighlightIntoView();
        return;
      case 'ArrowUp':
        e.preventDefault();
        highlight = (highlight - 1 + entries.length) % entries.length;
        scrollHighlightIntoView();
        return;
      case 'Home':
        e.preventDefault();
        highlight = 0;
        scrollHighlightIntoView();
        return;
      case 'End':
        e.preventDefault();
        highlight = entries.length - 1;
        scrollHighlightIntoView();
        return;
      case 'Enter': {
        const entry = entries[highlight];
        if (entry && entry.isDir) {
          e.preventDefault();
          void drillInto(entry);
        }
        return;
      }
      case 'Backspace':
        e.preventDefault();
        void goToParent();
        return;
    }
  }

  function scrollHighlightIntoView(): void {
    if (!listboxEl) return;
    const row = listboxEl.querySelector<HTMLLIElement>(
      `[data-index="${highlight}"]`,
    );
    row?.scrollIntoView({ block: 'nearest' });
  }

  function handlePathKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (debounceHandle) {
        clearTimeout(debounceHandle);
        debounceHandle = null;
      }
      void browse(pathText);
    }
  }
</script>

<div class="flex flex-col gap-2 min-h-0">
  <label class="flex flex-col gap-1 text-xs text-text-secondary">
    <span class="sr-only">Path</span>
    <input
      type="text"
      value={pathText}
      oninput={handlePathInput}
      onkeydown={handlePathKeydown}
      aria-label="Path"
      data-testid="directory-browser-path"
      autocomplete="off"
      spellcheck="false"
      class="w-full rounded-md border border-border bg-surface-0 px-3 py-1.5 text-xs font-mono text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50"
    />
  </label>

  {#if error}
    <div
      role="alert"
      data-testid="directory-browser-error"
      class="rounded-md border border-error/40 bg-error/10 px-3 py-2 text-xs text-error"
    >
      {error}
    </div>
  {/if}

  <ul
    bind:this={listboxEl}
    role="listbox"
    aria-label="Directory entries"
    tabindex={0}
    onkeydown={handleListKeydown}
    data-testid="directory-browser-list"
    class="flex-1 min-h-[220px] max-h-[360px] overflow-y-auto rounded-md border border-border/60 bg-surface-0/60 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    {#if loading && !listing}
      <li class="px-3 py-2 text-xs text-text-secondary/70" aria-hidden="true">
        Loading…
      </li>
    {:else if noMatches}
      <li
        class="px-3 py-2 text-xs text-text-secondary/70"
        data-testid="directory-browser-no-matches"
        aria-hidden="true"
      >
        No matches
      </li>
    {:else if listing && listing.entries.length === 0}
      <li class="px-3 py-2 text-xs text-text-secondary/70" aria-hidden="true">
        Empty directory
      </li>
    {:else if listing}
      {#each listing.entries as entry, i (entry.name)}
        <!-- svelte-ignore a11y_click_events_have_key_events — parent listbox handles keyboard -->
        <li
          role="option"
          aria-selected={highlight === i}
          data-index={i}
          data-testid="directory-browser-entry"
          data-is-dir={entry.isDir ? 'true' : 'false'}
          onclick={() => {
            highlight = i;
            if (entry.isDir) void drillInto(entry);
          }}
          class="flex items-center gap-2 px-3 py-1 text-xs cursor-pointer select-none
            {highlight === i
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
              class="shrink-0 text-[9px] font-semibold uppercase tracking-wide text-accent/80"
              title="Git repository"
              aria-label="Git repository"
            >
              git
            </span>
          {/if}
        </li>
      {/each}
      {#if listing.truncated}
        <li
          class="px-3 py-2 text-[11px] text-text-secondary/60 border-t border-border/50"
          aria-hidden="true"
        >
          …more entries hidden. Refine the path above to narrow the listing.
        </li>
      {/if}
    {/if}
  </ul>

  <p
    class="text-[10px] text-text-secondary/60 flex flex-wrap gap-x-3 gap-y-1"
    aria-hidden="true"
  >
    <span><kbd class="font-mono">↑↓</kbd> Navigate</span>
    <span><kbd class="font-mono">Enter</kbd> Drill in</span>
    <span><kbd class="font-mono">Backspace</kbd> Back</span>
    <span><kbd class="font-mono">Esc</kbd> Close</span>
  </p>
</div>
