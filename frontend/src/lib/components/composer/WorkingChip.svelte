<script lang="ts">
  // The "working" chip: the per-key spinner sprite (or the stock LED
  // chase), a verb, and an elapsed timer. Two hosts:
  //
  //   - ActivityRail, keyed on the thread's working session
  //     (`stableTurnKey`), with the compaction label/sprite swap;
  //   - AgentPaneComposerShell, keyed on the scoped launch, so the pane's
  //     chip holds its own verb and sprite for the agent's whole run
  //     (user ruling 2026-08-23: the pane shows the timer and the icon
  //     while its agent runs).
  //
  // Both picks hash over (threadId, pickKey), so a re-render or remount
  // never rerolls mid-run. The chip box is `activityRailChipClasses`,
  // which every height-reservation twin (Composer's spacer, the shell's
  // idle placeholder) shares by construction. The testids default to the
  // rail's; a second host prefixes its own.

  import { getSettings } from '../../stores/settings.svelte';
  import { activityRailChipClasses } from './activityRailClasses';
  import WorkingSprite from './WorkingSprite.svelte';
  import { BUILTIN_SPINNER_VERBS } from '../../spinners/builtinVerbs';
  import { assembleVerbPool, pickFromPool } from '../../spinners/pick';
  import { selectWorkingSprite } from '../../spinners/select';
  import { ensureCustomSpinners, peekCustomSpinners } from '../../stores/spinners.svelte';

  interface Props {
    threadId: string;
    /** Stable per-run key the verb and sprite picks hash over. */
    pickKey: string;
    /** Label-only swap to "Compacting" (+ the compaction sprite). Wins over a verb. */
    compacting?: boolean;
    elapsedLabel: string;
    /**
     * False reserves the timer's width without showing a clock — the
     * rail uses it across the pending-send handoff, before the provider
     * supplies its authoritative start.
     */
    showElapsed?: boolean;
    testIdPrefix?: string;
  }

  let {
    threadId,
    pickKey,
    compacting = false,
    elapsedLabel,
    showElapsed = true,
    testIdPrefix = 'activity-rail-working',
  }: Props = $props();

  // One verb per key, drawn from built-ins + custom verbs. Null when the
  // feature is off or the assembled pool is empty — the label falls back
  // to plain "Working".
  let spinnerVerb = $derived.by<string | null>(() => {
    const settings = getSettings();
    if (!settings.spinnerVerbsEnabled) return null;
    const pool = assembleVerbPool(
      BUILTIN_SPINNER_VERBS,
      settings.spinnerCustomVerbs ?? [],
      settings.spinnerBuiltinVerbsDisabled,
    );
    return pickFromPool(pool, threadId, pickKey, 'verb');
  });

  // The compaction label always wins over a spinner verb — it is
  // information.
  let workingLabel = $derived(compacting ? 'Compacting' : (spinnerVerb ?? 'Working'));

  // Custom sprites load lazily and only once animations are actually on;
  // the effect keys on the setting so flipping it mid-session attaches.
  $effect(() => {
    if (getSettings().spinnerAnimationsEnabled) ensureCustomSpinners();
  });

  // The per-key sprite standing in for the LED chase. Compacting swaps
  // to the assigned compaction sprite; null renders the stock LEDs.
  let workingSprite = $derived.by(() => {
    const settings = getSettings();
    if (!settings.spinnerAnimationsEnabled) return null;
    return selectWorkingSprite(
      peekCustomSpinners().sprites,
      settings.spinnerDisabledAnimations ?? [],
      compacting,
      settings.spinnerCompactionAnimation,
      threadId,
      pickKey,
    );
  });

  // The LED chase is a standing animation (4 presents/s — stepped, see
  // the working-indicator note in app.css). Low-power mode drops the
  // chase; the LEDs render static at their resting opacity. The same
  // flag freezes the working sprite at frame 0 when animations are on.
  let ledChase = $derived(!getSettings().lowPowerMode);
</script>

<span
  class="{activityRailChipClasses} shrink-0"
  role="status"
  aria-live="polite"
  data-testid={testIdPrefix}
>
  {#if workingSprite}
    <WorkingSprite sprite={workingSprite} animate={ledChase} />
  {:else}
    <span
      class="inline-flex items-center gap-1 {ledChase ? 'working-leds' : ''}"
      aria-hidden="true"
      data-testid="{testIdPrefix}-leds"
    >
      <span class="working-led h-2 w-1 rounded-[1.5px] bg-accent"></span>
      <span class="working-led h-2 w-1 rounded-[1.5px] bg-accent"></span>
      <span class="working-led h-2 w-1 rounded-[1.5px] bg-accent"></span>
    </span>
  {/if}
  <!-- Keep the DOM and initial timer width stable: visibility reserves
       the elapsed column without showing a clock. -->
  <span class="text-fg-muted" data-testid="{testIdPrefix}-label">
    {workingLabel} <span
      class="tabular-nums text-fg"
      class:invisible={!showElapsed}
      aria-hidden={!showElapsed}
      data-testid="{testIdPrefix}-elapsed"
    >{elapsedLabel}</span>
  </span>
</span>
