<script lang="ts" module>
  export const COMMIT_FILE_ROW_PX = 28;
</script>

<script lang="ts">
  import { untrack } from 'svelte';
  import Check from '@lucide/svelte/icons/check';
  import Icon from '../primitives/Icon.svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import type { CommitFile } from '../../types/git';
  import type { RowEstimate } from '../../utils/virtual/types';
  import { randomId } from '../../utils/randomId';
  import { commitFileStatus, splitPath } from './commitFiles';

  // The commit dialog's file list: one focusable listbox whose options are
  // the changed files. The active row is the one previewed; a row's
  // selection is whether it goes into the commit. Rows are virtualized at a
  // fixed height, so a list of any length stays reachable by key.

  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  interface Props {
    files: readonly CommitFile[];
    selected: ReadonlySet<string>;
    active: string | null;
    /** Every row is committed and none can be toggled. */
    locked: boolean;
    onActivate: (path: string) => void;
    onToggle: (path: string) => void;
    onToggleAll: () => void;
  }
  let { files, selected, active, locked, onActivate, onToggle, onToggleAll }: Props = $props();

  const listId = `commit-files-${randomId().slice(0, 8)}`;

  const estimate: RowEstimate = {
    at: () => COMMIT_FILE_ROW_PX,
    isExact: () => true,
  };

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();

  function applyScrollTarget(top: number): void {
    if (scrollEl) scrollEl.scrollTop = top;
  }

  // The engine tail-seeds until its first scroll input; the list opens at
  // its top.
  $effect(() => {
    const ref = listRef;
    if (ref) untrack(() => ref.revalidate());
  });

  let indexByPath = $derived(new Map(files.map((file, index) => [file.path, index])));
  let activeIndex = $derived(active === null ? -1 : indexByPath.get(active) ?? -1);

  function optionId(index: number): string {
    return `${listId}-option-${index}`;
  }

  function moveTo(index: number): void {
    const target = Math.max(0, Math.min(files.length - 1, index));
    const file = files[target];
    if (!file) return;
    onActivate(file.path);
    listRef?.scrollToIndex(target, { align: 'nearest' });
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    switch (event.key) {
      case 'ArrowDown':
      case 'j':
        moveTo(activeIndex + 1);
        break;
      case 'ArrowUp':
      case 'k':
        moveTo(activeIndex < 0 ? 0 : activeIndex - 1);
        break;
      case 'Home':
        moveTo(0);
        break;
      case 'End':
        moveTo(files.length - 1);
        break;
      case ' ':
        if (!locked && active !== null) onToggle(active);
        break;
      case 'a':
        if (!locked) onToggleAll();
        break;
      default:
        return;
    }
    event.preventDefault();
  }

  function isSelected(file: CommitFile): boolean {
    return locked || selected.has(file.path);
  }
</script>

<div
  bind:this={scrollEl}
  id={listId}
  role="listbox"
  tabindex="0"
  aria-label="Files to commit"
  aria-multiselectable="true"
  aria-activedescendant={activeIndex >= 0 ? optionId(activeIndex) : undefined}
  data-autofocus
  data-testid="commit-file-list"
  class="h-full overflow-y-auto overflow-x-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 focus:outline-none focus-visible:border-accent focus-visible:ring-2 focus-visible:ring-accent/40"
  style:overflow-anchor="none"
  onkeydown={handleKeydown}
>
  <LongListVirtualizer
    bind:this={listRef}
    data={files}
    getKey={(file) => file.path}
    scrollRef={scrollEl}
    {estimate}
    renderAll={IS_TEST}
    {applyScrollTarget}
    onCompensation={(compensation) => applyScrollTarget(compensation.target)}
  >
    {#snippet children(file: CommitFile)}
      {@const index = indexByPath.get(file.path) ?? 0}
      {@const status = commitFileStatus(file.status)}
      {@const shown = splitPath(file.path)}
      {@const checked = isSelected(file)}
      <!-- The listbox holds focus and answers the keys; an option that
           could take focus would lose it, and the keys with it, when it
           scrolled out of the mounted range. -->
      <!-- svelte-ignore a11y_click_events_have_key_events, a11y_interactive_supports_focus -->
      <div
        id={optionId(index)}
        role="option"
        aria-selected={checked}
        aria-label={file.path}
        data-testid="commit-file-row"
        data-active={file.path === active ? '' : undefined}
        title={file.oldPath ? `${file.oldPath} → ${file.path}` : file.path}
        class="flex items-center gap-2 pr-2 text-[0.75rem] cursor-pointer compact:select-none hover:bg-surface-2 data-[active]:bg-accent/12"
        style:height="{COMMIT_FILE_ROW_PX}px"
        onclick={() => onActivate(file.path)}
      >
        <span
          role="presentation"
          data-testid="commit-file-check"
          class="flex h-full w-8 shrink-0 items-center justify-center {locked ? 'cursor-default opacity-50' : ''}"
          onclick={(event) => {
            event.stopPropagation();
            onActivate(file.path);
            if (!locked) onToggle(file.path);
          }}
        >
          <span
            class="flex size-3.5 items-center justify-center rounded-[3px] border {checked ? 'border-accent bg-accent text-surface-0' : 'border-border-strong'}"
          >
            {#if checked}<Icon icon={Check} size={11} />{/if}
          </span>
        </span>
        <span class="w-3 shrink-0 text-center font-mono font-semibold {status.class}" title={status.label}>{file.status}</span>
        <span class="min-w-0 flex-1 truncate">
          <span class="text-fg">{shown.name}</span>
          {#if file.oldPath}<span class="text-fg-hint"> ← {splitPath(file.oldPath).name}</span>{/if}
          {#if shown.dir}<span class="text-fg-hint"> {shown.dir}</span>{/if}
        </span>
        {#if file.partlyStaged}
          <span
            class="shrink-0 rounded-[3px] border border-border-subtle px-1 text-[0.625rem] text-fg-muted"
            title="Part of this file is staged. Selecting it commits the whole file."
          >partly staged</span>
        {/if}
        <span class="shrink-0 font-mono text-[0.6875rem] tabular-nums">
          {#if file.binary}
            <span class="text-fg-hint">binary</span>
          {:else}
            {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
            {#if file.deletions > 0}<span class="text-error"> −{file.deletions}</span>{/if}
          {/if}
        </span>
      </div>
    {/snippet}
  </LongListVirtualizer>
</div>
