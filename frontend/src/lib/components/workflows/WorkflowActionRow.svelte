<script lang="ts">
  import { tick } from 'svelte';
  import type { WorkItem, WorkflowItemDetail, WorkflowResolvedReceipt } from '../../types/workflow';
  import { parseWorkflowDisposition } from '../../types/workflow';
  import {
    GetThread,
    WorkflowDiscussPR,
    WorkflowFetchPRReviewComments,
    WorkflowOpenTriageThread,
    WorkflowSendPRReviewCommentsToThread,
  } from '../../stores/bindings';
  import { dispatchWorkflowAction, workflowActionConfirmationKey, type WorkflowAction } from '../../stores/workflowActions';
  import { setWorkflowActionKeyHandler, type WorkflowActionKey } from '../../stores/workflowCommands.svelte';
  import { openThreadInNewPane } from '../../stores/panes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    getWorkflowArmedAction,
    getWorkflowReceipts,
    popWorkflowLevel,
    recordWorkflowReceipt,
    setWorkflowArmedAction,
    workflowThreadFromWire,
  } from '../../stores/workflowsPane.svelte';
  import { isViewOnlySession } from '../../transport/runMode';

  interface Props { detail: WorkflowItemDetail; questionText?: string; approveTarget?: string; onToggleFirstDiff?: () => void }
  let { detail, questionText = '', approveTarget = '', onToggleFirstDiff }: Props = $props();
  let item = $derived(detail.item as WorkItem);
  let disposition = $derived(parseWorkflowDisposition(item.disposition));
  let cost = $derived(detail.usage.costUsd || 0);
  let receipt = $derived(getWorkflowReceipts().get(item.id));
  let armed = $derived(getWorkflowArmedAction());
  let rejectOpen = $state(false);
  let rejectNote = $state('');
  let answer = $state('');
  let busy = $state(false);
  let prBusy = $state(false);
  let prReviewCounts = $state<Record<string, { count?: number; error?: string }>>({});
  const prReviewFetches = new Map<string, Promise<void>>();
  let answerInput: HTMLInputElement | undefined = $state(undefined);
  let rejectInput: HTMLInputElement | undefined = $state(undefined);
  let viewOnly = $derived(isViewOnlySession());
  let prReviewState = $derived(prReviewCounts[item.id]);
  let prReviewTitle = $derived(
    viewOnly
      ? 'Local only'
      : prReviewState?.error ?? (prReviewState?.count === undefined ? 'Loading review comments' : undefined),
  );

  async function act(action: WorkflowAction): Promise<void> {
    if (viewOnly || busy) return;
    busy = true;
    try {
      const result = await dispatchWorkflowAction(item, action, cost);
      setWorkflowArmedAction(null);
      rejectOpen = false;
      if (result) {
        recordWorkflowReceipt(result, true);
        addToast('success', result.message);
      } else {
        addToast('info', 'Teardown — turn stopped, locks released, worktree kept');
      }
    } catch (error) {
      addToast('error', userFacingError(error, 'Workflow action failed.'));
    } finally {
      busy = false;
    }
  }

  function armOrAct(kind: 'discard' | 'cancel'): void {
    if (viewOnly) return;
    const key = confirmationKey(kind);
    if (armed !== key) { setWorkflowArmedAction(key); return; }
    void act({ kind });
  }

  function confirmationKey(kind: 'discard' | 'cancel'): string {
    return workflowActionConfirmationKey(kind, item);
  }

  async function openReject(): Promise<void> {
    rejectOpen = true;
    await tick();
    rejectInput?.focus();
  }

  async function openPhaseThread(takeover = false): Promise<void> {
    if (viewOnly && takeover) return;
    const phase = [...detail.phases].reverse().find((entry) => Boolean(entry.threadId));
    if (!phase?.threadId) { addToast('error', 'No phase thread is available'); return; }
    try {
      const thread = await GetThread(phase.threadId);
      await openThreadInNewPane(workflowThreadFromWire(thread));
      if (takeover) addToast('info', 'Turn interrupted — the review thread is yours to steer');
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open the phase thread.'));
    }
  }

  async function openTriage(): Promise<void> {
    if (viewOnly) return;
    try {
      const thread = await WorkflowOpenTriageThread(item.id);
      await openThreadInNewPane(workflowThreadFromWire(thread));
      const result: WorkflowResolvedReceipt = {
        itemId: item.id, kind: 'handed-off', message: "Continuing with agent — thread opened in the run's worktree", costUsd: cost,
      };
      recordWorkflowReceipt(result, true);
      addToast('success', result.message);
    } catch (error) {
      addToast('error', userFacingError(error, 'Could not open a triage thread.'));
    }
  }

  async function loadPRReviewCount(itemId: string): Promise<void> {
    if (prReviewCounts[itemId] || prReviewFetches.has(itemId)) return;
    prReviewCounts = { ...prReviewCounts, [itemId]: {} };
    const request = (async () => {
      try {
        const result = await WorkflowFetchPRReviewComments(itemId);
        prReviewCounts = { ...prReviewCounts, [itemId]: { count: result.count } };
      } catch (error) {
        prReviewCounts = { ...prReviewCounts, [itemId]: { error: userFacingError(error, 'Could not load review comments.') } };
      } finally {
        prReviewFetches.delete(itemId);
      }
    })();
    prReviewFetches.set(itemId, request);
    await request;
  }

  async function openPRThread(kind: 'review' | 'discuss'): Promise<void> {
    if (viewOnly || prBusy || (kind === 'review' && prReviewState?.count === undefined)) return;
    prBusy = true;
    try {
      const thread = kind === 'review'
        ? await WorkflowSendPRReviewCommentsToThread(item.id)
        : await WorkflowDiscussPR(item.id);
      await openThreadInNewPane(workflowThreadFromWire(thread));
    } catch (error) {
      addToast('error', userFacingError(error, kind === 'review' ? 'Could not send PR review comments.' : 'Could not discuss this PR.'));
    } finally {
      prBusy = false;
    }
  }

  function suggestedAnswers(): string[] {
    return questionText
      .split(/\n|;/)
      .filter((line) => /^\s*\d+[.)]\s+/.test(line))
      .map((line) => line.replace(/^\s*\d+[.)]\s*/, '').trim())
      .filter(Boolean)
      .slice(0, 9);
  }

  function handleActionKey(key: WorkflowActionKey): void {
    if (viewOnly || receipt) return;
    if (disposition) {
      if (key === 't') void openTriage();
      return;
    }
    if (key === 'enter') { onToggleFirstDiff?.(); return; }
    if (key.startsWith('digit-') && item.reason === 'question') {
      const value = suggestedAnswers()[Number(key.slice(6)) - 1];
      if (value) void act({ kind: 'answer', answer: value });
      return;
    }
    if (key === 't') {
      if (item.state === 'failed' || item.state === 'done') void openTriage();
      else if (item.state === 'needs-human' && (item.reason === 'gate' || item.reason === 'question')) void openPhaseThread(true);
      else if (item.state === 'needs-human') void openTriage();
      return;
    }
    if (key === 'a') {
      if (item.state === 'needs-human' && item.reason === 'disposition') void act({ kind: 'merge' });
      else if (item.reason === 'gate') void act({ kind: 'approve' });
      else if (item.reason === 'question') answerInput?.focus();
      else if (item.state === 'needs-human') void act({ kind: 'resume' });
      else if (item.state === 'failed') void act({ kind: 're-enqueue' });
      else if (item.state === 'done') void act({ kind: 'merge' });
      return;
    }
    if (key === 'r') {
      if (item.reason === 'gate') void openReject();
      else if (item.state === 'needs-human' || item.state === 'failed' || item.state === 'done') armOrAct('discard');
    }
  }

  $effect(() => {
    setWorkflowActionKeyHandler(handleActionKey);
    return () => setWorkflowActionKeyHandler(null);
  });

  $effect(() => {
    if (!viewOnly && disposition?.action === 'pr') void loadPRReviewCount(item.id);
  });
</script>

{#if receipt}
  <div class="flex items-center gap-3 border-t border-border-subtle bg-success/8 px-4 py-3 text-sm text-success" data-testid="wf-resolved-receipt">
    <span class="min-w-0 flex-1">✓ {receipt.message}</span>
    <button class="rounded-md border border-success/30 px-2 py-1 text-xs" onclick={popWorkflowLevel} data-testid="wf-receipt-back">Back</button>
  </div>
{/if}
{#if !receipt || disposition}
  <div class="border-t border-border-subtle bg-surface-1 px-4 py-3" data-testid="wf-action-row">
    {#if rejectOpen}
      <form class="mb-2 flex gap-2" onsubmit={(event) => { event.preventDefault(); void act({ kind: 'reject', note: rejectNote }); }} data-testid="wf-reject-form">
        <input bind:this={rejectInput} bind:value={rejectNote} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} class="min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm disabled:opacity-50" placeholder="Optional note" data-testid="wf-reject-note" />
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-reject-send">Send</button>
      </form>
    {/if}
    <div class="flex flex-wrap items-center gap-2">
      {#if disposition}
        <div class="w-full space-y-1 text-xs" data-testid="wf-disposition-receipt">
          <p class="text-sm text-success">✓ {disposition.action === 'pr' ? `PR ${disposition.prRef ?? 'created'}` : disposition.action === 'merged' ? `${disposition.policy === 'manual' ? `Merged to ${disposition.base || item.baseBranch || 'base'}` : 'Merged automatically'}` : 'Discarded'} · {new Date(disposition.at).toLocaleString()}</p>
          {#if disposition.action === 'merged'}
            <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 gap-y-1 text-fg-muted">
              <dt>merge ·{' '}</dt><dd>{item.branch || 'run branch'} → {disposition.base || item.baseBranch || 'base'} · {disposition.mode === 'ff' ? 'fast-forward' : disposition.mode === 'merge' ? 'merge commit' : 'merged'}{disposition.sha ? ` · ${disposition.sha}` : ''}</dd>
              <dt>policy ·{' '}</dt><dd>{disposition.policy === 'manual' ? 'manual' : 'project opted in; a conflict or dirty base parks for you instead'}</dd>
              {#if disposition.policy !== 'manual' && disposition.sha}<dt>undo ·{' '}</dt><dd class="font-mono">git revert {disposition.sha}</dd>{/if}
            </dl>
          {:else if disposition.action === 'pr'}
            <p class="text-fg-muted">{item.branch || 'run branch'} · policy {disposition.policy}</p>
          {:else}
            <p class="text-fg-muted">branch dropped · record kept · policy {disposition.policy}</p>
          {/if}
        </div>
        {#if disposition.action === 'pr'}
          <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openPRThread('review')} disabled={viewOnly || prBusy || prReviewState?.count === undefined} title={prReviewTitle} data-testid="wf-pr-review-comments">Review comments ({prReviewState?.count ?? '…'})</button>
          <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs disabled:cursor-not-allowed disabled:opacity-50" onclick={() => openPRThread('discuss')} disabled={viewOnly || prBusy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-pr-discuss">Discuss this PR</button>
        {/if}
        {#if !receipt}<button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-receipt-continue">Continue with agent ↗ <kbd>t</kbd></button>{/if}
      {:else if item.state === 'needs-human' && item.reason === 'gate'}
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white" onclick={() => act({ kind: 'approve' })} disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-approve">Approve{approveTarget ? ` → ${approveTarget}` : ''} <kbd>a</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={openReject} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-request-changes">Request changes <kbd>r</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => openPhaseThread(true)} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-take-over">Take over <kbd>t</kbd></button>
      {:else if item.state === 'needs-human' && item.reason === 'question'}
        {#if suggestedAnswers().length > 0}
          <div class="flex w-full flex-wrap gap-1.5" data-testid="wf-suggested-answers">
            {#each suggestedAnswers() as suggestion, index}
              <button class="rounded-md border border-border-subtle px-2 py-1 text-xs" onclick={() => act({ kind: 'answer', answer: suggestion })} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-suggested-answer"><kbd>{index + 1}</kbd> {suggestion}</button>
            {/each}
          </div>
        {/if}
        <input bind:this={answerInput} bind:value={answer} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} class="min-w-40 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm disabled:opacity-50" placeholder="Answer — the phase resumes where it yielded" onkeydown={(event) => { if (event.key === 'Enter' && answer.trim()) void act({ kind: 'answer', answer: answer.trim() }); }} data-testid="wf-answer-input" />
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={() => act({ kind: 'answer', answer: answer.trim() })} disabled={viewOnly || !answer.trim() || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-answer-send">Send</button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => openPhaseThread(true)} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-question-take-over">Take over instead <kbd>t</kbd></button>
      {:else if item.state === 'failed'}
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-continue-agent">Continue with agent <kbd>t</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => act({ kind: 're-enqueue' })} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-resume">Re-enqueue with guidance <kbd>a</kbd></button>
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('discard')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-discard">{armed === confirmationKey('discard') ? 'Discard this run?' : 'Discard'} <kbd>r</kbd></button>
      {:else if item.state === 'needs-human' && item.reason === 'disposition'}
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={() => act({ kind: 'merge' })} disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-merge">Merge to main <kbd>a</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => act({ kind: 'create-pr' })} disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-create-pr">Create PR</button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-done-continue">Continue with agent ↗ <kbd>t</kbd></button>
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('discard')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-done-discard">{armed === confirmationKey('discard') ? 'Discard this run?' : 'Discard'} <kbd>r</kbd></button>
      {:else if item.state === 'needs-human'}
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-parked-continue">Continue with agent <kbd>t</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => act({ kind: 'resume' })} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-parked-resume">Re-enqueue with guidance <kbd>a</kbd></button>
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('discard')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-parked-discard">{armed === confirmationKey('discard') ? 'Discard this run?' : 'Discard'} <kbd>r</kbd></button>
      {:else if item.state === 'done'}
        <button class="rounded-md bg-accent px-3 py-1.5 text-xs text-white" onclick={() => act({ kind: 'merge' })} disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-merge">Merge to main <kbd>a</kbd></button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => act({ kind: 'create-pr' })} disabled={viewOnly || busy} title={viewOnly ? 'Local only' : undefined} data-testid="wf-create-pr">Create PR</button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={openTriage} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-done-continue">Continue with agent ↗ <kbd>t</kbd></button>
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('discard')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-done-discard">{armed === confirmationKey('discard') ? 'Discard this run?' : 'Discard'} <kbd>r</kbd></button>
      {:else if item.state === 'running'}
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={() => openPhaseThread(false)} data-testid="wf-open-phase">Open phase thread</button>
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('cancel')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-stop-run">{armed === confirmationKey('cancel') ? 'Stop this run?' : 'Stop this run'}</button>
      {:else if item.state === 'queued'}
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => act({ kind: 'remove' })} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-remove-queued">Remove from queue</button>
      {:else if item.state === 'cancelled'}
        <button class="rounded-md border border-error/40 px-3 py-1.5 text-xs text-error" onclick={() => armOrAct('discard')} disabled={viewOnly} title={viewOnly ? 'Local only' : undefined} data-testid="wf-discard-worktree">{armed === confirmationKey('discard') ? 'Discard this worktree?' : 'Discard worktree'}</button>
        <button class="rounded-md border border-border-subtle px-3 py-1.5 text-xs" onclick={popWorkflowLevel} data-testid="wf-cancelled-back">Back</button>
      {/if}
    </div>
  </div>
{/if}
