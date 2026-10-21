<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import CountBadge from '../primitives/CountBadge.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { PullsFilter } from '../../stores/pullsPageFilters.svelte';
  import type { PullsProjectChoice } from '../../stores/pullsPageRows';

  // The Merge requests tab's filter row: one state chip (Needs you, with
  // its count; Mine; Review requested; All open) and the project picker
  // at the end. The page remembers both per tab.

  interface Props {
    filter: PullsFilter;
    needsYouCount: number;
    projects: readonly PullsProjectChoice[];
    /** Empty means every project. */
    selectedProjects: readonly string[];
    onFilter: (next: PullsFilter) => void;
    onProjects: (next: string[]) => void;
  }

  let { filter, needsYouCount, projects, selectedProjects, onFilter, onProjects }: Props = $props();

  const CHIPS: ReadonlyArray<{ value: PullsFilter; label: string }> = [
    { value: 'needs_you', label: 'Needs you' },
    { value: 'mine', label: 'Mine' },
    { value: 'review_requested', label: 'Review requested' },
    { value: 'all', label: 'All open' },
  ];

  const chipClass = 'inline-flex h-6 shrink-0 cursor-pointer items-center gap-1.5 rounded-full border px-2.5 text-[0.6875rem] font-medium transition-colors compact:h-8 compact:select-none focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
  const onClass = 'border-accent/45 bg-accent/12 text-fg';
  const offClass = 'border-border-subtle text-fg-muted hover:border-border hover:text-fg';

  let projectsOpen = $state(false);
  let projectsEl: HTMLButtonElement | undefined = $state(undefined);

  const projectsLabel = $derived.by(() => {
    if (selectedProjects.length === 0) return 'All projects';
    if (selectedProjects.length === 1) {
      return projects.find((project) => project.id === selectedProjects[0])?.name ?? '1 project';
    }
    return `${selectedProjects.length} projects`;
  });

  function closeProjects(reason?: PopoverCloseReason): void {
    projectsOpen = false;
    restorePickerFocus(reason, { triggerEl: projectsEl });
  }

  function toggleProject(id: string): void {
    // None picked means every project, so unticking one from there picks
    // all the others; and picking the last missing one reads as all again.
    const picked = selectedProjects.length === 0 ? projects.map((project) => project.id) : [...selectedProjects];
    const next = picked.includes(id) ? picked.filter((existing) => existing !== id) : [...picked, id];
    onProjects(next.length === projects.length ? [] : next);
  }
</script>

<div class="flex min-w-0 flex-wrap items-center gap-1.5 px-3 py-2" role="group" aria-label="Merge request filters" data-testid="pulls-filters">
  {#each CHIPS as chip (chip.value)}
    {@const active = chip.value === filter}
    <button
      type="button"
      class="{chipClass} {active ? onClass : offClass}"
      aria-pressed={active}
      data-testid="pulls-filter-{chip.value}"
      onclick={() => onFilter(chip.value)}
    >
      {chip.label}
      {#if chip.value === 'needs_you'}
        <CountBadge count={needsYouCount} tone="accent" testId="pulls-filter-needs-you-count" />
      {/if}
    </button>
  {/each}
  {#if projects.length > 1}
    <button
      bind:this={projectsEl}
      type="button"
      class="{chipClass} {offClass} ml-auto"
      aria-haspopup="menu"
      aria-expanded={projectsOpen}
      data-testid="pulls-projects-picker"
      onclick={() => { projectsOpen = !projectsOpen; }}
    >
      <span class="max-w-[10rem] truncate">{projectsLabel}</span>
      <Icon icon={ChevronDown} size={11} strokeWidth={2} />
    </button>
    <Popover anchor={projectsEl} open={projectsOpen} onClose={closeProjects} placement="bottom-end" ariaLabel="Projects">
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
</div>
