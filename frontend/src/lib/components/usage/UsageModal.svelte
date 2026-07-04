<script lang="ts">
  // Usage modal: provider + period controls over the heatmap, totals,
  // per-model table, and top-projects sections. Each section owns its
  // own GetUsageStats fetch (provider + period passed down as props);
  // this component is layout-only — no fetching here.
  //
  // The period selector shares the persisted usagePeriod store with the
  // sidebar UsageFooter, so changing it here moves the footer too.

  import Modal from '../primitives/Modal.svelte';
  import Segmented from '../primitives/Segmented.svelte';
  import UsageHeatmap from './UsageHeatmap.svelte';
  import UsageTotalsRow from './UsageTotalsRow.svelte';
  import UsageModelTable from './UsageModelTable.svelte';
  import UsageTopProjects from './UsageTopProjects.svelte';
  import { getUsagePeriod, setUsagePeriod, VALID_PERIODS, type UsagePeriod } from '../../stores/usagePeriod.svelte';

  interface Props {
    open: boolean;
    onClose: () => void;
  }

  let { open, onClose }: Props = $props();

  type ProviderFilter = '' | 'claude' | 'codex';

  let providerFilter: ProviderFilter = $state('');

  const PROVIDER_OPTIONS: Array<{ value: ProviderFilter; label: string }> = [
    { value: '', label: 'All' },
    { value: 'claude', label: 'Claude' },
    { value: 'codex', label: 'Codex' },
  ];

  // Labels happen to match the period values themselves ('1w', '30d',
  // 'all'), so the options list derives straight from the shared
  // VALID_PERIODS constant instead of hardcoding a second array that
  // could drift out of sync with it.
  const PERIOD_OPTIONS: Array<{ value: UsagePeriod; label: string }> = VALID_PERIODS.map((value) => ({
    value,
    label: value,
  }));
</script>

<Modal {open} title="Usage" {onClose} width="xl">
  {#snippet children()}
    <div class="flex flex-col gap-5">
      <div class="flex items-center justify-between gap-3 flex-wrap">
        <Segmented
          options={PROVIDER_OPTIONS}
          value={providerFilter}
          onChange={(next) => (providerFilter = next)}
          ariaLabel="Provider filter"
        />
        <Segmented
          options={PERIOD_OPTIONS}
          value={getUsagePeriod()}
          onChange={setUsagePeriod}
          ariaLabel="Time period"
        />
      </div>
      <UsageHeatmap provider={providerFilter} />
      <UsageTotalsRow provider={providerFilter} />
      <UsageModelTable provider={providerFilter} />
      <UsageTopProjects provider={providerFilter} />
    </div>
  {/snippet}
</Modal>
