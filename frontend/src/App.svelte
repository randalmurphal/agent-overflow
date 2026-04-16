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

<main class="app-shell relative h-screen w-screen overflow-hidden text-text-primary">
  <div class="pointer-events-none absolute inset-0 opacity-70">
    <div class="absolute left-[-12rem] top-[-10rem] h-[28rem] w-[28rem] rounded-full bg-accent/12 blur-3xl"></div>
    <div class="absolute bottom-[-14rem] right-[-10rem] h-[24rem] w-[24rem] rounded-full bg-provider-codex/10 blur-3xl"></div>
  </div>
  <div class="relative flex h-full w-full">
    <Sidebar {pane} onOpenSettings={() => showSettings = true} />
    <div class="flex-1 flex flex-col min-w-0">
      {#if showSettings}
        <SettingsView onClose={() => showSettings = false} />
      {:else}
        <ChatView {pane} />
      {/if}
    </div>
  </div>
</main>
<Toast />
