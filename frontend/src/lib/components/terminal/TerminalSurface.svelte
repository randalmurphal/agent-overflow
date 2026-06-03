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
  import TerminalTabStrip from './TerminalTabStrip.svelte';
  import TerminalBody from './TerminalBody.svelte';
  import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';

  interface Props {
    surface: ThreadTerminalSurfaceContext;
    /** Injected by tests to skip auto-ListTerminals/OpenTerminal on mount. */
    manual?: boolean;
    /** Whether to offer the ▾ collapse affordance. True in the bottom drawer;
     *  false in a full terminal pane, where there is nothing to collapse into. */
    collapsible?: boolean;
  }

  let { surface, manual = false, collapsible = true }: Props = $props();

  let bodyEl: { focus: () => void } | undefined = $state(undefined);
  // Latch a "focus terminal once it exists" intent. The creator of the
  // surface (the ⌘` chord / header button for the drawer; the new-pane
  // create helper for a terminal pane) calls pane.requestTerminalFocus()
  // before the surface mounts; we read-and-clear that intent in onMount
  // below. On a cold first open, onMount's OpenTerminal (async, ~50–200 ms)
  // hasn't resolved, so bodyEl is undefined when we consume — the effect
  // below holds the intent and focuses the moment TerminalBody binds.
  let pendingFocus = $state(false);

  $effect(() => {
    if (!pendingFocus || !bodyEl) return;
    const el = bodyEl;
    pendingFocus = false;
    // Defer one frame past the mount flush so the terminal claims focus after
    // any focus changes that ran during this commit (e.g. the composer's own
    // mount/autofocus), rather than being immediately overridden by them.
    requestAnimationFrame(() => el.focus());
  });

  // Consume the pane's focus intent reactively. The read-and-clear runs on
  // mount (cold drawer open: intent set before mount) AND re-runs whenever the
  // intent flips on a live surface (warm nav-into via pane.focusLeft/Right),
  // because consumeFocusRequest reads the pane's `pendingTerminalFocus` $state.
  // It returns true at most once per requestTerminalFocus(), so a remount can't
  // re-grab focus; the rAF effect above lands it once TerminalBody binds.
  $effect(() => {
    if (surface.consumeFocusRequest()) pendingFocus = true;
  });

  // Read the key once at init (non-reactive on purpose — the parent keys this
  // component on the thread, so a thread swap remounts rather than mutating the
  // handle in place). The helper keeps that intent explicit and reads `surface`
  // inside a function so it isn't flagged as a stale top-level capture.
  function initialTerminalStateKey(): string {
    return surface.threadId ?? surface.paneId;
  }

  // Thread-owned state lets this renderer unmount/remount without losing tabs.
  // getThreadTerminalState is memoized per key, so the bottom drawer's thin
  // wrapper and this surface share one handle (the wrapper reads drawerHeight
  // from it to size the Drawer primitive).
  const handle: ThreadTerminalStateHandle = getThreadTerminalState(initialTerminalStateKey());

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
    // Closing the last tab leaves nothing to render. In the drawer, collapse
    // it (the user re-opens via the header button or ⌘J). In a full pane,
    // setVisible is a no-op and the "No active terminal" empty state stays put
    // with ＋ to re-open.
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
    // Auto-open a first terminal if none exist yet so the surface is not empty.
    if (handle.tabs.length === 0) {
      await openTerminal();
    }
  });
</script>

<TerminalTabStrip
  {handle}
  onOpen={openTerminal}
  onClose={closeTerminal}
  onSelect={selectTerminal}
  onCollapse={collapsible ? collapseDrawer : undefined}
  workspacePath={surface.workspacePath}
/>

{#if handle.activeTerminalID}
  {#key handle.activeTerminalID}
    <TerminalBody
      bind:this={bodyEl}
      {handle}
      terminalID={handle.activeTerminalID}
      paneId={surface.paneId}
    />
  {/key}
{:else}
  <div class="flex-1 min-h-0 flex items-center justify-center text-fg-muted text-sm">
    No active terminal.
  </div>
{/if}
