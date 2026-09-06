<script lang="ts">
  // A workflow definition on home (UI-SPEC §3.2 item 3): name + chain summary.
  // Its automations render inline on the row — trigger, next fire, skipped
  // fires, enable/disable, Run now — and nothing richer: changing cron, seeds
  // or conditions is work over the definition files, not a form (§11).
  // Authoring is out of this surface entirely (D32).
  //
  // R2: no variables, no envelopes, no schemas here. A definition that fails
  // dry-run validation shows its FIRST error inline, hint-toned.

  import WorkflowJobNotes from './WorkflowJobNotes.svelte';
  import { WorkflowRunAutomationNow, WorkflowSetAutomationEnabled } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { automationHasScope } from '../../transport/entityScopes';
  import type { WorkflowAutomationView, WorkflowDefinitionListing } from '../../types/workflow';
  import { workflowChainSummary, workflowCountdown, workflowDefinitionMeta, workflowMetaLine } from '../../stores/workflowData';
  import { refreshWorkflowRunsSoon } from '../../stores/workflowRuns.svelte';

  interface Props {
    definition: WorkflowDefinitionListing;
    automations: readonly WorkflowAutomationView[];
  }
  let { definition, automations }: Props = $props();

  // Every control here drives the workflow engine, which is `threads:autonomy`.
  let busy = $state('');

  let chain = $derived(workflowChainSummary(definition) || workflowDefinitionMeta(definition));

  function automationMeta(automation: WorkflowAutomationView): string {
    return workflowMetaLine([
      automation.triggerSummary || automation.triggerKind,
      automation.nextFireAt ? `next ${workflowCountdown(automation.nextFireAt)}` : '',
      automation.skipCount > 0 ? `${automation.skipCount} skipped` : '',
      automation.triggerError,
    ]);
  }

  async function toggleAutomation(automation: WorkflowAutomationView): Promise<void> {
    if (!automationHasScope('threads:autonomy', automation.id) || busy) return;
    busy = automation.id;
    try {
      await WorkflowSetAutomationEnabled(automation.id, !automation.enabled);
    } catch (err) {
      addToast('error', userFacingError(err, `Could not update ${automation.name}.`));
    } finally {
      busy = '';
    }
  }

  async function runNow(automation: WorkflowAutomationView): Promise<void> {
    if (!automationHasScope('threads:autonomy', automation.id) || busy) return;
    busy = automation.id;
    try {
      await WorkflowRunAutomationNow(automation.id);
      addToast('success', `Started — ${automation.name}`);
      refreshWorkflowRunsSoon();
    } catch (err) {
      addToast('error', userFacingError(err, `Could not run ${automation.name}.`));
    } finally {
      busy = '';
    }
  }
</script>

<div class="rounded-md px-2 py-1.5 hover:bg-surface-2/40" data-testid="workflow-definition-row" data-workflow-id={definition.id}>
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

  {#each automations as automation (automation.id)}
    {@const ungranted = !automationHasScope('threads:autonomy', automation.id)}
    {@const ungrantedTitle = ungranted ? 'Not granted to this device' : undefined}
    <div class="mt-1 flex min-w-0 items-center gap-2 pl-3" data-testid="workflow-automation-row" data-automation-id={automation.id}>
      <span class="min-w-0 flex-1 truncate text-[0.6875rem] text-fg-muted">{automation.name} · {automationMeta(automation)}</span>
      <button
        class="shrink-0 text-[0.6875rem] text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => { void toggleAutomation(automation); }}
        disabled={ungranted || busy === automation.id}
        title={ungrantedTitle}
        data-testid="workflow-automation-toggle"
      >{automation.enabled ? 'Disable' : 'Enable'}</button>
      <button
        class="shrink-0 text-[0.6875rem] text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => { void runNow(automation); }}
        disabled={ungranted || busy === automation.id}
        title={ungrantedTitle}
        data-testid="workflow-automation-run-now"
      >Run now</button>
    </div>
    <div class="mt-1 pl-3">
      <WorkflowJobNotes automationId={automation.id} />
    </div>
  {/each}
</div>
