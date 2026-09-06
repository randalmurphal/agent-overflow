<script lang="ts">
  import NetworkSection from './NetworkSection.svelte';
  import DevicesSection from './DevicesSection.svelte';
  import WSLSection from './WSLSection.svelte';
</script>

<div class="flex flex-col gap-5">
  <div class="rounded-xl border border-accent/25 bg-accent/5 p-4">
    <DevicesSection />
  </div>
  <NetworkSection />
  <WSLSection />
</div>
