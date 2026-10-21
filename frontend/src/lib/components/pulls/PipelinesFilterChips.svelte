<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import type { PipelineFilters, PipelineRefKind, PipelineStatusFilter } from '../../stores/pullsPageFilters.svelte';

  // The Pipelines tab's filter row: one chip per project (every project
  // when none is picked), then the ref kind and status pickers. Remembered
  // per computer with the tab.

  interface Props {
    filters: PipelineFilters;
    projects: readonly { id: string; name: string }[];
    onChange: (next: Partial<PipelineFilters>) => void;
  }

  let { filters, projects, onChange }: Props = $props();

  const REF_KINDS: ReadonlyArray<{ value: PipelineRefKind; label: string }> = [
    { value: '', label: 'Any ref' },
    { value: 'branch', label: 'Branches' },
    { value: 'merge_request', label: 'Merge requests' },
    { value: 'tag', label: 'Tags' },
  ];
  const STATUSES: ReadonlyArray<{ value: PipelineStatusFilter; label: string }> = [
    { value: '', label: 'All statuses' },
    { value: 'running', label: 'Running' },
    { value: 'pending', label: 'Pending' },
    { value: 'failed', label: 'Failed' },
    { value: 'success', label: 'Passed' },
    { value: 'canceled', label: 'Canceled' },
    { value: 'skipped', label: 'Skipped' },
    { value: 'manual', label: 'Manual' },
  ];

  const chipClass = 'inline-flex h-6 shrink-0 cursor-pointer items-center gap-1.5 rounded-full border px-2.5 text-[0.6875rem] font-medium transition-colors compact:h-8 compact:select-none focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
  const onClass = 'border-accent/45 bg-accent/12 text-fg';
  const offClass = 'border-border-subtle text-fg-muted hover:border-border hover:text-fg';

  const refLabel = $derived(REF_KINDS.find((kind) => kind.value === filters.refKind)?.label ?? 'Any ref');
  const statusLabel = $derived(STATUSES.find((status) => status.value === filters.status)?.label ?? 'All statuses');

  let openPicker: 'ref' | 'status' | null = $state(null);
  let refEl: HTMLButtonElement | undefined = $state(undefined);
  let statusEl: HTMLButtonElement | undefined = $state(undefined);

  function closePicker(reason?: PopoverCloseReason): void {
    const trigger = openPicker === 'ref' ? refEl : statusEl;
    openPicker = null;
    restorePickerFocus(reason, { triggerEl: trigger });
  }

  function projectOn(id: string): boolean {
    return filters.projects.length === 0 || filters.projects.includes(id);
  }

  function toggleProject(id: string): void {
    const picked = filters.projects.length === 0 ? projects.map((project) => project.id) : [...filters.projects];
    const next = picked.includes(id) ? picked.filter((existing) => existing !== id) : [...picked, id];
    // Every project picked reads as none picked; unticking the last one
    // would hide everything, so it stays on.
    if (next.length === 0) return;
    onChange({ projects: next.length === projects.length ? [] : next });
  }
</script>

<div class="flex min-w-0 flex-wrap items-center gap-1.5 px-3 py-2" role="group" aria-label="Pipeline filters" data-testid="pipelines-filters">
  {#if projects.length > 1}
    {#each projects as project (project.id)}
      {@const active = projectOn(project.id)}
      <button
        type="button"
        class="{chipClass} {active ? onClass : offClass}"
        aria-pressed={active}
        data-testid="pipelines-project-{project.id}"
        onclick={() => toggleProject(project.id)}
      >
        <span class="max-w-[10rem] truncate">{project.name}</span>
      </button>
    {/each}
  {/if}
  <button
    bind:this={refEl}
    type="button"
    class="{chipClass} {filters.refKind ? onClass : offClass} ml-auto"
    aria-haspopup="menu"
    aria-expanded={openPicker === 'ref'}
    data-testid="pipelines-ref-picker"
    onclick={() => { openPicker = openPicker === 'ref' ? null : 'ref'; }}
  >
    {refLabel}
    <Icon icon={ChevronDown} size={11} strokeWidth={2} />
  </button>
  <button
    bind:this={statusEl}
    type="button"
    class="{chipClass} {filters.status ? onClass : offClass}"
    aria-haspopup="menu"
    aria-expanded={openPicker === 'status'}
    data-testid="pipelines-status-picker"
    onclick={() => { openPicker = openPicker === 'status' ? null : 'status'; }}
  >
    {statusLabel}
    <Icon icon={ChevronDown} size={11} strokeWidth={2} />
  </button>
  <Popover anchor={openPicker === 'ref' ? refEl : statusEl} open={openPicker !== null} onClose={closePicker} placement="bottom-end" ariaLabel={openPicker === 'ref' ? 'Ref kind' : 'Status'}>
    {#if openPicker === 'ref'}
      <Menu ariaLabel="Ref kind" onClose={closePicker} minWidthClass="min-w-[180px]">
        {#each REF_KINDS as kind (kind.value)}
          <MenuItem label={kind.label} checkbox checked={filters.refKind === kind.value} onSelect={() => onChange({ refKind: kind.value })} testId="pipelines-ref-{kind.value || 'any'}" />
        {/each}
      </Menu>
    {:else}
      <Menu ariaLabel="Status" onClose={closePicker} minWidthClass="min-w-[180px]">
        {#each STATUSES as status (status.value)}
          <MenuItem label={status.label} checkbox checked={filters.status === status.value} onSelect={() => onChange({ status: status.value })} testId="pipelines-status-{status.value || 'all'}" />
        {/each}
      </Menu>
    {/if}
  </Popover>
</div>
