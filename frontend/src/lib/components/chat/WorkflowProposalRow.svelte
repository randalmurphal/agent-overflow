<script lang="ts">
  import type { Item } from '../../types/models';
  import type { WorkflowIntakePrefill } from '../../stores/workflowsPane.svelte';
  import { WorkflowDismissChatProposal, WorkflowQueueChatProposal } from '../../stores/bindings';
  import { openWorkflowIntake, openWorkflowsPane } from '../../stores/workflowsPane.svelte';
  import { refreshWorkflowsSidebar } from '../../stores/workflowsSidebar.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { isViewOnlySession } from '../../transport/runMode';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { userFacingError } from '../../utils/userFacingError';
  import WorkflowConfirmCard from './WorkflowConfirmCard.svelte';

  interface ProposalMeta {
    state: 'pending' | 'queued' | 'dismissed';
    projectId: string;
    projectName: string;
    workflowId: string;
    workflowName: string;
    workflowScope: 'project' | 'shared';
    goal: string;
    seeds: Record<string, unknown>;
    baseBranch: string;
    stepMode: boolean;
  }

  let { item }: { item: Item } = $props();
  let busy = $state(false);
  let viewOnly = $derived(isViewOnlySession());
  let meta = $derived.by((): ProposalMeta | null => {
    const value = parseJsonObject(item.meta);
    if (!value || !['pending', 'queued', 'dismissed'].includes(String(value.state))) return null;
    if (typeof value.projectId !== 'string' || typeof value.projectName !== 'string' ||
        typeof value.workflowId !== 'string' || typeof value.workflowName !== 'string' ||
        (value.workflowScope !== 'project' && value.workflowScope !== 'shared') ||
        typeof value.goal !== 'string' || typeof value.baseBranch !== 'string') return null;
    return {
      state: value.state as ProposalMeta['state'],
      projectId: value.projectId,
      projectName: value.projectName,
      workflowId: value.workflowId,
      workflowName: value.workflowName,
      workflowScope: value.workflowScope,
      goal: value.goal,
      seeds: value.seeds && typeof value.seeds === 'object' && !Array.isArray(value.seeds)
        ? value.seeds as Record<string, unknown> : {},
      baseBranch: value.baseBranch,
      stepMode: value.stepMode === true,
    };
  });
  let prefill = $derived.by((): WorkflowIntakePrefill => meta ? ({
    threadId: item.threadId,
    proposalId: item.id,
    projectId: meta.projectId,
    goal: meta.goal,
    workflowId: meta.workflowId,
    baseBranch: meta.baseBranch,
    seeds: meta.seeds,
    stepMode: meta.stepMode,
  }) : {});

  async function queue(): Promise<void> {
    if (!meta || viewOnly || busy) return;
    busy = true;
    try {
      await WorkflowQueueChatProposal(
        item.threadId, item.id, meta.projectId, meta.workflowId, meta.workflowScope,
        meta.goal, meta.seeds, meta.baseBranch, meta.stepMode,
      );
      addToast('success', 'Queued — starts when a slot frees');
      refreshWorkflowsSidebar();
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not queue the run.'));
    } finally {
      busy = false;
    }
  }

  async function edit(): Promise<void> {
    if (!meta || viewOnly || busy) return;
    try {
      await openWorkflowsPane({ kind: 'overview' });
      openWorkflowIntake(prefill);
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open workflow intake.'));
    }
  }

  async function dismiss(): Promise<void> {
    if (!meta || viewOnly || busy) return;
    busy = true;
    try {
      await WorkflowDismissChatProposal(item.threadId, item.id);
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not dismiss the proposal.'));
    } finally {
      busy = false;
    }
  }
</script>

{#if meta}
  <WorkflowConfirmCard
    projectName={meta.projectName}
    title={meta.goal}
    workflowName={meta.workflowName}
    baseBranch={meta.baseBranch}
    {prefill}
    state={meta.state}
    disabled={viewOnly}
    {busy}
    onQueue={() => void queue()}
    onEdit={() => void edit()}
    onDismiss={() => void dismiss()}
  />
{:else}
  <div class="rounded-lg border border-error/30 bg-error/10 px-3 py-2 text-sm text-error" role="alert">
    This workflow proposal has invalid persisted data.
  </div>
{/if}
