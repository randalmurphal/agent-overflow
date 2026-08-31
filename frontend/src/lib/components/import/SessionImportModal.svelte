<script lang="ts">
  // The import surface. Reads everything from sessionImport.svelte.ts —
  // including `open`, which the store owns so a palette command or the
  // sidebar trigger can raise the modal without either of them mounting it.
  // That also makes the close guard airtight: Esc, the backdrop and Cancel
  // all land on the store's `closeSessionImport`, which refuses while a run
  // is in flight, and there is no second `open` for a caller to disagree
  // with. App.svelte is the mount point (the sidebar unmounts on collapse).
  //
  // Layout: Modal `padding="none"` so the toolbar can be flush and the list
  // can own its own scroller (see SessionImportList).
  //
  // The surface-level keyboard handler lives here rather than in the list
  // because search is autofocused: arrows have to move the roving cursor
  // while focus is still in the search box, and Enter has to run the import.
  //
  // What the modal keeps is what needs the filtered projection: the roving
  // cursor, the id math the listbox ARIA hangs off, and the wiring between
  // them. The body's empty/error states are one component
  // (SessionImportBanner) picked by one pure classifier (importSurface), and
  // the footer's morphing primary is one pure resolver (resolveImportCta).

  import { untrack } from 'svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Modal from '../primitives/Modal.svelte';
  import SessionImportBanner from './SessionImportBanner.svelte';
  import SessionImportList from './SessionImportList.svelte';
  import SessionImportProgress from './SessionImportProgress.svelte';
  import SessionImportToolbar from './SessionImportToolbar.svelte';
  import {
    closeSessionImport,
    getFailedImportIds,
    getImportProjectFilter,
    getImportProviderFilter,
    getImportProviders,
    getImportQuery,
    getImportRowResult,
    getImportRows,
    getImportSelection,
    getImportShowAlreadyRan,
    getSessionImportRun,
    getSessionImportStatus,
    isSessionImportOpen,
    loadImportCatalog,
    setImportQuery,
    setProjectFilter,
    setProviderFilter,
    setSelection,
    setShowAlreadyRan,
    startImport,
    toggleRow,
  } from '../../stores/sessionImport.svelte';
  import {
    countAlreadyRanRows,
    filterImportRows,
    importSurface,
    selectAllState,
    selectionSummary,
    surfaceHasCatalog,
  } from '../../stores/sessionImportFilter';
  import { resolveImportCta } from './sessionImportCta';
  import { resolveImportKeyAction } from './sessionImportKeyboard';
  import { hasScope } from '../../transport/scopes';
  import { formatPayloadSize } from '../../utils/payloadExpansion.svelte';

  // Instance-scoped so two mounted surfaces could never mint the same row id.
  const INSTANCE = crypto.randomUUID().slice(0, 8);
  const ROW_ID_PREFIX = `session-import-row-${INSTANCE}`;
  const LISTBOX_ID = `session-import-listbox-${INSTANCE}`;

  let open = $derived(isSessionImportOpen());
  let status = $derived(getSessionImportStatus());
  let providers = $derived(getImportProviders());
  let rows = $derived(getImportRows());
  let selection = $derived(getImportSelection());
  let run = $derived(getSessionImportRun());
  let runActive = $derived(run?.active === true);
  // Importing walks the provider homes and writes the threads it finds.
  let ungranted = $derived(!hasScope('threads:operate'));

  let providerFilter = $derived(getImportProviderFilter());
  let projectFilter = $derived(getImportProjectFilter());
  let query = $derived(getImportQuery());
  let showAlreadyRan = $derived(getImportShowAlreadyRan());

  let filters = $derived({ providerFilter, projectFilter, query, showAlreadyRan });
  let filtered = $derived(filterImportRows(rows, filters));
  let filteredIds = $derived(new Set(filtered.map((row) => row.id)));
  let selectAll = $derived(selectAllState(filtered, selection));
  // What the toggle is withholding right now. Feeds the toolbar's control,
  // and tells an empty view whether the filters are the reason it is empty.
  let alreadyRanCount = $derived(countAlreadyRanRows(rows, filters));

  let surface = $derived(
    importSurface({
      status,
      providers,
      rowCount: rows.length,
      filteredCount: filtered.length,
      hiddenCount: showAlreadyRan ? 0 : alreadyRanCount,
    }),
  );
  let hasCatalog = $derived(surfaceHasCatalog(surface));

  // Roving cursor into `filtered`. Kept as a raw index and clamped on read so
  // a shrinking filter can never point past the end.
  let activeIndex = $state(0);
  let activeCursor = $derived(
    filtered.length === 0 ? -1 : Math.min(Math.max(activeIndex, 0), filtered.length - 1),
  );
  // Both the listbox and the search box reference these, so the id math has
  // exactly one owner.
  let listboxId = $derived(filtered.length > 0 ? LISTBOX_ID : undefined);
  let activeDescendant = $derived(
    activeCursor < 0 ? undefined : `${ROW_ID_PREFIX}-${filtered[activeCursor].id}`,
  );

  let cta = $derived(
    resolveImportCta({
      status,
      run,
      importUngranted: ungranted,
      failedIds: getFailedImportIds(),
      selection,
      filteredIds,
    }),
  );

  // No separate thread figure: one selected provider session maps to one AO
  // thread. The summary reads over the whole catalogue when a selection
  // exists, because a selection survives filter changes.
  let summary = $derived(
    selection.size > 0 ? selectionSummary(rows, selection) : selectionSummary(filtered, filteredIds),
  );
  let summaryText = $derived.by(() => {
    const lead =
      selection.size > 0
        ? `${summary.count} of ${rows.length} selected`
        : `${filtered.length} of ${rows.length} shown`;
    return `${lead} · ${formatPayloadSize(summary.bytes)}`;
  });

  // Re-scan on every open; a catalogue that already loaded is reused (the
  // store's own no-force short circuit), so reopening is free.
  // untrack: loadImportCatalog reads and writes the same store state this
  // component renders, so a tracked call would re-run the effect on settle.
  $effect(() => {
    if (!open) return;
    untrack(() => {
      activeIndex = 0;
      void loadImportCatalog();
    });
  });

  // A filter change replaces the visible set, so the cursor's row is gone.
  // Reading the three filters is the whole dependency list; `activeIndex` is
  // written but never read here, so this cannot re-enter.
  $effect(() => {
    void providerFilter;
    void projectFilter;
    void query;
    void showAlreadyRan;
    activeIndex = 0;
  });

  function handleRefresh(): void {
    void loadImportCatalog(true);
  }

  function clearFilters(): void {
    setProviderFilter('all');
    setProjectFilter(null);
    setImportQuery('');
  }

  function selectAllFiltered(): void {
    setSelection([...selection, ...filteredIds]);
  }

  function clearFilteredSelection(): void {
    setSelection([...selection].filter((id) => !filteredIds.has(id)));
  }

  // Select-all only ever adds or removes the rows currently on screen —
  // selections made under a different filter are not the user's target here
  // and silently dropping them would contradict the footer's count.
  function handleToggleAll(): void {
    if (selectAll === 'all') clearFilteredSelection();
    else selectAllFiltered();
  }

  function runImport(): void {
    if (!cta.enabled) return;
    void startImport(cta.targetIds);
  }

  function moveActive(delta: number): void {
    if (filtered.length === 0) return;
    const next = activeCursor < 0 ? 0 : activeCursor + delta;
    activeIndex = Math.min(Math.max(next, 0), filtered.length - 1);
  }

  function toggleActive(): void {
    if (activeCursor < 0) return;
    toggleRow(filtered[activeCursor].id);
  }

  function handleKeydown(e: KeyboardEvent): void {
    // A run freezes the surface; the store refuses the actions anyway, but
    // the cursor shouldn't drift under the progress strip either.
    if (runActive) return;

    // The resolver decides ownership; a non-null answer means the surface
    // takes the key, which is exactly when the default is suppressed.
    const action = resolveImportKeyAction(e);
    if (!action) return;
    e.preventDefault();
    switch (action) {
      case 'select-all':
        selectAllFiltered();
        return;
      case 'cursor-down':
        moveActive(1);
        return;
      case 'cursor-up':
        moveActive(-1);
        return;
      case 'toggle-active':
        toggleActive();
        return;
      case 'run-import':
        runImport();
        return;
    }
  }
</script>

<Modal
  {open}
  title="Import Sessions"
  onClose={closeSessionImport}
  width="xl"
  padding="none"
>
  {#snippet headerActions()}
    <IconButton
      label="Rescan provider sessions"
      size="sm"
      testId="session-import-refresh"
      disabled={runActive || surface === 'loading'}
      onClick={handleRefresh}
    >
      {#snippet children()}
        <Icon icon={RefreshCw} size={13} />
      {/snippet}
    </IconButton>
  {/snippet}

  {#snippet children()}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div class="flex min-h-0 flex-col" onkeydown={handleKeydown} data-testid="session-import-body">
      {#if hasCatalog}
        <SessionImportToolbar
          {selectAll}
          {listboxId}
          {activeDescendant}
          {alreadyRanCount}
          filteredCount={filtered.length}
          disabled={runActive}
          onToggleAll={handleToggleAll}
        />

        <!-- Sticky strip between the toolbar and the list. The list stays
             mounted underneath so per-row outcome stamps land on the rows
             they belong to; the strip renders itself only when a run exists. -->
        <SessionImportProgress />
      {/if}

      <!-- Everything that is not a row, in the one slot they all share. -->
      <SessionImportBanner
        {surface}
        {alreadyRanCount}
        onClearFilters={clearFilters}
        onShowAlreadyRan={() => setShowAlreadyRan(true)}
      />

      {#if surface === 'rows'}
        <SessionImportList
          id={LISTBOX_ID}
          rows={filtered}
          {selection}
          {activeDescendant}
          activeIndex={activeCursor}
          resultFor={getImportRowResult}
          disabled={runActive}
          idPrefix={ROW_ID_PREFIX}
          onToggle={toggleRow}
        />
      {/if}
    </div>
  {/snippet}

  {#snippet footer()}
    <span
      class="mr-auto self-center text-[0.6875rem] tabular-nums text-fg-muted"
      data-testid="session-import-summary"
    >
      {summaryText}
    </span>
    <Button
      variant="secondary"
      size="sm"
      testId="session-import-cancel"
      disabled={runActive}
      onclick={closeSessionImport}
    >
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      testId="session-import-confirm"
      title={ungranted ? 'Not granted to this device' : undefined}
      disabled={!cta.enabled}
      loading={runActive}
      onclick={runImport}
    >
      {#snippet children()}{cta.label}{/snippet}
    </Button>
  {/snippet}
</Modal>
