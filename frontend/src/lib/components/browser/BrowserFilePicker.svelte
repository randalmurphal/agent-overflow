<script lang="ts">
  import FileIcon from '@lucide/svelte/icons/file';
  import Search from '@lucide/svelte/icons/search';
  import { onDestroy, untrack } from 'svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import type { WorkspaceRef } from '../../types/git';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { isLocalEnter } from '../../utils/localEnter';
  import Icon from '../primitives/Icon.svelte';
  import Modal from '../primitives/Modal.svelte';
  import DirectoryBrowser from '../sidebar/DirectoryBrowser.svelte';
  import { createBrowserFileSearch } from './browserFileSearch.svelte';

  // Opens a file in the browser without typing its path: a search over the
  // thread's workspace (the @-mention search) and, with no query, the folder
  // view rooted at the workspace on the computer that owns it. The browser
  // opens the chosen absolute path through its own file authorization, so
  // nothing here decides what may be opened.

  let {
    workspace,
    rootPath,
    backend,
    onChoose,
    onClose,
  }: {
    workspace: WorkspaceRef | null;
    /** The workspace directory the folder view starts in. */
    rootPath: string;
    backend: BackendKey;
    onChoose: (path: string) => void;
    onClose: () => void;
  } = $props();

  const search = createBrowserFileSearch(() => workspace);
  onDestroy(() => search.dispose());
  const startPath = untrack(() => rootPath);

  let highlight = $state(0);
  let list: HTMLUListElement | undefined = $state(undefined);
  let searching = $derived(search.query.trim() !== '');

  $effect(() => {
    void search.results;
    highlight = 0;
  });

  function move(step: number): void {
    const count = search.results.length;
    if (count === 0) return;
    highlight = (highlight + step + count) % count;
    list?.querySelector<HTMLElement>(`[data-index="${highlight}"]`)?.scrollIntoView({ block: 'nearest' });
  }

  function onSearchKeydown(event: KeyboardEvent): void {
    if (isImeComposingEvent(event) || !searching) return;
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      move(1);
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      move(-1);
    } else if (isLocalEnter(event)) {
      event.preventDefault();
      const hit = search.results[highlight];
      if (hit) onChoose(hit.absolute);
    }
  }
</script>

<Modal open title="Open file" {onClose} width="md" padding="tight">
  <div class="flex min-h-0 flex-col gap-2" data-testid="browser-file-picker">
    <label class="relative flex items-center">
      <span class="sr-only">Search files</span>
      <Icon icon={Search} size={13} class="pointer-events-none absolute left-2.5 text-fg-muted" />
      <input
        type="text"
        value={search.query}
        oninput={(event) => search.setQuery(event.currentTarget.value)}
        onkeydown={onSearchKeydown}
        placeholder="Search files in this workspace"
        aria-label="Search files"
        data-testid="browser-file-search"
        data-autofocus
        autocomplete="off"
        spellcheck="false"
        class="w-full rounded-md border border-border bg-surface-0 py-1.5 pl-8 pr-3 text-xs text-fg placeholder:text-fg-hint focus:border-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      />
    </label>

    {#if searching}
      {#if search.error}
        <div role="alert" class="rounded-md border border-error/40 bg-error/10 px-3 py-2 text-xs text-error" data-testid="browser-file-search-error">{search.error}</div>
      {/if}
      <ul
        bind:this={list}
        role="listbox"
        aria-label="Matching files"
        class="max-h-[360px] min-h-[220px] overflow-y-auto rounded-md border border-border/60 bg-surface-0/60"
        data-testid="browser-file-results"
      >
        {#if search.loading && search.results.length === 0}
          <li class="px-3 py-2 text-xs text-fg-muted" aria-hidden="true">Searching…</li>
        {:else if search.results.length === 0 && !search.error}
          <li class="px-3 py-2 text-xs text-fg-muted" aria-hidden="true">No matching files</li>
        {:else}
          {#each search.results as hit, i (hit.absolute)}
            <!-- svelte-ignore a11y_click_events_have_key_events — the search field drives the keyboard -->
            <li
              role="option"
              aria-selected={highlight === i}
              data-index={i}
              data-testid="browser-file-result"
              class="flex cursor-pointer select-none items-center gap-2 px-3 py-1 text-xs {highlight === i ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:bg-surface-2/40 hover:text-fg'}"
              title={hit.absolute}
              onmouseenter={() => (highlight = i)}
              onclick={() => onChoose(hit.absolute)}
            >
              <Icon icon={FileIcon} size={13} class="shrink-0 text-fg-muted" />
              <span class="min-w-0 truncate font-mono">{hit.path}</span>
            </li>
          {/each}
          {#if search.truncated}
            <li class="border-t border-border/50 px-3 py-2 text-[0.6875rem] text-fg-muted" aria-hidden="true">More files match. Refine the search to narrow it.</li>
          {/if}
        {/if}
      </ul>
    {:else}
      <DirectoryBrowser initialPath={startPath} {backend} onSelectFile={onChoose} />
    {/if}
  </div>
</Modal>
