<script lang="ts">
  import type { WorkflowItemDetail } from '../../types/workflow';
  import { envelopeText as textOutput, parsePhaseEnvelope as envelope } from '../../utils/workflowEnvelope';

  interface Props { detail: WorkflowItemDetail }
  let { detail }: Props = $props();

  let reasonWord = $derived.by(() => {
    const reason = detail.item.reason || 'failed';
    return reason.replace(/^check-failed-/, '').replaceAll('-', ' ');
  });

  // The failure line is folded whole. Spread across the template it was a
  // merged text run over `failedCheck` AND `reasonWord`, so the run could
  // re-render on the reason after the check went null and before the branch
  // tore down.
  let failedCheck = $derived.by(() => {
    const checks = new Set(detail.checkPhaseIds ?? []);
    const candidates = detail.phases.filter((phase) => {
      if (!checks.has(phase.phaseId)) return false;
      const output = envelope(phase)?.outputs;
      return phase.status === 'failed' || output?.passed === false;
    });
    const phase = candidates[candidates.length - 1];
    if (!phase) return null;
    const output = envelope(phase)?.outputs;
    const detailText = textOutput(output?.details)
      || textOutput(output?.detail)
      || textOutput(output?.summary)
      || 'check failed';
    return {
      text: `✗ ${phase.phaseId} — ${detailText} ×${phase.attempt} · ${reasonWord}`,
    };
  });

  let diagnosis = $derived.by(() => {
    for (const phase of [...detail.phases].reverse()) {
      const value = textOutput(envelope(phase)?.outputs?.diagnosis);
      if (value) return { attempt: phase.attempt, value };
    }
    return null;
  });
</script>

<section class="space-y-1 text-sm" data-testid="wf-failure-evidence">
  {#if failedCheck}
    <p class="text-error" data-testid="wf-failure-check">{failedCheck?.text ?? ''}</p>
  {:else}
    <p class="text-error" data-testid="wf-failure-check">✗ {detail.item.reason || 'workflow failed'}</p>
  {/if}
  {#if diagnosis}
    <blockquote class="border-l-2 border-error/60 pl-3 italic text-fg-muted" data-testid="wf-failure-diagnosis">diagnosis #{diagnosis.attempt}: “{diagnosis.value}”</blockquote>
  {/if}
</section>
