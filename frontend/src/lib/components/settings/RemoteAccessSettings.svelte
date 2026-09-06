<script lang="ts">
  // Settings → Access & sharing: network access and devices paired to the
  // selected computer. This frontend's connections live on Computers.

  import NetworkSection from './NetworkSection.svelte';
  import DevicesSection from './DevicesSection.svelte';
  import WSLSection from './WSLSection.svelte';
  import AgentComputersSection from './AgentComputersSection.svelte';
</script>

<!--
  WSLSection self-hides on non-WSL hosts and in --connect mode, so the
  "extra" composition here is a no-op on macOS and native Linux. Mounting
  it on this page (rather than its own page) keeps the nav rail stable
  across platforms — a page that's empty on most hosts would clutter the
  UX without buying anything.
-->
<div class="settings-sections">
  <NetworkSection />
  <DevicesSection />
  <AgentComputersSection />
  <WSLSection />
</div>
