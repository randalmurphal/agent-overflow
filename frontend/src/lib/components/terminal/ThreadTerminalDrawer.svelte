<script lang="ts">
  import { onMount } from 'svelte';
  import {
    OpenTerminal,
    CloseTerminal,
    ListTerminals,
    TerminalOpenOptions,
  } from '../../stores/bindings';
  import type {
    TerminalHandle,
    TerminalSessionSummary,
  } from '../../types/terminal';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import {
    getThreadTerminalState,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import Drawer from '../primitives/Drawer.svelte';
  import TerminalTabStrip from './TerminalTabStrip.svelte';
  import TerminalBody from './TerminalBody.svelte';
  import type { ThreadTerminalDrawerProps } from './terminalDrawerTypes';

  let { surface, manual = false }: ThreadTerminalDrawerProps = $props();

  let bodyEl: { focus: () => void } | undefined = $state(undefined);

  // Listen for the smart-toggle's "focus terminal" intent so mod+`
  // can hand DOM focus to the active terminal even when the drawer
  // was already open.
  $effect(() => {
    if (typeof window === 'undefined') return;
    const handler = (event: Event): void => {
      const detail = (event as CustomEvent<{ paneId?: string }>).detail;
      if (!detail || detail.paneId !== surface.paneId) return;
      // Defer to next frame so the drawer's first paint completes
      // before we try to focus into it (covers the just-opened case).
      requestAnimationFrame(() => bodyEl?.focus());
    };
    window.addEventListener('agent-overflow:focus-terminal', handler);
    return () => window.removeEventListener('agent-overflow:focus-terminal', handler);
  });

  function initialTerminalStateKey(): string {
    return surface.threadId ?? surface.paneId;
  }

  // Thread-owned state lets this renderer unmount/remount without losing tabs
  // and keeps the model ready for a future app-level terminal dock.
  const handle: ThreadTerminalStateHandle = getThreadTerminalState(initialTerminalStateKey());

  // Drawer primitive owns the pointer-capture resize math; we just
  // push new heights into the persisted handle so remounts read back
  // the user's preferred size.
  function handleResize(size: number): void {
    handle.setDrawerHeight(size);
  }

  async function openTerminal() {
    const threadId = surface.threadId;
    if (!threadId) return;
    try {
      const th = (await OpenTerminal(
        threadId,
        new TerminalOpenOptions({ cwd: surface.workspacePath }),
      )) as TerminalHandle;
      if (th?.summary) {
        handle.addTab(th.summary);
      }
    } catch (err) {
      console.error('terminal: OpenTerminal failed', err);
      addToast('error', `Could not open terminal: ${errString(err)}`);
    }
  }

  async function closeTerminal(terminalID: string) {
    try {
      await CloseTerminal(terminalID);
    } catch (err) {
      console.error('terminal: CloseTerminal failed', err);
    }
    handle.removeTab(terminalID);
    // Closing the last tab leaves the drawer with nothing to render —
    // collapse it instead of showing the "No active terminal" empty
    // state. The user can re-open via the header button or ⌘J.
    if (handle.tabs.length === 0) {
      collapseDrawer();
    }
  }

  function selectTerminal(terminalID: string) {
    handle.setActive(terminalID);
  }

  function collapseDrawer() {
    surface.setVisible(false);
  }

  let previousTabCount = handle.tabs.length;

  $effect(() => {
    const count = handle.tabs.length;
    if (previousTabCount > 0 && count === 0) {
      collapseDrawer();
    }
    previousTabCount = count;
  });

  onMount(async () => {
    const threadId = surface.threadId;
    if (manual || !threadId) return;

    try {
      const list = (await ListTerminals(threadId)) as TerminalSessionSummary[] | null;
      if (list) {
        const listedIDs = new Set(list.map((s) => s.terminalID));
        for (const tab of handle.tabs) {
          if (!listedIDs.has(tab.terminalID)) {
            handle.removeTab(tab.terminalID);
          }
        }
        for (const s of list) {
          handle.addTab(s);
        }
      }
    } catch (err) {
      console.error('terminal: ListTerminals failed', err);
    }
    // Auto-open a first terminal if none exist yet so the drawer is not empty.
    if (handle.tabs.length === 0) {
      await openTerminal();
    }
  });
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
      <TerminalTabStrip
        {handle}
        onOpen={openTerminal}
        onClose={closeTerminal}
        onSelect={selectTerminal}
        onCollapse={collapseDrawer}
        workspacePath={surface.workspacePath}
      />

      {#if handle.activeTerminalID}
        {#key handle.activeTerminalID}
          <TerminalBody
            bind:this={bodyEl}
            {handle}
            terminalID={handle.activeTerminalID}
            onSendToComposer={surface.sendTerminalChip}
          />
        {/key}
      {:else}
        <div class="flex-1 min-h-0 flex items-center justify-center text-fg-muted text-sm">
          No active terminal.
        </div>
      {/if}
    {/snippet}
  </Drawer>
</div>
