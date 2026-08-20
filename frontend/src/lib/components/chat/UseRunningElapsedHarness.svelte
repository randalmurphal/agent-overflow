<script lang="ts">
  import {
    __runningElapsedTickerSubscribersForTest,
    createRunningElapsed,
  } from './useRunningElapsed.svelte';

  let {
    firstRunning = true,
    secondRunning = false,
    createdAt = 1,
    churnItem = { status: 'idle' },
  }: {
    firstRunning?: boolean;
    secondRunning?: boolean;
    createdAt?: number;
    // Mirrors the production call sites: `isTicking` closures read a status
    // off an item OBJECT that streaming re-derives fresh per delta. The
    // churn test replaces this prop with a value-equal fresh object.
    churnItem?: { status: string };
  } = $props();

  const first = createRunningElapsed(
    () => firstRunning,
    () => createdAt,
  );
  const second = createRunningElapsed(
    () => secondRunning,
    () => createdAt,
  );
  const churn = createRunningElapsed(
    () => churnItem.status === 'running',
    () => createdAt,
  );
</script>

<div data-testid="first-label">{first.label}</div>
<div data-testid="second-label">{second.label}</div>
<div data-testid="churn-label">{churn.label}</div>
<div data-testid="subscriber-count">{__runningElapsedTickerSubscribersForTest()}</div>
