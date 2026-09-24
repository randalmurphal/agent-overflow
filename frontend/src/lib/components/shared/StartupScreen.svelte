<script lang="ts">
  // The pane area before the startup layout restores. Blank on an ordinary
  // boot, which restores within a moment. While a computer reports that it
  // is starting, it shows that computer's boot phase, the same sentence and
  // step line the Windows launcher's loading page shows. This is the one
  // place the elapsed clock ticks: the sidebar's row for the same computer
  // names the phase and step only.
  import { startupMetaText, startupStatusText } from '../../transport/startupProgress';
  import { backendDisplayName, getAttachedBackends, hasMultipleBackends } from '../../stores/attachedBackends.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';

  let starting = $derived.by(() => {
    for (const entry of getAttachedBackends()) {
      const status = getTransportStatusFor(entry.id);
      if (status.status === 'starting' && status.startup) return { entry, startup: status.startup };
    }
    return null;
  });
  let title = $derived.by(() => {
    if (starting === null) return '';
    const verb = starting.startup.updatingTo ? 'Updating' : 'Starting';
    return hasMultipleBackends() ? `${verb} ${backendDisplayName(starting.entry)}` : `${verb} Agent Overflow`;
  });
</script>

<section
  class="flex h-full min-w-full flex-1 items-center justify-center bg-transparent px-8"
  data-testid="startup-screen"
>
  {#if starting !== null}
    <div role="status" aria-live="polite" class="flex max-w-md flex-col items-center gap-2 text-center" data-testid="startup-screen-status">
      <SteppedSpinner size={20} class="mb-1" animate={!getSettings().lowPowerMode} />
      <p class="text-base font-medium text-text-primary">{title}</p>
      <p class="text-sm text-fg-muted [overflow-wrap:anywhere]" data-testid="startup-screen-phase">{startupStatusText(starting.startup)}</p>
      {#if startupMetaText(starting.startup)}
        <p class="text-xs text-fg-muted opacity-80 tabular-nums" data-testid="startup-screen-meta">{startupMetaText(starting.startup)}</p>
      {/if}
    </div>
  {/if}
</section>
