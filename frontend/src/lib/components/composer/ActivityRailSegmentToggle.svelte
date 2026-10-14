<script lang="ts">
  // An activity rail segment's toggle: icon, name, count and a pulse while
  // a row runs. The Background segment uses it in the composer's rail and
  // an agent pane's rail, the Agents segment in the composer's; each host
  // owns the open state and the body.
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';
  import { activityRailChipClasses } from './activityRailClasses';
  import Icon from '../primitives/Icon.svelte';

  let {
    icon,
    name,
    segment,
    count,
    running,
    open,
    onToggle,
    controls,
    testIdPrefix = 'activity-rail',
  }: {
    icon: Component<LucideProps>;
    /** The segment's name, shown on the wide rungs. */
    name: string;
    /** The segment's test id part: `<prefix>-<segment>-toggle`. */
    segment: string;
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
  aria-label={`${name} ${count}`}
  title={name}
  data-testid="{testIdPrefix}-{segment}-toggle"
>
  <Icon
    {icon}
    size={11}
    strokeWidth={2.25}
    class="shrink-0 text-fg-hint/70"
  />
  <span data-activity-rail-name>{name}</span>
  <span
    class="rounded-[var(--radius-field)] bg-accent/15 px-1 text-[0.625rem] font-medium text-accent"
    data-testid="{testIdPrefix}-{segment}-count"
  >{count}</span>
  {#if running}
    <span
      class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse"
      aria-hidden="true"
      data-testid="{testIdPrefix}-{segment}-pulse"
    ></span>
  {/if}
</button>
