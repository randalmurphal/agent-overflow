<script lang="ts">
  // Test host for <ActivityRail>. The rail's visibility and its
  // background-controller/clock ownership live in the HOST (Composer) via
  // createActivityRailHost — the rail itself renders unconditionally when
  // mounted. This fixture wires the rail exactly like Composer does (same
  // factory, same mount gate), so ActivityRail.test.ts exercises the
  // production visibility predicate rather than a reimplementation.
  import { onDestroy, onMount } from 'svelte';
  import ActivityRail from '../../lib/components/composer/ActivityRail.svelte';
  import { createActivityRailHost } from '../../lib/components/composer/activityRailHost.svelte';
  import type { ThreadPane } from '../../lib/stores/thread.svelte';
  import type { UserInputRequest } from '../../lib/types/events';

  interface Props {
    pane: ThreadPane;
    inputRequest?: UserInputRequest | null;
    inputCollapsed?: boolean;
    onToggleInput?: () => void;
  }

  let {
    pane,
    inputRequest = null,
    inputCollapsed = false,
    onToggleInput = () => {},
  }: Props = $props();

  const host = createActivityRailHost(() => pane, () => inputRequest !== null);
  let release: (() => void) | null = null;
  onMount(() => { release = host.mount(); });
  onDestroy(() => { release?.(); });
</script>

{#if host.railVisible}
  <ActivityRail
    {pane}
    bg={host.bg}
    clock={host.clock}
    {inputRequest}
    {inputCollapsed}
    {onToggleInput}
  />
{/if}
