<script lang="ts">
  // Per-state evidence (UI-SPEC §4.3, Evidence column). Read-only by design:
  // everything that changes a run lives in the fixed action-row footer, so
  // this block can be reasoned about as "what the human is looking at".
  //
  // R2 — no envelopes, no schemas, no gate traces. The only things pulled out
  // of an envelope are the question a phase asked and the named outputs a
  // stopped phase managed to record.

  import WorkflowGateDiff from './WorkflowGateDiff.svelte';
  import WorkflowFailureEvidence from './WorkflowFailureEvidence.svelte';
  import WorkflowDisposition from './WorkflowDisposition.svelte';
  import type { WorkItem, WorkflowItemDetail } from '../../types/workflow';
  import { parseWorkflowDisposition } from '../../types/workflow';
  import type { WorkflowFailedUnit, WorkflowResolutionKind } from '../../utils/workflowActionRows';
  import { workflowPartialOutputs, workflowQuestionText } from '../../utils/workflowEnvelope';
  import { workflowAge, workflowMetaLine } from '../../stores/workflowData';
  import { workflowClosedDuration } from '../../utils/format';
  import { runMapNodeStyle, type RunMapNodeStyle } from '../../utils/workflowRunMapStyle';

  interface Props {
    item: WorkItem;
    detail: WorkflowItemDetail;
    kind: WorkflowResolutionKind;
    /** The unit a `needs-human(unit-failed)` park is about. */
    failedUnit: WorkflowFailedUnit | null;
    /** Enter on a gate expands the first changed file in place (§8). */
    expandFirstDiff: boolean;
  }
  let { item, detail, kind, failedUnit, expandFirstDiff }: Props = $props();

  // A run that could not finish shows one evidence block whichever state it
  // stopped in — the diagnosis is the same question, and only the way back
  // differs (`failed` reruns, `blocked` resumes). The check strip is suppressed
  // for both because the failure evidence carries the checks that matter.
  // Provider retry exhaustion resolves on the `paused` row because a bare
  // resume continues its session, but it still keeps the diagnosis block.
  // Loop-limit and legacy retry parks do too, whichever action row they use.
  let unresolved = $derived(
    kind === 'failed' ||
      kind === 'blocked' ||
      item.reason === 'provider-retries-exhausted' ||
      item.reason === 'loop-limit-exhausted' ||
      item.reason === 'retries-exhausted',
  );

  let checkPhases = $derived.by(() => {
    const checks = new Set(detail.checkPhaseIds ?? []);
    return (detail.phases ?? []).filter((phase) => checks.has(phase.phaseId));
  });
  // The thread whose workspace holds the changes: the newest attempt that ran.
  let diffThreadId = $derived([...(detail.phases ?? [])].reverse().find((phase) => phase.threadId)?.threadId ?? '');
  let question = $derived(workflowQuestionText(detail));
  let partialOutputs = $derived(workflowPartialOutputs(detail));
  let disposition = $derived(parseWorkflowDisposition(item.disposition));

  // Why the ENGINE stopped it, when the engine is what diagnosed the stop: a
  // worktree that would not cut, a phase missing from the frozen definition, a
  // spent budget. Such an attempt ran no turn, so it has no envelope and every
  // other block on this page has nothing to show for it. Read off the LAST
  // attempt only — the one the run is resting on. Scanning back for any
  // attempt with a cause would resurrect an earlier, already-repaired park as
  // the current diagnosis (the engine clears the cause on reopen, but a
  // `--phase` repair leaves the old parked row behind).
  let parkCause = $derived(((detail.phases ?? []).at(-1))?.cause ?? '');

  let pausedReceipt = $derived.by(() => {
    const ago = `${workflowAge(item.endedAt || item.startedAt || item.createdAt)} ago`;
    if (item.reason === 'interrupted') return 'interrupted — the app was restarted';
    // A checkpoint park is the one stop nobody has to diagnose: it happened
    // because it was asked for, and the partial outputs below it are a wave's
    // worth of finished work rather than the wreckage of an unfinished one.
    if (item.reason === 'checkpoint') return `stopped at your checkpoint · ${ago}`;
    if (item.reason === 'provider-retries-exhausted') {
      return `provider retries exhausted — resume continues where it stopped · ${ago}`;
    }
    if (item.reason === 'loop-limit-exhausted') {
      return `workflow loop limit exhausted — restart from an earlier phase · ${ago}`;
    }
    // Compatibility copy for runs persisted before the causes were split.
    if (item.reason === 'retries-exhausted') {
      return `retry or loop limit exhausted — inspect the cause before resuming · ${ago}`;
    }
    return `paused by you · ${ago}`;
  });

  /**
   * The check strip speaks the run map's vocabulary (R1), not a green one of
   * its own: a passing check is DONE, which is neutral — success is the
   * expected outcome and carries no attention — and only a failure takes a
   * hue. Routed through `runMapNodeStyle` so the glyph and the tone are the
   * same ones the node for that phase draws two blocks up the page.
   */
  // Anything not yet decided reads as pending rather than running: the strip is
  // one glyph per check, and `running`'s glyph is the spinner this row does not
  // draw. The strip only renders for a run that is NOT unresolved, so a check
  // still in flight here is the rare interleaving, not the normal case.
  function checkStyle(status: string): RunMapNodeStyle {
    if (status === 'completed') return runMapNodeStyle('done');
    if (status === 'failed') return runMapNodeStyle('failed');
    return runMapNodeStyle('pending');
  }
</script>

<div class="space-y-3 px-4" data-testid="workflow-evidence" data-kind={kind}>
  {#if checkPhases.length > 0 && !unresolved}
    <section data-testid="workflow-checks">
      <h3 class="mb-1 text-[0.6875rem] font-semibold uppercase tracking-wider text-fg-muted">Checks</h3>
      <div class="flex flex-wrap gap-x-3 gap-y-1 text-xs">
        {#each checkPhases as phase (phase.phaseId + ':' + phase.attempt)}
          {@const style = checkStyle(phase.status)}
          <span class={style.tone} data-testid="workflow-check" data-check-status={phase.status}>
            {style.glyph} {phase.phaseId} {workflowClosedDuration(phase.startedAt, phase.endedAt ?? 0)}
          </span>
        {/each}
      </div>
    </section>
  {/if}

  {#if parkCause}
    <blockquote class="border-l-2 border-error/60 pl-3 text-sm text-fg-muted" data-testid="workflow-park-cause">
      {parkCause}
    </blockquote>
  {/if}

  {#if kind === 'gate' || kind === 'done'}
    <WorkflowGateDiff threadId={diffThreadId} baseBranch={item.baseBranch ?? ''} expandFirst={expandFirstDiff} />
  {/if}

  {#if kind === 'question'}
    <blockquote class="border-l-2 border-warning/60 pl-3 text-sm italic text-fg" data-testid="workflow-question">
      {question || 'The phase needs an answer.'}
    </blockquote>
  {/if}

  {#if unresolved}
    <WorkflowFailureEvidence {detail} />
  {/if}

  {#if kind === 'paused' || kind === 'checkpoint' || item.reason === 'loop-limit-exhausted'}
    <section class="space-y-1 text-sm" data-testid="workflow-paused-receipt">
      <p class="text-fg-muted">{pausedReceipt}</p>
      {#if partialOutputs.length > 0}
        <ul class="space-y-0.5 border-l-2 border-border-subtle pl-3 text-xs text-fg-muted" data-testid="workflow-partial-outputs">
          {#each partialOutputs as line (line)}<li class="truncate">{line}</li>{/each}
        </ul>
      {/if}
    </section>
  {/if}

  {#if kind === 'unit-failed' && failedUnit}
    <section class="space-y-1 text-sm" data-testid="workflow-unit-failure">
      <p class="text-error">✗ {failedUnit.label}{failedUnit.meta ? ` — ${failedUnit.meta}` : ''}</p>
      {#if failedUnit.unit.feedback}
        <blockquote class="border-l-2 border-error/60 pl-3 text-xs italic text-fg-muted" data-testid="workflow-unit-diagnosis">
          {failedUnit.unit.feedback}
        </blockquote>
      {/if}
    </section>
  {/if}

  {#if kind === 'taken-over'}
    <p class="text-sm text-fg-muted" data-testid="workflow-takeover-receipt">
      {workflowMetaLine(['under your control', `since ${workflowAge(item.endedAt || item.startedAt || item.createdAt)} ago`])}
    </p>
  {/if}

  {#if kind === 'cancelled'}
    <p class="text-sm text-fg-muted" data-testid="workflow-cancelled-receipt">
      cancelled · worktree {item.worktreePath ? 'kept' : 'not created'}
    </p>
  {/if}

  {#if disposition}
    <WorkflowDisposition {item} {disposition} />
  {/if}
</div>
