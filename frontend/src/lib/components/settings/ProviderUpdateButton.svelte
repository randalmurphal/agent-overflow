<script lang="ts">
  // The one "Update to X" control, for the provider page and the notes
  // modal. It renders while an update is on offer or a run is on. A run
  // shows as busy whatever the last check answered: a check that fails
  // mid-run clears the offered version, not the run.
  import type { ComponentProps } from 'svelte';
  import Button from '../primitives/Button.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { hasScope } from '../../transport/scopes';
  import {
    hasProviderUpdate,
    providerUpdate,
    providerUpdateRunning,
    runProviderUpdate,
  } from '../../stores/providerUpdates.svelte';

  let {
    provider,
    backend,
    variant,
    size,
    testId,
  }: {
    provider: string;
    backend: BackendKey;
    variant: ComponentProps<typeof Button>['variant'];
    size: ComponentProps<typeof Button>['size'];
    testId: string;
  } = $props();

  let updating = $derived(hasScope('access:admin', backend) && providerUpdateRunning(provider, backend));
  let offered = $derived(
    hasProviderUpdate(provider, backend) ? (providerUpdate(provider, backend)?.latestVersion ?? '') : '',
  );
</script>

{#if updating || offered}
  <Button
    {variant}
    {size}
    class="shrink-0"
    loading={updating}
    {testId}
    onclick={() => void runProviderUpdate(provider, backend)}
  >
    {#snippet children()}{updating ? 'Updating…' : `Update to ${offered}`}{/snippet}
  </Button>
{/if}
