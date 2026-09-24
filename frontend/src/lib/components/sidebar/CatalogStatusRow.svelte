<script lang="ts">
  // One computer whose threads or projects have not loaded. Loading reads
  // the computer's connection: a starting backend names its boot phase and
  // step, an unreachable one says it is connecting. The elapsed clock is
  // not repeated here; the startup screen or the connection banner for the
  // same computer already ticks it. A failed load shows its error and a
  // Retry; the catalog store keeps retrying on its own meanwhile.
  import type { BackendKey } from '../../transport/backendKey';
  import { isTerminalConnectionStatus } from '../../transport/connectionRefusal';
  import { startupStatusText, startupStepText } from '../../transport/startupProgress';
  import { retryCatalogLoad } from '../../stores/catalogLoad.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import Button from '../primitives/Button.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';

  interface Props {
    backend: BackendKey;
    /** The computer's name when several are attached; '' otherwise. */
    name: string;
    /** The failed load's message, or null while loading. */
    error: string | null;
  }

  let { backend, name, error }: Props = $props();

  let status = $derived(getTransportStatusFor(backend));
  let startup = $derived(status.status === 'starting' ? status.startup : undefined);
  let label = $derived.by(() => {
    if (startup) return startupStatusText(startup);
    if (status.status === 'connected' || status.status === 'disconnected') return 'Loading projects…';
    if (isTerminalConnectionStatus(status.status)) return 'Not connected.';
    return 'Connecting…';
  });
  let meta = $derived(startup ? startupStepText(startup) : '');
  let prefix = $derived(name ? `${name}: ` : '');
</script>

{#if error !== null}
  <div
    role="alert"
    data-testid="sidebar-catalog-failed"
    data-backend={backend}
    class="flex min-w-0 items-start gap-2 px-2 py-2 text-xs text-error"
  >
    <p class="min-w-0 flex-1 [overflow-wrap:anywhere]">{prefix}{error}</p>
    <Button variant="danger-outline" size="xs" testId="sidebar-catalog-retry" onclick={() => retryCatalogLoad(backend)}>
      Retry
    </Button>
  </div>
{:else}
  <div
    role="status"
    aria-busy="true"
    data-testid="sidebar-catalog-loading"
    data-backend={backend}
    data-status={status.status}
    class="flex min-w-0 items-start gap-2 px-2 py-2 text-xs text-fg-muted"
  >
    <SteppedSpinner class="mt-0.5" animate={!getSettings().lowPowerMode} />
    <div class="min-w-0 flex-1">
      <p class="[overflow-wrap:anywhere]" data-testid="sidebar-catalog-loading-label">{prefix}{label}</p>
      {#if meta}
        <p class="mt-0.5 opacity-80 tabular-nums" data-testid="sidebar-catalog-loading-meta">{meta}</p>
      {/if}
    </div>
  </div>
{/if}
