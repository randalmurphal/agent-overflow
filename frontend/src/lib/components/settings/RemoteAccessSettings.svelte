<script lang="ts">
  import NetworkSection from './NetworkSection.svelte';
  import DevicesSection from './DevicesSection.svelte';
  import WSLSection from './WSLSection.svelte';
  import DeviceNameField from './DeviceNameField.svelte';
  import { settingsComputer } from './settingsComputer';
  const { backend } = settingsComputer();
</script>

<div class="flex flex-col gap-5">
  <DeviceNameField {backend} fieldId="remote.device-name" />
  <div class="rounded-xl border border-accent/25 bg-accent/5 p-4">
    <DevicesSection />
  </div>
  <NetworkSection />
  <WSLSection />
</div>
