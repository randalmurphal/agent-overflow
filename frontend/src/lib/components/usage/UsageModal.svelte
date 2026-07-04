<script lang="ts">
  // Usage modal: provider filter on top, the fixed 26-week heatmap, then
  // a period + project filter line, totals, and the per-model table.
  // Each section owns its own GetUsageStats fetch (filters passed down
  // as props); this component is layout-only apart from the project
  // option list it feeds its own dropdown.
  //
  // The period selector shares the persisted usagePeriod store with the
  // sidebar UsageFooter, so changing it here moves the footer too. It
  // deliberately does NOT affect the heatmap — that always shows its
  // fixed 26-week window; provider/project filters apply everywhere.

  import Modal from '../primitives/Modal.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import UsageHeatmap from './UsageHeatmap.svelte';
  import UsageTotalsRow from './UsageTotalsRow.svelte';
  import UsageModelTable from './UsageModelTable.svelte';
  import { UsageQuery } from '../../stores/bindings';
  import { getUsageRefreshVersion } from '../../stores/usageRefresh.svelte';
  import { createUsageStats } from '../../stores/usageQuery.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { getUsagePeriod, setUsagePeriod, VALID_PERIODS, type UsagePeriod } from '../../stores/usagePeriod.svelte';

  interface Props {
    open: boolean;
    onClose: () => void;
  }

  let { open, onClose }: Props = $props();

  type ProviderFilter = '' | 'claude' | 'codex';

  let providerFilter: ProviderFilter = $state('');
  let projectFilter = $state('');

  const PROVIDER_OPTIONS: Array<{ value: ProviderFilter; label: string }> = [
    { value: '', label: 'All' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
  ];

  // Labels happen to match the period values themselves ('day', 'week',
  // 'month', 'all'), so the options list derives straight from the
  // shared VALID_PERIODS constant instead of hardcoding a second array
  // that could drift out of sync with it.
  const PERIOD_OPTIONS: Array<{ value: UsagePeriod; label: string }> = VALID_PERIODS.map((value) => ({
    value,
    label: value,
  }));

  // Dropdown option list: every project that has EVER logged usage —
  // lifetime and unfiltered by provider/period on purpose, so a
  // selected project can't vanish out of the list when the other
  // filters change.
  const projectStats = createUsageStats(() => {
    if (!open) return null;
    getUsageRefreshVersion();
    return new UsageQuery({ groupBy: 'project' });
  });

  /** Maps a project-id bucket key to a display name. A non-empty id
   *  absent from the projects store means the project was since
   *  deleted. */
  function projectLabel(id: string): string {
    const project = getProject(id);
    return project ? project.project.name : '(deleted)';
  }

  // Rows with no project association (empty project_id) are only
  // reachable through "All Projects": an empty id is both the
  // all-projects option value here and the no-filter sentinel in
  // UsageQuery.ProjectID, so it can't double as a selectable bucket.
  let projectOptions = $derived(
    (projectStats.buckets ?? [])
      .filter((b) => b.bucket !== '')
      .map((b) => ({ id: b.bucket, label: projectLabel(b.bucket) }))
      .sort((a, b) => a.label.localeCompare(b.label)),
  );

  const PROJECT_SELECT_CLASS =
    'rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 text-fg ' +
    'text-[0.6875rem] px-2 py-1 max-w-[13rem] truncate cursor-pointer transition-colors ' +
    'focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40';
</script>

<Modal {open} title="Usage" {onClose} width="md">
  {#snippet children()}
    <div class="flex flex-col gap-4">
      <Segmented
        options={PROVIDER_OPTIONS}
        value={providerFilter}
        onChange={(next) => (providerFilter = next)}
        ariaLabel="Provider filter"
      />
      <UsageHeatmap provider={providerFilter} projectId={projectFilter} />
      <div class="flex items-center justify-between gap-3 flex-wrap">
        <select
          class={PROJECT_SELECT_CLASS}
          aria-label="Project filter"
          data-testid="usage-project-select"
          value={projectFilter}
          onchange={(e) => (projectFilter = (e.target as HTMLSelectElement).value)}
        >
          <option value="">All Projects</option>
          {#each projectOptions as option (option.id)}
            <option value={option.id}>{option.label}</option>
          {/each}
        </select>
        <Segmented
          options={PERIOD_OPTIONS}
          value={getUsagePeriod()}
          onChange={setUsagePeriod}
          ariaLabel="Time period"
        />
      </div>
      <UsageTotalsRow provider={providerFilter} projectId={projectFilter} />
      <UsageModelTable provider={providerFilter} projectId={projectFilter} />
    </div>
  {/snippet}
</Modal>
