<script lang="ts">
  import type { WorkItemPhase, WorkflowItemDetail } from '../../types/workflow';

  interface FailureEnvelope {
    outputs?: Record<string, unknown> | null;
    reason?: unknown;
  }

  interface Props { detail: WorkflowItemDetail }
  let { detail }: Props = $props();

  function envelope(phase: WorkItemPhase): FailureEnvelope | null {
    if (!phase.outputEnvelope) return null;
    try {
      return (typeof phase.outputEnvelope === 'string'
        ? JSON.parse(phase.outputEnvelope)
        : phase.outputEnvelope) as FailureEnvelope | null;
    } catch (error) {
      console.warn(`workflows: could not parse failure envelope for ${phase.phaseId} attempt ${phase.attempt}`, error);
      return null;
    }
  }

  function textOutput(value: unknown): string {
    return typeof value === 'string' ? value.trim() : '';
  }

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
    return { phase, detailText };
  });

  let diagnosis = $derived.by(() => {
    for (const phase of [...detail.phases].reverse()) {
      const value = textOutput(envelope(phase)?.outputs?.diagnosis);
      if (value) return { attempt: phase.attempt, value };
    }
    return null;
  });

  let reasonWord = $derived.by(() => {
    const reason = detail.item.reason || 'failed';
    return reason.replace(/^check-failed-/, '').replaceAll('-', ' ');
  });
</script>

<section class="space-y-1 text-sm" data-testid="wf-failure-evidence">
  {#if failedCheck}
    <p class="text-error" data-testid="wf-failure-check">✗ {failedCheck.phase.phaseId} — {failedCheck.detailText} ×{failedCheck.phase.attempt} · {reasonWord}</p>
  {:else}
    <p class="text-error" data-testid="wf-failure-check">✗ {detail.item.reason || 'workflow failed'}</p>
  {/if}
  {#if diagnosis}
    <blockquote class="border-l-2 border-error/60 pl-3 italic text-fg-muted" data-testid="wf-failure-diagnosis">diagnosis #{diagnosis.attempt}: “{diagnosis.value}”</blockquote>
  {/if}
</section>
