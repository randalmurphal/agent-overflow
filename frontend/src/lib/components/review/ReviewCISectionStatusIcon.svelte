<script lang="ts">
  import Circle from '@lucide/svelte/icons/circle';
  import CircleCheck from '@lucide/svelte/icons/circle-check';
  import CircleMinus from '@lucide/svelte/icons/circle-minus';
  import CircleSlash from '@lucide/svelte/icons/circle-slash';
  import CircleX from '@lucide/svelte/icons/circle-x';
  import Dot from '@lucide/svelte/icons/dot';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';

  // A CI log section's status, as ReviewCILogView's section rows show it.
  let { status }: { status: string } = $props();

  const animate = $derived(!getSettings().lowPowerMode);
</script>

{#if status === 'success'}
  <Icon icon={CircleCheck} size={13} class="shrink-0 text-success" />
{:else if status === 'failed'}
  <Icon icon={CircleX} size={13} class="shrink-0 text-error" />
{:else if status === 'running'}
  <SteppedSpinner size={13} class="text-accent" {animate} />
{:else if status === 'pending'}
  <Icon icon={Circle} size={13} class="shrink-0 text-fg-subtle" />
{:else if status === 'canceled'}
  <Icon icon={CircleSlash} size={13} class="shrink-0 text-fg-muted" />
{:else if status === 'done'}
  <Icon icon={CircleCheck} size={13} class="shrink-0 text-fg-subtle" />
{:else if status === 'output'}
  <Icon icon={Dot} size={13} class="shrink-0 text-fg-subtle" />
{:else}
  <Icon icon={CircleMinus} size={13} class="shrink-0 text-fg-muted" />
{/if}
