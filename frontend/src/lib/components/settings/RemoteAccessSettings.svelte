<script lang="ts">
  // Settings → Remote access: opening this backend to the network, the WSL
  // distro the launcher boots, and the saved `--connect` targets.

  import NetworkSection from './NetworkSection.svelte';
  import WSLSection from './WSLSection.svelte';
  import RemoteEndpointsSection from './RemoteEndpointsSection.svelte';
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
  <WSLSection />
  <RemoteEndpointsSection />
</div>
