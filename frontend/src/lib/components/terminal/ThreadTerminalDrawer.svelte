<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { wailsEventOn } from '../../stores/events';
  import {
    OpenTerminal,
    CloseTerminal,
    ListTerminals,
    TerminalOpenOptions,
  } from '../../stores/bindings';
  import type {
    TerminalExitEventPayload,
    TerminalOutputEventPayload,
    TerminalHandle,
    TerminalSessionSummary,
  } from '../../types/terminal';
  import { decodeTerminalOutput } from '../../types/terminal';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import {
    createThreadTerminalState,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import TerminalTabStrip from './TerminalTabStrip.svelte';
  import TerminalBody from './TerminalBody.svelte';

  interface SendToComposerChip {
    id: string;
    label: string;
    preview: string;
    content: string;
    createdAt: number;
  }

  interface Props {
    pane: ThreadPane;
    /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
    manual?: boolean;
    /** Called when the user captures selected terminal text as a chip. */
    onSendToComposer?: (chip: SendToComposerChip) => void;
  }

  let { pane, manual = false, onSendToComposer }: Props = $props();

  // One state container per drawer instance. The drawer is keyed on the
  // thread ID in the parent, so switching threads remounts the drawer.
  const handle: ThreadTerminalStateHandle = createThreadTerminalState();

  let dragging = false;
  let dragStartY = 0;
  let dragStartHeight = 0;

  async function openTerminal() {
    if (!pane.thread) return;
    try {
      const th = (await OpenTerminal(
        pane.thread.id,
        new TerminalOpenOptions({ cwd: pane.thread.workspacePath }),
      )) as TerminalHandle;
      if (th?.summary) {
        handle.addTab(th.summary);
      }
    } catch (err) {
      console.error('terminal: OpenTerminal failed', err);
      addToast('error', `Could not open terminal: ${err}`);
    }
  }

  async function closeTerminal(terminalID: string) {
    try {
      await CloseTerminal(terminalID);
    } catch (err) {
      console.error('terminal: CloseTerminal failed', err);
    }
    handle.removeTab(terminalID);
  }

  function selectTerminal(terminalID: string) {
    handle.setActive(terminalID);
  }

  function collapseDrawer() {
    pane.setShowTerminal(false);
  }

  // Output and exit events are routed directly here: only the active pane's
  // drawer is mounted, so a single subscription per drawer is sufficient.
  let cancelOutput: (() => void) | null = null;
  let cancelExit: (() => void) | null = null;

  onMount(async () => {
    cancelOutput = wailsEventOn<TerminalOutputEventPayload>('terminal:output', (payload) => {
      if (pane.thread && payload.threadID !== pane.thread.id) return;
      const decoded = decodeTerminalOutput(payload.data);
      handle.appendOutput(payload.terminalID, decoded);
    });
    cancelExit = wailsEventOn<TerminalExitEventPayload>('terminal:exit', (payload) => {
      if (pane.thread && payload.threadID !== pane.thread.id) return;
      handle.markExit(payload.terminalID, payload.code, payload.reason);
    });

    if (manual || !pane.thread) return;

    try {
      const list = (await ListTerminals(pane.thread.id)) as TerminalSessionSummary[] | null;
      if (list) {
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

  onDestroy(() => {
    cancelOutput?.();
    cancelExit?.();
  });

  function startDrag(e: PointerEvent) {
    dragging = true;
    dragStartY = e.clientY;
    dragStartHeight = handle.drawerHeight;
    (e.target as HTMLElement).setPointerCapture(e.pointerId);
  }

  function onDrag(e: PointerEvent) {
    if (!dragging) return;
    const delta = dragStartY - e.clientY;
    handle.setDrawerHeight(dragStartHeight + delta);
  }

  function endDrag(e: PointerEvent) {
    if (!dragging) return;
    dragging = false;
    (e.target as HTMLElement).releasePointerCapture(e.pointerId);
  }
</script>

<div
  class="flex flex-col border-t border-border bg-surface-0 shrink-0"
  style={`height: ${handle.drawerHeight}px`}
  data-testid="terminal-drawer"
>
  <div
    class="h-1.5 cursor-row-resize bg-surface-1 hover:bg-accent/30"
    data-testid="terminal-drawer-resize"
    role="separator"
    aria-orientation="horizontal"
    onpointerdown={startDrag}
    onpointermove={onDrag}
    onpointerup={endDrag}
    onpointercancel={endDrag}
  ></div>

  <TerminalTabStrip
    {handle}
    onOpen={openTerminal}
    onClose={closeTerminal}
    onSelect={selectTerminal}
    onCollapse={collapseDrawer}
  />

  {#if handle.activeTerminalID}
    {#key handle.activeTerminalID}
      <TerminalBody
        {handle}
        terminalID={handle.activeTerminalID}
        onSendToComposer={onSendToComposer}
      />
    {/key}
  {:else}
    <div class="flex-1 min-h-0 flex items-center justify-center text-text-secondary text-sm">
      No active terminal.
    </div>
  {/if}
</div>
