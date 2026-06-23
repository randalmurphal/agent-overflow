<script lang="ts">
  // Renderable stand-in for ThreadTerminalDrawer, used by the
  // LazyThreadTerminalDrawer cold-settle test. The real drawer mounts xterm,
  // which would explode the unit surface; here we only need a component that
  // mounts cleanly so the lazy wrapper's post-import `.then` — which fires
  // `surface.settleAfterAsyncMount()` — actually runs.
  import type { ThreadTerminalDrawerProps } from '../../lib/components/terminal/terminalDrawerTypes';

  let { surface, manual = false }: ThreadTerminalDrawerProps = $props();
</script>

<div
  data-testid="stub-terminal-drawer"
  data-pane-id={surface.paneId}
  data-manual={manual}
></div>
