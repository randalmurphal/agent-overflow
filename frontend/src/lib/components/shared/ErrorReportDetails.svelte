<script lang="ts">
  // What an error notice shows when expanded: which call failed, its log
  // reference, and each layer of the error, outermost first. Selectable so
  // a person can quote one line; the copy action carries the rest.
  import type { CapturedError } from '../../stores/errorReports.svelte';
  import { errorLayers } from '../../utils/errorReport';

  let { captured }: { captured: CapturedError } = $props();

  let report = $derived(captured.report);
  let layers = $derived(errorLayers(report));
  let at = $derived(new Date(report.detail?.at ?? report.at).toLocaleTimeString());
</script>

<div
  class="mt-1.5 max-h-48 overflow-auto rounded-md bg-fg/5 px-2 py-1.5 font-mono text-[0.6875rem] leading-snug select-text text-text-secondary"
  data-testid="error-details"
>
  <div class="flex flex-wrap gap-x-3 opacity-80">
    {#if report.detail}
      <span>{report.detail.method}</span>
      <span>ref {report.detail.ref}</span>
    {/if}
    <span>{at}</span>
  </div>
  {#if layers.length > 0}
    <ol class="mt-1 list-decimal pl-5 space-y-0.5">
      {#each layers as layer, index (index)}
        <li class="break-words whitespace-pre-wrap">{layer}</li>
      {/each}
    </ol>
  {/if}
</div>
