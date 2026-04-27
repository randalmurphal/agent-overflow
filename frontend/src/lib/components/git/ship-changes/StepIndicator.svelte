<script lang="ts">
  // Three-step progress rail for the Ship Changes drawer. Renders the three
  // stages (Commit / Push / PR) with per-step status dots. Each step's
  // visual state is derived from the active phase so the rail mirrors the
  // state machine exactly — no duplicated state.

  import type { ShipChangesPhase } from '../../../stores/shipChanges.svelte';
  import { forgeLabels } from '../../../utils/forgeLabels';

  let { phase, forge }: { phase: ShipChangesPhase; forge?: string } = $props();

  type StepState = 'pending' | 'active' | 'busy' | 'error' | 'done';
  interface Step {
    key: 'commit' | 'push' | 'pr';
    label: string;
  }

  // Step 3 label adapts to the detected forge ("Open PR" / "Open MR").
  // Internal stage keys stay `pr.*` regardless of forge — split on `.`
  // below depends on it, and renaming would cascade through the whole
  // state machine for cosmetic gain only.
  let labels = $derived(forgeLabels(forge));
  let steps = $derived<Step[]>([
    { key: 'commit', label: 'Commit' },
    { key: 'push', label: 'Push' },
    { key: 'pr', label: labels.openAction },
  ]);

  function stepState(stepKey: 'commit' | 'push' | 'pr', current: ShipChangesPhase): StepState {
    // The phases are prefixed with the step name, so a simple prefix match
    // tells us whether the active phase belongs to this step.
    const [stepOfPhase, kind] = current.split('.') as [string, string | undefined];
    if (current === 'idle') return stepKey === 'commit' ? 'active' : 'pending';

    if (stepOfPhase === stepKey) {
      if (kind === 'busy') return 'busy';
      if (kind === 'error') return 'error';
      if (kind === 'done') return 'done';
      return 'active';
    }

    const order = { commit: 0, push: 1, pr: 2 } as const;
    if (stepOfPhase in order && stepKey in order) {
      const cur = order[stepOfPhase as keyof typeof order];
      const mine = order[stepKey as keyof typeof order];
      if (mine < cur) return 'done';
      if (mine > cur) return 'pending';
    }
    return 'pending';
  }
</script>

<ol
  class="flex items-center gap-2 text-[11px] uppercase tracking-wide text-text-secondary/70"
  aria-label="Ship Changes progress"
  data-testid="ship-changes-steps"
>
  {#each steps as step, i (step.key)}
    {@const state = stepState(step.key, phase)}
    <li class="flex items-center gap-1" data-step={step.key} data-state={state}>
      <span
        aria-hidden="true"
        class="flex h-4 w-4 items-center justify-center rounded-full border text-[9px]
          {state === 'done'
            ? 'bg-accent border-accent text-surface-0'
            : state === 'active'
              ? 'border-accent text-accent'
              : state === 'busy'
                ? 'border-accent text-accent animate-pulse'
                : state === 'error'
                  ? 'border-error text-error'
                  : 'border-border text-text-secondary/40'}"
      >
        {#if state === 'done'}
          &#10003;
        {:else if state === 'error'}
          !
        {:else}
          {i + 1}
        {/if}
      </span>
      <span
        class={state === 'pending'
          ? 'text-text-secondary/40'
          : state === 'error'
            ? 'text-error'
            : state === 'done'
              ? 'text-text-secondary'
              : 'text-text-primary'}
      >
        {step.label}
      </span>
      {#if i < steps.length - 1}
        <span aria-hidden="true" class="mx-1 h-px w-4 bg-border/70"></span>
      {/if}
    </li>
  {/each}
</ol>
