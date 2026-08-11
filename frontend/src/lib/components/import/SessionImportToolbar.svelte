<script lang="ts">
  // Sticky filter bar above the catalogue: tri-state select-all, project
  // dropdown, provider segment, search.
  //
  // Filter state is store-owned, so this reads it straight from
  // sessionImport.svelte.ts rather than having the modal drill five values
  // down and five setters back. The only things it takes as props are the
  // facts it cannot compute — the select-all tri-state and the already-ran
  // count (both depend on the modal's filtered projection) and whether a run
  // has frozen the surface.
  //
  // The project picker is the shared Popover + Menu composition; what that
  // costs inside a Modal, and why it is safe here, is in
  // SessionImportProjectMenu.svelte.

  import Search from '@lucide/svelte/icons/search';
  import Icon from '../primitives/Icon.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import SessionImportProjectMenu from './SessionImportProjectMenu.svelte';
  import { providerLabel } from '../../providers/catalog';
  import {
    getImportProjectFilter,
    getImportProviderFilter,
    getImportProviders,
    getImportQuery,
    getImportRows,
    getImportShowAlreadyRan,
    setImportQuery,
    setProjectFilter,
    setProviderFilter,
    setShowAlreadyRan,
  } from '../../stores/sessionImport.svelte';
  import { buildProjectGroups, type ImportSelectAllState } from '../../stores/sessionImportFilter';
  import type { ImportProviderFilter } from '../../types/sessionImport';

  interface Props {
    /** Tri-state over the currently visible rows. */
    selectAll: ImportSelectAllState;
    filteredCount: number;
    /**
     * Rows that already ran in Agent Overflow and pass the current filters —
     * what the toggle would add, or (with it on) what it would take away.
     */
    alreadyRanCount: number;
    disabled: boolean;
    /**
     * The listbox the search box drives, and the row the roving cursor sits
     * on. Both belong on the search input because that is where focus is:
     * `aria-activedescendant` on an element that never holds focus announces
     * nothing. Undefined whenever no list is rendered.
     */
    listboxId: string | undefined;
    activeDescendant: string | undefined;
    onToggleAll: () => void;
  }

  let {
    selectAll,
    filteredCount,
    alreadyRanCount,
    disabled,
    listboxId,
    activeDescendant,
    onToggleAll,
  }: Props = $props();

  const PROVIDER_OPTIONS: Array<{ value: ImportProviderFilter; label: string }> = [
    { value: 'all', label: 'All' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
  ];

  let query = $derived(getImportQuery());
  let providerFilter = $derived(getImportProviderFilter());
  let projectFilter = $derived(getImportProjectFilter());
  let showAlreadyRan = $derived(getImportShowAlreadyRan());

  // Groups come from every offered row (so picking one project doesn't empty
  // the menu it was picked in) while the counts respect provider + query.
  let projectGroups = $derived(
    buildProjectGroups(getImportRows(), { providerFilter, query, showAlreadyRan }),
  );

  // The toggle is an affordance for rows that exist: with none of them under
  // the current filters and the toggle off there is nothing to reveal. It
  // stays rendered while it is ON, whatever the count, or turning it back off
  // would need the filters put back first.
  let showAlreadyRanToggle = $derived(alreadyRanCount > 0 || showAlreadyRan);

  // Quiet provenance footnote. Naming the provider costs one word and is the
  // difference between "which files?" and an answer.
  let skippedNote = $derived(
    getImportProviders()
      .filter((p) => p.skippedCount > 0)
      .map(
        (p) =>
          `${providerLabel(p.provider)}: ${p.skippedCount} unreadable ` +
          `${p.skippedCount === 1 ? 'file' : 'files'} skipped`,
      )
      .join(' · '),
  );
</script>

<div
  class="flex shrink-0 flex-col gap-1 border-b border-border-subtle bg-surface-1 px-3 py-2"
  data-testid="session-import-toolbar"
>
  <div class="flex items-center gap-2">
    <label class="flex shrink-0 items-center gap-1.5 text-[0.6875rem] text-fg-muted">
      <input
        type="checkbox"
        class="accent-accent disabled:cursor-not-allowed"
        data-testid="session-import-select-all"
        data-state={selectAll}
        checked={selectAll === 'all'}
        indeterminate={selectAll === 'some'}
        {disabled}
        onchange={onToggleAll}
      />
      <span class="tabular-nums">{filteredCount}</span>
      <span>shown</span>
    </label>

    <SessionImportProjectMenu
      groups={projectGroups}
      value={projectFilter}
      {disabled}
      onSelect={setProjectFilter}
    />

    <Segmented
      options={PROVIDER_OPTIONS}
      value={providerFilter}
      onChange={setProviderFilter}
      ariaLabel="Provider filter"
      {disabled}
    />

    <div class="relative min-w-0 flex-1">
      <span class="pointer-events-none absolute inset-y-0 left-2 flex items-center text-fg-hint">
        <Icon icon={Search} size={12} />
      </span>
      <input
        type="search"
        value={query}
        oninput={(e) => setImportQuery(e.currentTarget.value)}
        placeholder="Search sessions"
        aria-label="Search sessions"
        aria-controls={listboxId}
        aria-activedescendant={activeDescendant}
        data-testid="session-import-search"
        data-autofocus
        autocomplete="off"
        spellcheck="false"
        {disabled}
        class="w-full rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 py-1 pl-6 pr-2
          text-[0.6875rem] text-fg placeholder:text-fg-hint transition-colors
          disabled:cursor-not-allowed disabled:opacity-50
          focus:border-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
      />
    </div>
  </div>

  <!-- Quiet second line: the already-ran toggle and the skipped-file
       footnote. Both are asides to the controls above, and the toggle's
       sentence is too long to sit inside that row without squeezing search. -->
  {#if showAlreadyRanToggle || skippedNote}
    <div class="flex items-center gap-3 text-[0.625rem] text-fg-hint">
      {#if showAlreadyRanToggle}
        <label class="flex shrink-0 items-center gap-1.5">
          <input
            type="checkbox"
            class="accent-accent disabled:cursor-not-allowed"
            data-testid="session-import-show-already-ran"
            checked={showAlreadyRan}
            {disabled}
            onchange={(e) => setShowAlreadyRan(e.currentTarget.checked)}
          />
          <span>
            Show sessions that already ran in Agent Overflow
            <span class="tabular-nums">({alreadyRanCount})</span>
          </span>
        </label>
      {/if}
      {#if skippedNote}
        <p data-testid="session-import-skipped">{skippedNote}</p>
      {/if}
    </div>
  {/if}
</div>
