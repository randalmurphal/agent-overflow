<script lang="ts">
  // The activity rail's Background segment toggle: icon, name, row count
  // and a pulse while a row runs. Shared by the composer's rail and an
  // agent pane's rail, which each own their open state and body.
  import { activityRailChipClasses } from './activityRailClasses';
  import Icon from '../primitives/Icon.svelte';
  import SendToBack from '@lucide/svelte/icons/send-to-back';

  let {
    count,
    running,
    open,
    onToggle,
    controls,
    testIdPrefix = 'activity-rail',
  }: {
    count: number;
    running: boolean;
    open: boolean;
    onToggle: () => void;
    /** Id of the body the toggle opens. */
    controls: string;
    testIdPrefix?: string;
  } = $props();
</script>

<button
  type="button"
  class="{activityRailChipClasses} shrink-0 text-fg-muted transition-colors hover:bg-surface-2/45 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 {open ? 'bg-accent/10 text-accent' : ''}"
  onclick={onToggle}
  aria-controls={controls}
  aria-expanded={open}
  aria-label={`Background ${count}`}
  title="Background"
  data-testid="{testIdPrefix}-background-toggle"
>
  <Icon
    icon={SendToBack}
    size={11}
    strokeWidth={2.25}
    class="shrink-0 text-fg-hint/70"
  />
  <span data-activity-rail-name>Background</span>
  <span
    class="rounded-[var(--radius-field)] bg-accent/15 px-1 text-[0.625rem] font-medium text-accent"
    data-testid="{testIdPrefix}-background-count"
  >{count}</span>
  {#if running}
    <span
      class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse"
      aria-hidden="true"
      data-testid="{testIdPrefix}-background-pulse"
    ></span>
  {/if}
</button>
