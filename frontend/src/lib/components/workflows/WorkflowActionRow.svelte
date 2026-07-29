<script lang="ts">
  // The fixed action-row footer (UI-SPEC §4.3), primary first, each button
  // carrying the key §8 binds it to. Every mutation on the run detail goes
  // through here, which is why this is also where the §8 key target is
  // registered: one component decides what `a` means in each state.
  //
  // Destructive actions never fire on first press — `Drop unit` and `Stop this
  // run` arm a confirm (Esc disarms, §2.2) and `Discard` opens the §4.5 loss
  // preview. There is no un-previewed destructive path.
  //
  // Receipts, toasts and the sweep auto-advance are the shared resolution path
  // in stores/workflowResolve.ts — the discard dialog resolves through the
  // same one.

  import { tick } from 'svelte';
  import type { WorkItem, WorkflowItemDetail } from '../../types/workflow';
  import {
    workflowActionForKey,
    workflowActionRow,
    workflowResolutionKind,
    type WorkflowActionButton,
    type WorkflowActionKey,
  } from '../../utils/workflowActionRows';
  import { workflowActionConfirmationKey, type WorkflowActionRequest } from '../../stores/workflowActions';
  import { resolveWorkflowRun } from '../../stores/workflowResolve';
  import { registerWorkflowsActionTarget } from '../../stores/workflowCommands.svelte';
  import { openWorkflowThreadById, takeOverWorkflowUnit } from '../../stores/workflowThreads';
  import { getWorkflowReceipt } from '../../stores/workflowRuns.svelte';
  import {
    closeWorkflowsOverlay,
    getWorkflowArmedAction,
    popWorkflowsOverlay,
    setWorkflowArmedAction,
    setWorkflowsOverlayDialog,
  } from '../../stores/workflowsOverlay.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { isViewOnlySession } from '../../transport/runMode';

  interface Props {
    item: WorkItem;
    detail: WorkflowItemDetail;
    /** Root-tree total, shown on the all-clear summary (§4.4). */
    costUsd: number;
    /** Names the phase an approved gate routes to. */
    nextPhaseId: string;
    /** The unit a `needs-human(unit-failed)` park is about (§4.3). */
    failedUnitId: string;
    failedUnitThreadId: string;
    /** Enter's other meaning on a gate: expand the first changed file (§8). */
    onToggleFirstDiff: () => void;
  }
  let {
    item, detail, costUsd, nextPhaseId, failedUnitId, failedUnitThreadId, onToggleFirstDiff,
  }: Props = $props();

  let viewOnly = $derived(isViewOnlySession());
  const localOnly = $derived(viewOnly ? 'Local only' : undefined);
  let kind = $derived(workflowResolutionKind(item));
  let row = $derived(workflowActionRow({ kind, nextPhaseId }));
  let receipt = $derived(getWorkflowReceipt(item.id));
  let armed = $derived(getWorkflowArmedAction());
  let latestThreadId = $derived([...(detail.phases ?? [])].reverse().find((phase) => phase.threadId)?.threadId ?? '');

  let busy = $state(false);
  let noteFor = $state<'request-changes' | 'rerun' | null>(null);
  let note = $state('');
  let answer = $state('');
  let answerInput = $state<HTMLInputElement | undefined>(undefined);
  let noteInput = $state<HTMLInputElement | undefined>(undefined);

  async function act(request: WorkflowActionRequest): Promise<void> {
    if (viewOnly || busy) return;
    busy = true;
    try {
      if (await resolveWorkflowRun(item, request, costUsd)) {
        noteFor = null;
        note = '';
        answer = '';
      }
    } finally {
      busy = false;
    }
  }

  async function openNote(target: 'request-changes' | 'rerun'): Promise<void> {
    noteFor = target;
    note = '';
    await tick();
    noteInput?.focus();
  }

  function commitNote(): void {
    if (noteFor === 'request-changes') void act({ kind: 'request-changes', note: note.trim() });
    else if (noteFor === 'rerun') void act({ kind: 'rerun', guidance: note.trim() });
  }

  // The only thread hand-off left on this row: detach one live unit from engine
  // control and open the thread it is already running in. Nothing here creates
  // a thread — every run-level spawn was removed (D32).
  async function takeOverUnit(): Promise<void> {
    if (viewOnly || busy) return;
    if (!failedUnitId) {
      addToast('warning', 'No failed unit to take over.');
      return;
    }
    await takeOverWorkflowUnit(item.id, failedUnitId, failedUnitThreadId);
  }

  function run(action: WorkflowActionButton): void {
    if (viewOnly || busy) return;
    if (action.arms) {
      const key = workflowActionConfirmationKey(action.id, item);
      if (armed !== key) {
        setWorkflowArmedAction(key);
        return;
      }
      setWorkflowArmedAction(null);
    }
    switch (action.id) {
      case 'approve': void act({ kind: 'approve' }); return;
      case 'request-changes': void openNote('request-changes'); return;
      case 'rerun': void openNote('rerun'); return;
      case 'resume': void act({ kind: 'resume' }); return;
      case 'complete-takeover': void act({ kind: 'complete-takeover' }); return;
      case 'retry-unit':
        if (!failedUnitId) addToast('warning', 'No failed unit to retry.');
        else void act({ kind: 'retry-unit', unitId: failedUnitId, note: '' });
        return;
      case 'drop-unit':
        if (!failedUnitId) addToast('warning', 'No failed unit to drop.');
        else void act({ kind: 'drop-unit', unitId: failedUnitId, note: '' });
        return;
      case 'take-over-unit': void takeOverUnit(); return;
      case 'merge': void act({ kind: 'merge' }); return;
      case 'create-pr': void act({ kind: 'create-pr' }); return;
      case 'open-phase-thread': void openWorkflowThreadById(latestThreadId); return;
      case 'pause': void act({ kind: 'pause' }); return;
      case 'cancel': void act({ kind: 'cancel' }); return;
      // Preview is consent (§4.5): discard never destroys from this row.
      case 'discard': setWorkflowsOverlayDialog('discard'); return;
      case 'back': if (!popWorkflowsOverlay()) closeWorkflowsOverlay(); return;
    }
  }

  function sendAnswer(): void {
    const value = answer.trim();
    if (!value) return;
    void act({ kind: 'answer', answer: value });
  }

  // §8 key target. `a` on a question focuses the input instead of firing an
  // action, because a question has no committable primary until it is typed.
  $effect(() => registerWorkflowsActionTarget({
    action(key: WorkflowActionKey) {
      if (viewOnly || receipt) return;
      if (key === 'a' && kind === 'question') {
        answerInput?.focus();
        return;
      }
      const action = workflowActionForKey(row, key);
      if (action) run(action);
    },
    enter() {
      if (viewOnly || receipt) return;
      if (noteFor) commitNote();
      else if (kind === 'question' && answer.trim()) sendAnswer();
      else onToggleFirstDiff();
    },
  }));

  function label(action: WorkflowActionButton): string {
    return action.arms && armed === workflowActionConfirmationKey(action.id, item)
      ? `${action.label} — confirm?`
      : action.label;
  }

  const variantClass: Record<WorkflowActionButton['variant'], string> = {
    primary: 'bg-accent text-white hover:bg-accent/90',
    secondary: 'border border-border-subtle text-fg hover:bg-surface-2',
    ghost: 'text-fg-muted hover:text-fg',
    'danger-outline': 'border border-error/40 text-error hover:bg-error/10',
  };
</script>

<footer class="sticky bottom-0 border-t border-border-subtle bg-surface-1" data-testid="workflow-action-row">
  {#if receipt}
    <div class="flex items-center gap-3 bg-success/10 px-4 py-3 text-sm text-success" data-testid="workflow-resolved-receipt">
      <span class="min-w-0 flex-1">✓ {receipt.message}</span>
      <button
        class="rounded-md border border-success/30 px-2 py-1 text-xs"
        onclick={() => { if (!popWorkflowsOverlay()) closeWorkflowsOverlay(); }}
        data-testid="workflow-receipt-back"
      >Back</button>
    </div>
  {:else}
    {#if kind === 'question'}
      <form
        class="flex gap-2 px-4 pt-3"
        onsubmit={(event) => { event.preventDefault(); sendAnswer(); }}
        data-testid="workflow-answer-form"
      >
        <input
          bind:this={answerInput}
          bind:value={answer}
          class="min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm disabled:cursor-not-allowed disabled:opacity-50"
          placeholder="Answer — the phase resumes where it yielded"
          disabled={viewOnly}
          title={localOnly}
          data-testid="workflow-answer-input"
        />
        <button
          class="shrink-0 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white disabled:cursor-not-allowed disabled:opacity-50"
          disabled={viewOnly || busy || !answer.trim()}
          title={localOnly}
          data-testid="workflow-answer-send"
        >Send</button>
      </form>
    {/if}

    {#if noteFor}
      <form
        class="flex gap-2 px-4 pt-3"
        onsubmit={(event) => { event.preventDefault(); commitNote(); }}
        data-testid="workflow-note-form"
      >
        <input
          bind:this={noteInput}
          bind:value={note}
          class="min-w-0 flex-1 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-sm disabled:cursor-not-allowed disabled:opacity-50"
          placeholder={noteFor === 'rerun' ? 'Guidance for the new attempt (optional)' : 'What needs to change (optional)'}
          disabled={viewOnly}
          title={localOnly}
          data-testid="workflow-note-input"
        />
        <button
          class="shrink-0 rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white disabled:cursor-not-allowed disabled:opacity-50"
          disabled={viewOnly || busy}
          title={localOnly}
          data-testid="workflow-note-send"
        >Send</button>
      </form>
    {/if}

    <div class="flex flex-wrap items-center gap-2 px-4 py-3">
      {#each row as action (action.id)}
        <button
          class={['rounded-md px-3 py-1.5 text-xs disabled:cursor-not-allowed disabled:opacity-50', variantClass[action.variant]].join(' ')}
          onclick={() => run(action)}
          disabled={viewOnly || busy}
          title={localOnly}
          data-testid="workflow-action"
          data-action-id={action.id}
        >{label(action)}{#if action.key}<kbd class="ml-1.5 opacity-60">{action.key}</kbd>{/if}</button>
      {/each}
    </div>
  {/if}
</footer>
