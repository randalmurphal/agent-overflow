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

  let { surface, manual = false }: ThreadTerminalDrawerProps = $props();

  let Drawer = $state<Component<ThreadTerminalDrawerProps> | null>(cachedDrawer);
  let loadError = $state<string | null>(null);

  onMount(() => {
    if (Drawer) return;
    let cancelled = false;

    loadThreadTerminalDrawer()
      .then((component) => {
        if (cancelled) return;
        Drawer = component;
        // Cold-open re-pin (see terminalDrawerTypes.settleAfterAsyncMount): the
        // real drawer is an in-flow shrink-0 box (120–320px) that steals height
        // from the flex-1 timeline, and it commits HERE — after setShowTerminal's
        // 2-rAF open lease has already released (this branch only runs because the
        // dynamic import resolved late) and with no scrollEl ResizeObserver
        // watching. Without this, a stuck-to-bottom timeline won't re-pin and the
        // latest messages hide behind the terminal. The warm path renders the
        // drawer in-flush under the open lease and never reaches here (onMount
        // early-returns when Drawer is already cached).
        surface.settleAfterAsyncMount?.();
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
  <Drawer {surface} {manual} />
{:else if loadError}
  <div class="border-t border-border bg-panel px-3 py-2 text-[0.75rem] text-error" data-testid="terminal-drawer-load-error">
    Failed to load terminal drawer: {loadError}
  </div>
{:else}
  <div class="border-t border-border bg-panel px-3 py-2 text-[0.75rem] text-fg-muted" data-testid="terminal-drawer-loading">
    Loading terminal...
  </div>
{/if}
