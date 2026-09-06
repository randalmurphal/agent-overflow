<script lang="ts">
  import { workflowItemHasScope } from '../../transport/entityScopes';
  // The disposition block on a terminal run (UI-SPEC §4.3 "done", §4.7). A
  // manual disposition renders as a receipt; an auto-merge project's receipt
  // also carries the policy that decided it and the one-line undo.
  //
  // After Create PR the PR block appears: `Review comments (N)` (lazily
  // counted, single-flight — one fetch per run per mount) and `Discuss this
  // PR`. Both ride the run's one linked thread and open it as a normal pane,
  // which closes the overlay (R3).

  import type { WorkItem, WorkflowDispositionReceipt } from '../../types/workflow';
  import { WorkflowDiscussPR, WorkflowFetchPRReviewComments, WorkflowSendPRReviewCommentsToThread } from '../../stores/bindings';
  import type { Thread } from '../../types/models';
  import { openWorkflowThread } from '../../stores/workflowThreads';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';

  interface Props {
    item: WorkItem;
    disposition: WorkflowDispositionReceipt;
  }
  let { item, disposition }: Props = $props();

  // Every control here drives the workflow engine, which is `threads:autonomy`.
  let ungranted = $derived(!workflowItemHasScope('threads:autonomy', item.id));
  let base = $derived(disposition.base || item.baseBranch || 'base');
  let mode = $derived(disposition.mode === 'ff' ? 'fast-forward' : disposition.mode === 'merge' ? 'merge commit' : 'merged');
  let headline = $derived(
    disposition.action === 'merged'
      ? `Merged to ${base} — ${mode}${disposition.sha ? ` ${disposition.sha.slice(0, 8)}` : ''}`
      : disposition.action === 'pr'
        ? `Created ${disposition.prRef || 'pull request'}`
        : 'Discarded — record kept',
  );

  let reviewCount = $state<number | null>(null);
  let reviewError = $state('');
  let busy = $state('');
  let countedFor = '';

  // Lazy + single-flight: the count is a network round trip to the forge, so
  // it is fetched once per (run, PR) and never on a non-PR disposition.
  $effect(() => {
    const key = `${item.id}\n${disposition.prRef ?? ''}`;
    if (ungranted || disposition.action !== 'pr' || key === countedFor) return;
    countedFor = key;
    reviewCount = null;
    reviewError = '';
    void (async () => {
      try {
        const result = await WorkflowFetchPRReviewComments(item.id);
        reviewCount = result.count;
      } catch (err) {
        reviewError = userFacingError(err, 'Could not load review comments.');
      }
    })();
  });

  async function openPRThread(kind: 'review' | 'discuss'): Promise<void> {
    if (ungranted || busy) return;
    busy = kind;
    try {
      const thread = (kind === 'review'
        ? await WorkflowSendPRReviewCommentsToThread(item.id)
        : await WorkflowDiscussPR(item.id)) as Thread;
      if (!thread?.id) throw new Error('No thread was returned');
      await openWorkflowThread(thread);
    } catch (err) {
      addToast('error', userFacingError(
        err,
        kind === 'review' ? 'Could not send the review comments.' : 'Could not open the PR discussion.',
      ));
    } finally {
      busy = '';
    }
  }
</script>

<section class="space-y-1 text-xs" data-testid="workflow-disposition">
  <p class="text-sm text-success" data-testid="workflow-disposition-headline">✓ {headline}</p>
  {#if disposition.action === 'merged'}
    <p class="text-fg-muted">{item.branch || 'run branch'} → {base} · policy {disposition.policy}</p>
    {#if disposition.policy !== 'manual' && disposition.sha}
      <p class="font-mono text-fg-muted" data-testid="workflow-disposition-undo">undo · git revert {disposition.sha}</p>
    {/if}
    {#if disposition.cleanupFailed}
      <p class="text-warning" data-testid="workflow-disposition-cleanup">The worktree could not be removed — it is still on disk.</p>
    {/if}
  {:else if disposition.action === 'pr'}
    <p class="text-fg-muted">{item.branch || 'run branch'} · policy {disposition.policy}</p>
    <div class="flex flex-wrap gap-2 pt-1">
      <button
        class="rounded-md border border-border-subtle px-2.5 py-1 text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => { void openPRThread('review'); }}
        disabled={ungranted || busy !== '' || reviewCount === null}
        title={ungranted ? 'Not granted to this device' : reviewError || undefined}
        data-testid="workflow-pr-review-comments"
      >Review comments ({reviewError ? '–' : reviewCount ?? '…'})</button>
      <button
        class="rounded-md border border-border-subtle px-2.5 py-1 text-fg-muted hover:text-fg disabled:cursor-not-allowed disabled:opacity-50"
        onclick={() => { void openPRThread('discuss'); }}
        disabled={ungranted || busy !== ''}
        title={ungranted ? 'Not granted to this device' : undefined}
        data-testid="workflow-pr-discuss"
      >Discuss this PR</button>
    </div>
  {:else}
    <p class="text-fg-muted">branch dropped · record kept · policy {disposition.policy}</p>
  {/if}
</section>
