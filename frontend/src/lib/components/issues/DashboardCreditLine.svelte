<script lang="ts">
  // What a widget that counts credit says while the last read of who
  // worked on the tickets failed, with why (`WidgetValue.evidence`), on
  // the Dashboards screen and on the board's summary row alike.
  import type { WidgetValue } from '../../../../bindings/agent-overflow/internal/trackerdash/models';

  let { value }: { value: WidgetValue | undefined } = $props();
</script>

{#if value?.evidence}
  <p class="text-[0.6875rem] text-warning" role="status" data-testid="dashboard-widget-evidence">{value.evidence}</p>
{/if}
