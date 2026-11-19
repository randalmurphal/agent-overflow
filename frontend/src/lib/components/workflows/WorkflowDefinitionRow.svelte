<script lang="ts">
  // A workflow definition on home (UI-SPEC §3.2 item 3): name + chain summary.
  // Authoring is out of this surface entirely (D32).
  //
  // R2: no variables, no envelopes, no schemas here. A definition that fails
  // dry-run validation shows its FIRST error inline, hint-toned.

  import type { WorkflowDefinitionListing } from '../../types/workflow';
  import { workflowChainSummary, workflowDefinitionMeta } from '../../stores/workflowData';

  interface Props {
    definition: WorkflowDefinitionListing;
  }
  let { definition }: Props = $props();

  let chain = $derived(workflowChainSummary(definition) || workflowDefinitionMeta(definition));
</script>

<!-- Focusable by script only: the list find's Enter lands on the first row. -->
<div class="rounded-md px-2 py-1.5 hover:bg-surface-2/40 focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40" tabindex="-1" data-find-row data-testid="workflow-definition-row" data-workflow-id={definition.id}>
  <div class="flex min-w-0 items-baseline gap-2">
    <span class="shrink-0 text-sm text-fg">{definition.name || definition.id}</span>
    {#if definition.scope === 'shared'}
      <span class="shrink-0 rounded bg-surface-2 px-1 text-[0.625rem] text-fg-muted">shared</span>
    {/if}
    <span class="min-w-0 flex-1 truncate text-[0.6875rem] text-fg-muted">{chain}</span>
  </div>

  {#if !definition.valid && definition.firstValidationError}
    <p class="pt-0.5 text-[0.6875rem] text-fg-subtle" data-testid="workflow-definition-error">{definition.firstValidationError}</p>
  {/if}
</div>
