<script lang="ts">
  // Settings → Updates, the supervised machines: one card per attached
  // backend that reports a supervisor (docs/architecture/serve-mode.md
  // § Updating over the wire). Renders nothing on a desktop with no serve
  // host attached, which is the common case, so the section above it reads
  // exactly as before.
  //
  // The list is `supervisedMachines()`: a machine joins when its status
  // read says `supervised`, which the store issues on every hello it holds
  // `access:admin` for. Nothing here fires an RPC on mount.
  import MachineUpdateCard from './MachineUpdateCard.svelte';
  import { supervisedMachines } from '../../stores/serviceUpdate.svelte';

  const machines = $derived(supervisedMachines());
</script>

{#if machines.length > 0}
  <section class="flex flex-col gap-3" data-testid="machine-updates">
    <div class="flex flex-col gap-0.5">
      <span class="text-[0.8125rem] font-semibold text-fg">Machines</span>
      <p class="text-[0.71875rem] text-fg-muted">
        A machine running as a service installs the version you pick and restarts into it. One
        that fails to come back up is rolled back.
      </p>
    </div>
    {#each machines as key (key)}
      <MachineUpdateCard {key} />
    {/each}
  </section>
{/if}
