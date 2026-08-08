<script lang="ts">
  // Sticky filter bar above the catalogue: tri-state select-all, project
  // dropdown, provider segment, search.
  //
  // Filter state is store-owned, so this reads it straight from
  // sessionImport.svelte.ts rather than having the modal drill four values
  // down and four setters back. The only things it takes as props are the
  // two facts it cannot compute — the select-all tri-state (which depends on
  // the modal's filtered projection) and whether a run has frozen the
  // surface.
  //
  // The project picker is a native <select>, not Popover+Menu: Popover
  // portals its floating element to <body>, which escapes Modal's focus trap
  // and duplicates its Escape path (see the caller contract at the top of
  // Popover.svelte). UsageModal's project filter is the same shape and the
  // same solution.

  import Search from '@lucide/svelte/icons/search';
  import Icon from '../primitives/Icon.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import { providerLabel } from '../../providers/catalog';
  import {
    getImportProjectFilter,
    getImportProviderFilter,
    getImportProviders,
    getImportQuery,
    getImportRows,
    setImportQuery,
    setProjectFilter,
    setProviderFilter,
  } from '../../stores/sessionImport.svelte';
  import { buildProjectGroups, type ImportSelectAllState } from '../../stores/sessionImportFilter';
  import type { ImportProviderFilter } from '../../types/sessionImport';

  interface Props {
    /** Tri-state over the currently visible rows. */
    selectAll: ImportSelectAllState;
    filteredCount: number;
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

  const SELECT_CLASS =
    'max-w-[16rem] shrink-0 truncate rounded-[var(--radius-field)] border border-border-subtle ' +
    'bg-surface-0 px-2 py-1 text-[0.6875rem] text-fg transition-colors ' +
    'disabled:cursor-not-allowed disabled:opacity-50 ' +
    'focus:border-accent focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';

  let query = $derived(getImportQuery());
  let providerFilter = $derived(getImportProviderFilter());
  let projectFilter = $derived(getImportProjectFilter());

  // Groups come from every row (so picking one project doesn't empty the
  // menu it was picked in) while the counts respect provider + query.
  let projectGroups = $derived(
    buildProjectGroups(getImportRows(), { providerFilter, query }),
  );

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

  function handleProject(e: Event): void {
    const value = (e.currentTarget as HTMLSelectElement).value;
    setProjectFilter(value === '' ? null : value);
  }
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

    <select
      class={SELECT_CLASS}
      aria-label="Project filter"
      data-testid="session-import-project-select"
      value={projectFilter ?? ''}
      {disabled}
      onchange={handleProject}
    >
      <option value="">All projects</option>
      {#each projectGroups as group (group.path)}
        <option value={group.path}>{group.label} ({group.count})</option>
      {/each}
    </select>

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

  {#if skippedNote}
    <p class="text-[0.625rem] text-fg-hint" data-testid="session-import-skipped">{skippedNote}</p>
  {/if}
</div>
