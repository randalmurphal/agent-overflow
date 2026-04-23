<script module lang="ts">
  import type { Component } from 'svelte';
  import type { ThreadTerminalDrawerProps } from './terminalDrawerTypes';

  type ThreadTerminalDrawerModule = typeof import('./ThreadTerminalDrawer.svelte');

  let cachedDrawer: Component<ThreadTerminalDrawerProps> | null = null;
  let pendingDrawerLoad: Promise<Component<ThreadTerminalDrawerProps>> | null = null;

  function loadThreadTerminalDrawer(): Promise<Component<ThreadTerminalDrawerProps>> {
    if (cachedDrawer) return Promise.resolve(cachedDrawer);
    pendingDrawerLoad ??= import('./ThreadTerminalDrawer.svelte')
      .then((mod: ThreadTerminalDrawerModule) => {
        cachedDrawer = mod.default;
        return cachedDrawer;
      })
      .catch((err) => {
        pendingDrawerLoad = null;
        throw err;
      });
    return pendingDrawerLoad;
  }
</script>

<script lang="ts">
  import { onMount } from 'svelte';

  let { pane, manual = false, onSendToComposer }: ThreadTerminalDrawerProps = $props();

  let Drawer = $state<Component<ThreadTerminalDrawerProps> | null>(cachedDrawer);
  let loadError = $state<string | null>(null);

  onMount(() => {
    if (Drawer) return;
    let cancelled = false;

    loadThreadTerminalDrawer()
      .then((component) => {
        if (!cancelled) Drawer = component;
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

{#if Drawer}
  <Drawer {pane} {manual} {onSendToComposer} />
{:else if loadError}
  <div class="border-t border-border bg-panel px-3 py-2 text-[12px] text-error" data-testid="terminal-drawer-load-error">
    Failed to load terminal drawer: {loadError}
  </div>
{:else}
  <div class="border-t border-border bg-panel px-3 py-2 text-[12px] text-fg-muted" data-testid="terminal-drawer-loading">
    Loading terminal...
  </div>
{/if}
