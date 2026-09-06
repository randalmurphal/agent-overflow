<script lang="ts">
  import { workflowItemBackend, HOME_BACKEND } from '../../transport/entityIndex';
  // Run detail (UI-SPEC §4) — the resolution surface. Header block, the
  // two-row digest, per-state evidence, the run map, the outputs block, and
  // the fixed action-row footer.
  //
  // THREE sources, deliberately, because they go stale on different clocks:
  // the run RECORD comes from the cached list row (bound thread, live state,
  // patched by `workflow:item-state`); the run's EVIDENCE — the attempts this
  // run recorded, its artifacts, its outputs, its spend — comes from the
  // per-run detail load, which is root-only and evicted when the overlay
  // leaves this run; and the run's SHAPE comes from the run-map entity store,
  // which owns the whole tree and patches itself from the same events
  // (RUN-MAP §4.2). `WorkflowRunMap` attaches that store itself — this
  // component hands it the item id and nothing else.

  import WorkflowRunHeader from './WorkflowRunHeader.svelte';
  import WorkflowEvidence from './WorkflowEvidence.svelte';
  import WorkflowRunMap from './WorkflowRunMap.svelte';
  import WorkflowOutputs from './WorkflowOutputs.svelte';
  import WorkflowActionRow from './WorkflowActionRow.svelte';
  import { parseWorkflowDigest } from '../../types/workflow';
  import {
    failedWorkflowUnitInDetail,
    workflowDigestFallback,
    workflowResolutionKind,
  } from '../../utils/workflowActionRows';
  import { openInEditor } from '../../stores/openInEditor';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { hasScope } from '../../transport/scopes';
  import {
    getWorkflowCatalog,
    getWorkflowCosts,
    getWorkflowDetail,
    getWorkflowRun,
    loadWorkflowDetail,
  } from '../../stores/workflowRuns.svelte';

  interface Props { itemId: string }
  let { itemId }: Props = $props();

  let item = $derived(getWorkflowRun(itemId));
  let detail = $derived(getWorkflowDetail(itemId));
  // The only gated affordance is opening a run artifact in an editor.
  let noHost = $derived(!hasScope('host', workflowItemBackend(itemId) ?? HOME_BACKEND));
  let expandFirstDiff = $state(false);

  $effect(() => {
    void loadWorkflowDetail(itemId);
  });

  // Enter's expansion is per-run: stepping the sweep to the next run must not
  // silently expand a file the human never asked about.
  $effect(() => {
    void itemId;
    expandFirstDiff = false;
  });

  let kind = $derived(item ? workflowResolutionKind(item) : 'running');
  let digest = $derived(item ? parseWorkflowDigest(item.digest) : null);
  let fallback = $derived(workflowDigestFallback(kind, item?.currentPhaseId ?? ''));
  let failedUnit = $derived(detail ? failedWorkflowUnitInDetail(detail) : null);
  // The composed spend, not `usage.costUsd`: that field is the providers'
  // reported half alone, and a Codex phase reports none — a codex-heavy run
  // read as free until the app started pricing its token-only rows.
  let costUsd = $derived(detail?.spend?.costUsd || getWorkflowCosts()[itemId] || 0);
  let terminal = $derived(
    item !== undefined && (item.state === 'done' || item.state === 'failed' || item.state === 'cancelled' || kind === 'done'),
  );

  // `Approve → <next>` names the phase the gate routes to. The definition's
  // declared order is the only place that lives; a run whose definition is
  // gone falls back to the generic label.
  let nextPhaseId = $derived.by(() => {
    if (!item || !detail) return '';
    const phases = getWorkflowCatalog(item.projectId)?.workflows
      .find((definition) => definition.id === item.workflowId)?.phases ?? [];
    const latest = [...(detail.phases ?? [])].reverse()[0]?.phaseId ?? '';
    const index = phases.findIndex((phase) => phase.id === latest);
    return index >= 0 ? phases[index + 1]?.id ?? '' : '';
  });

  async function openArtifact(path: string): Promise<void> {
    if (noHost) return;
    try {
      await openInEditor(workflowItemBackend(itemId) ?? HOME_BACKEND, path, 0, 0, '', '');
    } catch (err) {
      addToast('error', userFacingError(err, 'Could not open that file.'));
    }
  }
</script>

{#if !item}
  <p class="px-4 py-6 text-xs text-fg-muted" data-testid="workflow-run-missing">This run is no longer available.</p>
{:else if !detail}
  <p class="px-4 py-6 text-xs text-fg-muted" data-testid="workflow-run-loading">Loading run…</p>
{:else}
  <div class="flex min-h-full flex-col" data-testid="workflow-run-detail" data-item-id={itemId}>
    <div class="flex-1 space-y-4 pb-4">
      <WorkflowRunHeader {item} {costUsd} />

      <section class="mx-4 grid gap-2 rounded-md border border-border-subtle bg-surface-0 p-3" data-testid="workflow-digest">
        <div>
          <div class="text-[0.625rem] font-semibold uppercase tracking-wider text-fg-muted">What happened</div>
          <p class="mt-0.5 text-sm text-fg">{digest?.whatHappened || fallback.whatHappened}</p>
        </div>
        <div>
          <div class="text-[0.625rem] font-semibold uppercase tracking-wider text-fg-muted">What it needs</div>
          <p class="mt-0.5 text-sm text-fg">{digest?.whatItNeeds || fallback.whatItNeeds}</p>
        </div>
      </section>

      <WorkflowEvidence {item} {detail} {kind} {failedUnit} {expandFirstDiff} />

      <section class="px-4">
        <h3 class="pb-1 text-[0.6875rem] font-semibold uppercase tracking-wider text-fg-muted">Progress</h3>
        <WorkflowRunMap {itemId} />
      </section>

      {#if terminal}
        <div class="px-4">
          <WorkflowOutputs
            values={detail.outputs ?? {}}
            artifacts={detail.artifacts ?? []}
            openDisabled={noHost}
            onOpenArtifact={(path) => { void openArtifact(path); }}
          />
        </div>
      {/if}
    </div>

    <WorkflowActionRow
      {item}
      {detail}
      {costUsd}
      {nextPhaseId}
      failedUnitId={failedUnit?.unit.unitId ?? ''}
      failedUnitThreadId={failedUnit?.threadId ?? ''}
      onToggleFirstDiff={() => { expandFirstDiff = !expandFirstDiff; }}
    />
  </div>
{/if}
