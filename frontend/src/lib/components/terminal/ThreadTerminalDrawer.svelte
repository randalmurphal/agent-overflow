<script lang="ts">
  import {
    getThreadTerminalState,
    terminalStateKeyForPane,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import Drawer from '../primitives/Drawer.svelte';
  import TerminalSurface from './TerminalSurface.svelte';
  import type { ThreadTerminalDrawerProps } from './terminalDrawerTypes';

  let { surface, manual = false }: ThreadTerminalDrawerProps = $props();

  // Same key TerminalSurface derives, read once at init (the parent keys this
  // on the thread). The helper reads `surface` inside a function so it isn't
  // flagged as a stale top-level capture.
  function initialTerminalStateKey(): string {
    return terminalStateKeyForPane(surface.threadId, surface.paneId);
  }

  // getThreadTerminalState is memoized per key — so this wrapper and the surface
  // share one handle. The drawer needs it only for the persisted height that
  // sizes the Drawer primitive; tabs / active terminal / open-close all live in
  // the surface.
  const handle: ThreadTerminalStateHandle = getThreadTerminalState(initialTerminalStateKey());

  // Drawer primitive owns the pointer-capture resize math; we just push new
  // heights into the persisted handle so remounts read back the user's size.
  function handleResize(size: number): void {
    handle.setDrawerHeight(size);
  }
</script>

<div data-testid="terminal-drawer">
  <Drawer
    position="bottom"
    size={handle.drawerHeight}
    minSize={120}
    resizable={true}
    onResize={handleResize}
    acquireResizeLease={surface.acquireResizeLease}
  >
    {#snippet children()}
      <TerminalSurface {surface} {manual} />
    {/snippet}
  </Drawer>
</div>
