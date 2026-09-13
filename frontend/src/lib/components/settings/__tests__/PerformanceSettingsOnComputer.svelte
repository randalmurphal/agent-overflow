<script lang="ts">
  // Mounts the Performance page the way SettingsView does for a chosen
  // computer: the backend key goes into context before the page reads it.
  import { provideSettingsComputer } from '../settingsComputer';
  import PerformanceSettings from '../PerformanceSettings.svelte';
  import type { BackendKey } from '../../../transport/backendKey';

  let { backend }: { backend: BackendKey } = $props();
  // Context is set once at init, the way SettingsView remounts a page per
  // computer; the prop is not expected to change under a mounted page.
  // svelte-ignore state_referenced_locally
  provideSettingsComputer(backend);
</script>

<PerformanceSettings />
