<script lang="ts">
  import { onMount } from 'svelte';
  import { getMainPane } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
  import { refreshThreads } from './lib/stores/threads.svelte';
  import { loadSettings, getSettings } from './lib/stores/settings.svelte';
  import { applyTheme } from './lib/utils/theme';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import ChatView from './lib/components/chat/ChatView.svelte';
  import Toast from './lib/components/shared/Toast.svelte';
  import SettingsView from './lib/components/settings/SettingsView.svelte';

  let showSettings = $state(false);

  const pane = getMainPane();

  $effect(() => {
    applyTheme(getSettings().theme);
  });

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    refreshThreads();
    loadSettings();

    return () => {
      cleanupEvents();
    };
  });
</script>

<main class="h-screen w-screen bg-surface-0 text-text-primary flex overflow-hidden">
  <Sidebar {pane} onOpenSettings={() => showSettings = true} />
  <div class="flex-1 flex flex-col min-w-0">
    {#if showSettings}
      <SettingsView onClose={() => showSettings = false} />
    {:else}
      <ChatView {pane} />
    {/if}
  </div>
</main>
<Toast />
