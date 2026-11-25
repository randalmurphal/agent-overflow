<script lang="ts">
  // The Board screen: the board's columns side by side, the row scrolling
  // sideways when the pane is narrower than the columns. Under compact the
  // page never scrolls sideways: a strip of the columns, each with its count
  // and estimate sum, picks the one column shown (kept per board while the
  // app runs; the first in-progress column until one is picked). Only mine
  // and Search filter the held cards; while either is on, each column not
  // held whole is read whole from this computer's copy.
  import { untrack } from 'svelte';
  import BoardColumn from './BoardColumn.svelte';
  import StatusMark from './StatusMark.svelte';
  import { columnCount } from './boardFormat';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import { compactColumn, setCompactColumn, type CardField } from '../../stores/issuesPane.svelte';
  import { loadMoreOfSection, setTrackerBoardFiltered, type TrackerBoardRef } from '../../stores/trackerBoard.svelte';
  import {
    filtering,
    screenColumns,
    sectionKey,
    shownColumn,
    type CardFilter,
    type HeldBoard,
    type ScreenColumn,
  } from '../../stores/trackerBoardModel';

  interface Props {
    boardKey: string;
    boardRef: TrackerBoardRef;
    held: HeldBoard;
    /** The section being read (`TrackerBoardState.paging`). */
    paging: ReadonlySet<string>;
    /** The sections whose page read failed (`TrackerBoardState.sectionErrors`). */
    sectionErrors: ReadonlyMap<string, unknown>;
    filter: CardFilter;
    hidden: ReadonlySet<CardField>;
    compact: boolean;
    /** False while the pane is not on screen. */
    shown: boolean;
  }

  let { boardKey, boardRef, held, paging, sectionErrors, filter, hidden, compact, shown }: Props = $props();

  // The last columns, so a rebuild keeps the rows arrays nothing changed.
  let previous: ScreenColumn[] = [];
  const columns = $derived.by(() => {
    const next = screenColumns(held, previous);
    previous = next;
    return next;
  });
  const filtered = $derived(filtering(filter));
  const visible = $derived(columns.map((col) => shownColumn(held, col, filter)));
  const now = $derived(getMinuteNow());
  const unit = $derived(held.shape.unit);
  // One header count per column, for the strip and the column's header.
  const counts = $derived(columns.map((col, i) => columnCount(col, visible[i], filtered, unit)));
  const noSprint = $derived(held.shape.plansSprints && !held.shape.sprints.some((sp) => sp.state === 'active'));

  // A filter reads the rest of every column while it is on.
  $effect(() => {
    const on = filtered;
    untrack(() => setTrackerBoardFiltered(boardKey, on));
  });

  function moreOf(col: ScreenColumn): () => void {
    return () => {
      if (col.moreFrom !== null) loadMoreOfSection(boardKey, held.sections[col.moreFrom].section);
    };
  }

  // The sections a column still reads from: its own on a kanban board,
  // every sprint's on a sprint board.
  function readingFrom(col: ScreenColumn): string[] {
    if (col.moreFrom === null) return [];
    if (held.shape.plansSprints) return held.sections.filter((s) => s.section.kind === 'sprint').map((s) => sectionKey(s.section));
    return [sectionKey(held.sections[col.moreFrom].section)];
  }

  function pagingOf(col: ScreenColumn): boolean {
    return readingFrom(col).some((k) => paging.has(k));
  }

  function failedOf(col: ScreenColumn): boolean {
    return readingFrom(col).some((k) => sectionErrors.has(k));
  }

  const picked = $derived.by(() => {
    const chosen = compactColumn(boardRef);
    if (chosen !== null && chosen < columns.length) return chosen;
    const working = columns.findIndex((col) => col.statusGroups.includes('in_progress'));
    return working >= 0 ? working : 0;
  });

</script>

<div class="flex min-h-0 flex-1 flex-col" data-testid="issues-board">
  {#if noSprint}
    <p class="px-3 py-2 text-sm text-fg-muted" data-testid="issues-board-no-sprint">No sprint is active on this board.</p>
  {:else if columns.length === 0}
    <p class="px-3 py-2 text-sm text-fg-muted" data-testid="issues-board-no-columns">This board has no columns.</p>
  {:else if compact}
    <div class="flex flex-wrap gap-1 border-b border-border-subtle px-2 py-1.5" role="tablist" aria-label="Columns" data-testid="issues-column-strip">
      {#each columns as col, i (i)}
        <button
          type="button"
          role="tab"
          aria-selected={i === picked}
          class="inline-flex h-9 min-w-0 items-center gap-1.5 rounded-[var(--radius-control)] px-2.5 text-sm compact:select-none {i === picked ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
          data-testid="issues-column-tab"
          onclick={() => setCompactColumn(boardRef, i)}
        >
          <StatusMark group={col.statusGroups[0]} />
          <span class="truncate">{col.name}</span>
          <span class="shrink-0 tabular-nums text-fg-muted">{counts[i]}</span>
        </button>
      {/each}
    </div>
    {#key picked}
      <div class="flex min-h-0 flex-1 px-2 pt-2">
        <BoardColumn
          column={columns[picked]}
          rows={visible[picked].rows}
          count={counts[picked]}
          {filtered}
          {unit}
          {hidden}
          {now}
          paging={pagingOf(columns[picked])}
          failed={failedOf(columns[picked])}
          onNearEnd={moreOf(columns[picked])}
          {shown}
          wide
        />
      </div>
    {/key}
  {:else}
    <div class="flex min-h-0 flex-1 gap-3 overflow-x-auto px-3 pt-3" data-testid="issues-columns">
      {#each columns as col, i (i)}
        <BoardColumn
          column={col}
          rows={visible[i].rows}
          count={counts[i]}
          {filtered}
          {unit}
          {hidden}
          {now}
          paging={pagingOf(col)}
          failed={failedOf(col)}
          onNearEnd={moreOf(col)}
          {shown}
        />
      {/each}
    </div>
  {/if}
</div>
