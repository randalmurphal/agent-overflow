<script lang="ts">
  // Shared status-indicator span used by tool-row headers
  // (AgentRow, AdvisorRow, GenericToolCallRow). Centralises the
  // `data-status` / `data-state` / `aria-label` triple so any future
  // attribute drift (a11y label, status taxonomy) updates in one
  // place instead of three near-identical templates.
  import Indicator from './Indicator.svelte';
  import { indicatorAriaLabel, type IndicatorState } from './rowState';
  import type { Item } from '../../types/models';

  let {
    item,
    state,
    testId,
  }: {
    item: Pick<Item, 'status'>;
    state: IndicatorState;
    testId: string;
  } = $props();
</script>

{#if state}
  <span
    data-testid={testId}
    data-status={item.status}
    data-state={state}
    aria-label={indicatorAriaLabel(state)}
  >
    <Indicator {state} />
  </span>
{/if}
