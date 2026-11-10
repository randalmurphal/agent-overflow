<script lang="ts">
  import ArrowUpDown from '@lucide/svelte/icons/arrow-up-down';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import ListFilter from '@lucide/svelte/icons/list-filter';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { PullsFilter, PullsSort, PullsSortKey } from '../../stores/pullsPageFilters.svelte';
  import type { PullsProjectChoice } from '../../stores/pullsPageRows';

  // The Merge requests tab's toolbar: three menu buttons, the filter
  // (Needs you with its count, Mine, Reviewing, All open), the projects
  // and the sort. The list column is a container (`@container/pulls`)
  // and under 380px the buttons drop their labels for their icons, so a
  // narrow column never cuts a control off. The page remembers all
  // three per computer.

  interface Props {
    filter: PullsFilter;
    needsYouCount: number;
    projects: readonly PullsProjectChoice[];
    /** Empty means every project. */
    selectedProjects: readonly string[];
    sort: PullsSort;
    onFilter: (next: PullsFilter) => void;
    onProjects: (next: string[]) => void;
    onSort: (next: PullsSort) => void;
  }

  let { filter, needsYouCount, projects, selectedProjects, sort, onFilter, onProjects, onSort }: Props = $props();

  const FILTERS: ReadonlyArray<{ value: PullsFilter; label: string; description: string }> = [
    { value: 'needs_you', label: 'Needs you', description: 'Your turn: a review asked of you, or moved since yours' },
    { value: 'mine', label: 'Mine', description: 'Opened by you' },
    { value: 'reviewing', label: 'Reviewing', description: 'You are a reviewer' },
    { value: 'all', label: 'All open', description: 'Every open merge request' },
  ];
  const SORTS: ReadonlyArray<{ value: PullsSortKey; label: string; ascending: string; descending: string }> = [
    { value: 'updated', label: 'Updated', descending: 'Newest first', ascending: 'Oldest first' },
    { value: 'created', label: 'Created', descending: 'Newest first', ascending: 'Oldest first' },
    { value: 'title', label: 'Title', descending: 'A to Z', ascending: 'Z to A' },
    { value: 'author', label: 'Author', descending: 'A to Z', ascending: 'Z to A' },
    { value: 'attention', label: 'Attention', descending: 'Most urgent first', ascending: 'Least urgent first' },
  ];

  const BUTTON =
    'inline-flex h-6 shrink-0 cursor-pointer items-center gap-1.5 rounded-[var(--radius-control)] border px-2 text-[0.6875rem] whitespace-nowrap transition-colors ' +
    'compact:h-9 compact:select-none focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 @max-[380px]/pulls:px-1.5';
  const BUTTON_ON = 'border-border-subtle bg-surface-1 text-fg';
  const BUTTON_OFF = 'border-transparent text-fg-muted hover:bg-surface-1 hover:text-fg';
  // The label beside the icon; gone under 380px.
  const LABEL = 'truncate @max-[380px]/pulls:hidden';

  type Open = 'filter' | 'projects' | 'sort' | null;
  let open: Open = $state(null);
  let filterEl: HTMLButtonElement | undefined = $state(undefined);
  let projectsEl: HTMLButtonElement | undefined = $state(undefined);
  let sortEl: HTMLButtonElement | undefined = $state(undefined);

  const filterLabel = $derived(FILTERS.find((entry) => entry.value === filter)?.label ?? 'All open');
  const sortEntry = $derived(SORTS.find((entry) => entry.value === sort.key) ?? SORTS[0]!);
  const projectsLabel = $derived.by(() => {
    if (selectedProjects.length === 0) return projects.length === 1 ? projects[0]!.name : 'All projects';
    if (selectedProjects.length === 1) return projects.find((project) => project.id === selectedProjects[0])?.name ?? '1 project';
    return `${selectedProjects.length} projects`;
  });

  function toggle(which: Exclude<Open, null>): void {
    open = open === which ? null : which;
  }

  function closer(which: Exclude<Open, null>, trigger: () => HTMLButtonElement | undefined): (reason?: PopoverCloseReason) => void {
    return (reason) => {
      if (open === which) open = null;
      restorePickerFocus(reason, { triggerEl: trigger() });
    };
  }
  const closeFilter = closer('filter', () => filterEl);
  const closeProjects = closer('projects', () => projectsEl);
  const closeSort = closer('sort', () => sortEl);

  function toggleProject(id: string): void {
    // None picked means every project, so unticking one from there picks
    // all the others; and picking the last missing one reads as all again.
    const picked = selectedProjects.length === 0 ? projects.map((project) => project.id) : [...selectedProjects];
    const next = picked.includes(id) ? picked.filter((existing) => existing !== id) : [...picked, id];
    onProjects(next.length === projects.length ? [] : next);
  }

  // A single choice closes its menu; the projects menu stays open for
  // ticking several.
  function pickFilter(value: PullsFilter): void {
    onFilter(value);
    closeFilter();
  }

  // Picking the current key again turns the order around.
  function pickSort(key: PullsSortKey): void {
    onSort(key === sort.key ? { key, reversed: !sort.reversed } : { key, reversed: false });
    closeSort();
  }
</script>

<div class="flex min-w-0 items-center gap-1 px-2 py-1.5" role="group" aria-label="Merge request filters" data-testid="pulls-filters">
  <button
    bind:this={filterEl}
    type="button"
    class="{BUTTON} {filter === 'all' ? BUTTON_OFF : BUTTON_ON} min-w-0"
    aria-haspopup="menu"
    aria-expanded={open === 'filter'}
    aria-label="Filter: {filterLabel}"
    data-testid="pulls-filter"
    data-filter={filter}
    onclick={() => toggle('filter')}
  >
    <Icon icon={ListFilter} size={13} strokeWidth={2} />
    <span class={LABEL}>{filterLabel}</span>
    {#if needsYouCount > 0}
      <span class="font-medium tabular-nums text-accent" data-testid="pulls-filter-needs-you-count">{needsYouCount}</span>
    {/if}
    <Icon icon={ChevronDown} size={11} strokeWidth={2} />
  </button>
  <Popover anchor={filterEl} open={open === 'filter'} onClose={closeFilter} placement="bottom-start" ariaLabel="Filter">
    <Menu ariaLabel="Filter" onClose={closeFilter} minWidthClass="min-w-[240px]">
      {#each FILTERS as entry (entry.value)}
        <MenuItem
          label={entry.label}
          description={entry.description}
          suffix={entry.value === 'needs_you' && needsYouCount > 0 ? String(needsYouCount) : undefined}
          checked={entry.value === filter}
          onSelect={() => pickFilter(entry.value)}
          testId="pulls-filter-{entry.value}"
        />
      {/each}
    </Menu>
  </Popover>

  {#if projects.length > 1}
    <button
      bind:this={projectsEl}
      type="button"
      class="{BUTTON} {selectedProjects.length === 0 ? BUTTON_OFF : BUTTON_ON} min-w-0"
      aria-haspopup="menu"
      aria-expanded={open === 'projects'}
      aria-label="Projects: {projectsLabel}"
      data-testid="pulls-projects-picker"
      onclick={() => toggle('projects')}
    >
      <Icon icon={FolderGit2} size={13} strokeWidth={1.75} />
      <span class={LABEL}>{projectsLabel}</span>
      {#if selectedProjects.length > 0}
        <span class="hidden font-medium tabular-nums @max-[380px]/pulls:inline">{selectedProjects.length}</span>
      {/if}
      <Icon icon={ChevronDown} size={11} strokeWidth={2} />
    </button>
    <Popover anchor={projectsEl} open={open === 'projects'} onClose={closeProjects} placement="bottom-start" ariaLabel="Projects">
      <Menu ariaLabel="Projects" onClose={closeProjects} minWidthClass="min-w-[220px]">
        <MenuItem label="All projects" checkbox checked={selectedProjects.length === 0} onSelect={() => onProjects([])} testId="pulls-projects-all" />
        <MenuDivider />
        {#each projects as project (project.id)}
          <MenuItem
            label={project.name}
            checkbox
            checked={selectedProjects.length === 0 || selectedProjects.includes(project.id)}
            onSelect={() => toggleProject(project.id)}
            testId="pulls-project-{project.id}"
          />
        {/each}
      </Menu>
    </Popover>
  {/if}

  <button
    bind:this={sortEl}
    type="button"
    class="{BUTTON} {BUTTON_OFF} ml-auto"
    aria-haspopup="menu"
    aria-expanded={open === 'sort'}
    aria-label="Sort: {sortEntry.label}, {sort.reversed ? sortEntry.ascending : sortEntry.descending}"
    data-testid="pulls-sort"
    data-sort={sort.key}
    data-reversed={sort.reversed ? 'true' : undefined}
    onclick={() => toggle('sort')}
  >
    <Icon icon={ArrowUpDown} size={13} strokeWidth={2} />
    <span class={LABEL}>{sortEntry.label}</span>
    <Icon icon={ChevronDown} size={11} strokeWidth={2} />
  </button>
  <Popover anchor={sortEl} open={open === 'sort'} onClose={closeSort} placement="bottom-end" ariaLabel="Sort">
    <Menu ariaLabel="Sort" onClose={closeSort} minWidthClass="min-w-[200px]">
      {#each SORTS as entry (entry.value)}
        {@const on = entry.value === sort.key}
        <MenuItem
          label={entry.label}
          description={on ? (sort.reversed ? entry.ascending : entry.descending) : entry.descending}
          checked={on}
          onSelect={() => pickSort(entry.value)}
          testId="pulls-sort-{entry.value}"
        />
      {/each}
    </Menu>
  </Popover>
</div>
