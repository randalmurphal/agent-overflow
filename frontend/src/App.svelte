<script lang="ts">
  import { onMount } from 'svelte';
  import { getMainPane } from './lib/stores/panes.svelte';
  import { setupEventListeners } from './lib/stores/events';
  import { refreshThreads } from './lib/stores/threads.svelte';
  import Sidebar from './lib/components/sidebar/Sidebar.svelte';
  import ChatView from './lib/components/chat/ChatView.svelte';

  const pane = getMainPane();

  onMount(() => {
    const cleanupEvents = setupEventListeners();
    refreshThreads();

    return () => {
      cleanupEvents();
    };
  });
</script>

<main class="h-screen w-screen bg-surface-0 text-text-primary flex overflow-hidden">
  <Sidebar {pane} />
  <div class="flex-1 flex flex-col min-w-0">
    <ChatView {pane} />
  </div>
</main>
