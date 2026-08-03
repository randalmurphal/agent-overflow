<script lang="ts">
  // The step list of a worktree setup run. Split out of WorktreeSetupPanel so
  // the panel stays a shell (chrome, output, actions) and this stays a pure
  // projection of two index-aligned arrays.
  import type { WorktreeSetupStep, WorktreeSetupStepStatus } from '../../stores/worktreeSetup.svelte';

  let {
    steps,
    statuses,
  }: {
    steps: WorktreeSetupStep[];
    statuses: WorktreeSetupStepStatus[];
  } = $props();

  const MARKERS: Record<WorktreeSetupStepStatus, { glyph: string; class: string }> = {
    pending: { glyph: '○', class: 'text-fg-muted' },
    running: { glyph: '●', class: 'text-info' },
    succeeded: { glyph: '✓', class: 'text-success' },
    failed: { glyph: '✗', class: 'text-error' },
  };

  function statusOf(index: number): WorktreeSetupStepStatus {
    return statuses[index] ?? 'pending';
  }
</script>

<ol class="flex flex-col gap-0.5" data-testid="worktree-setup-steps">
  {#each steps as step (step.index)}
    {@const status = statusOf(step.index)}
    {@const marker = MARKERS[status]}
    <li
      class="flex items-baseline gap-2 text-xs"
      data-testid="worktree-setup-step"
      data-step-status={status}
    >
      <span
        class="{marker.class} {status === 'running' ? 'animate-pulse' : ''} w-3 shrink-0 text-center"
        aria-hidden="true"
      >{marker.glyph}</span>
      <span
        class="truncate font-mono {status === 'failed' ? 'text-error' : 'text-text-secondary'}"
        title={step.label}
      >{step.label}</span>
    </li>
  {/each}
</ol>
