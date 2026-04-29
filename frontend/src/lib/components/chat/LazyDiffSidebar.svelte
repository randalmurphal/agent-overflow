<script module lang="ts">
  import type { Component } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';

  interface DiffSidebarProps {
    pane: ThreadPane;
  }

  type DiffSidebarModule = typeof import('./DiffSidebar.svelte');

  // Module-scoped cache: once the chunk has loaded for any sidebar
  // mount in this session it stays cached. Subsequent opens are
  // synchronous (no Suspense fallback).
  let cachedSidebar: Component<DiffSidebarProps> | null = null;
  let pendingLoad: Promise<Component<DiffSidebarProps>> | null = null;

  function loadDiffSidebar(): Promise<Component<DiffSidebarProps>> {
    if (cachedSidebar) return Promise.resolve(cachedSidebar);
    pendingLoad ??= import('./DiffSidebar.svelte')
      .then((mod: DiffSidebarModule) => {
        cachedSidebar = mod.default;
        return cachedSidebar;
      })
      .catch((err) => {
        pendingLoad = null;
        throw err;
      });
    return pendingLoad;
  }
</script>

<script lang="ts">
  import { onMount } from 'svelte';

  let { pane }: DiffSidebarProps = $props();

  let Sidebar = $state<Component<DiffSidebarProps> | null>(cachedSidebar);
  let loadError = $state<string | null>(null);

  onMount(() => {
    if (Sidebar) return;
    let cancelled = false;

    loadDiffSidebar()
      .then((component) => {
        if (!cancelled) Sidebar = component;
      })
      .catch((err) => {
        if (!cancelled) {
          loadError = err instanceof Error ? err.message : String(err);
        }
      });

    return () => {
      cancelled = true;
    };
  });
</script>

{#if Sidebar}
  <Sidebar {pane} />
{:else if loadError}
  <div
    class="flex min-h-0 flex-1 flex-col px-3 py-3 text-xs text-error"
    data-testid="diff-sidebar-load-error"
  >
    Failed to load diff sidebar: {loadError}
  </div>
{:else}
  <div
    class="flex min-h-0 flex-1 flex-col px-3 py-3 text-xs text-fg-muted"
    data-testid="diff-sidebar-loading-shell"
  >
    Loading diff sidebar…
  </div>
{/if}
