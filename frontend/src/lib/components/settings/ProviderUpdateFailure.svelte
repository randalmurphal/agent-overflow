<script lang="ts">
  // Why a provider's update did not happen, shown beside the control that
  // offers it: on the provider page and in the notes modal.
  import SettingsCallout from './SettingsCallout.svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { providerUpdateFailure } from '../../stores/providerUpdates.svelte';

  let {
    provider,
    backend,
    testId,
    class: className = '',
  }: {
    provider: string;
    backend: BackendKey;
    testId: string;
    class?: string;
  } = $props();

  let failure = $derived(providerUpdateFailure(provider, backend));
</script>

{#if failure}
  <div class={className} data-testid={testId}>
    <SettingsCallout tone="error">
      <p>{failure.error}</p>
      {#if failure.output}
        <pre class="mt-1.5 max-h-40 overflow-auto whitespace-pre-wrap break-all font-mono text-[0.6875rem] text-fg-muted">{failure.output}</pre>
      {/if}
    </SettingsCallout>
  </div>
{/if}
