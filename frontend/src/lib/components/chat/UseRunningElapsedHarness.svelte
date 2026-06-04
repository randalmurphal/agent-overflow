<script lang="ts">
  import {
    __runningElapsedTickerSubscribersForTest,
    createRunningElapsed,
  } from './useRunningElapsed.svelte';

  let {
    firstRunning = true,
    secondRunning = false,
    createdAt = 1,
  }: {
    firstRunning?: boolean;
    secondRunning?: boolean;
    createdAt?: number;
  } = $props();

  const first = createRunningElapsed(
    () => firstRunning,
    () => createdAt,
  );
  const second = createRunningElapsed(
    () => secondRunning,
    () => createdAt,
  );
</script>

<div data-testid="first-label">{first.label}</div>
<div data-testid="second-label">{second.label}</div>
<div data-testid="subscriber-count">{__runningElapsedTickerSubscribersForTest()}</div>
