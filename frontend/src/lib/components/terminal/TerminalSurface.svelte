<script lang="ts">
  import { onMount } from 'svelte';
  import {
    OpenTerminal,
    CloseTerminal,
    ListTerminals,
    RefreshTerminal,
    TerminalOpenOptions,
  } from '../../stores/bindings';
  import type {
    TerminalHandle,
    TerminalSessionSummary,
  } from '../../types/terminal';
  import { addToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import {
    getThreadTerminalState,
    terminalStateKeyForPane,
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
  // Latch a "focus the terminal body once it exists" intent, drained (and
  // cleared) by the rAF effect below. Every create/switch/close changes
  // activeTerminalID, and TerminalBody is keyed on it, so each remounts the body
  // and drops xterm focus — re-latching keeps the cursor on the active tab
  // instead of stranding the user on the tab strip. Two entry points feed this
  // one latch, split by whether the caller can reach this component's $state:
  //   • Out-of-component callers go through pane.requestTerminalFocus(), which
  //     the consume effect below drains: the surface creator on a cold open
  //     (⌘` chord / drawer header button / new-pane helper — set before mount,
  //     before OpenTerminal (async, ~50–200 ms) resolves, so bodyEl is undefined
  //     when we consume and the latch holds until TerminalBody binds), and the
  //     keyboard tab commands (terminal.newTab / closeTab / nextTab / prevTab),
  //     which run in builtinCommands and have no handle to this component.
  //   • In-component mouse handlers (＋ click, tab click, close-to-sibling) set
  //     it directly, since they already hold this $state.
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
    return terminalStateKeyForPane(surface.threadId, surface.paneId);
  }

  // Thread-owned state lets this renderer unmount/remount without losing tabs.
  // getThreadTerminalState is memoized per key, so the bottom drawer's thin
  // wrapper and this surface share one handle (the wrapper reads drawerHeight
  // from it to size the Drawer primitive).
  const handle: ThreadTerminalStateHandle = getThreadTerminalState(initialTerminalStateKey());
  let mounted = true;

  function canUseTerminalResult(threadId: string, workspacePath: string | undefined): boolean {
    if (!mounted) return false;
    if (surface.threadId !== threadId) return false;
    return surface.canAdoptOpenedTerminal?.(threadId, workspacePath) ?? true;
  }

  async function closeStaleOpenedTerminal(terminalID: string): Promise<void> {
    try {
      await CloseTerminal(terminalID);
    } catch (err) {
      console.error('terminal: CloseTerminal for stale open failed', err);
    }
  }

  async function openTerminal(opts: { focus?: boolean } = {}) {
    const threadId = surface.threadId;
    if (!threadId) return;
    const workspacePath = surface.workspacePath;
    try {
      const th = (await OpenTerminal(
        threadId,
        new TerminalOpenOptions({ cwd: workspacePath }),
      )) as TerminalHandle;
      if (!canUseTerminalResult(threadId, workspacePath)) {
        if (th?.terminalID) {
          await closeStaleOpenedTerminal(th.terminalID);
        }
        return;
      }
      if (th?.summary) {
        handle.addTab(th.summary);
        // The ＋ button passes focus:true so the freshly opened tab grabs the
        // cursor. The mount auto-open leaves it unset — its focus is driven by
        // the pane's consume-once intent (see pendingFocus above), not here.
        if (opts.focus) pendingFocus = true;
      }
    } catch (err) {
      console.error('terminal: OpenTerminal failed', err);
      addToast('error', `Could not open terminal: ${userFacingError(err)}`);
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
    // with ＋ to re-open. When a sibling tab remains, removeTab promotes it to
    // active (a remount); re-latch focus so the cursor follows into it.
    if (handle.tabs.length === 0) {
      collapseDrawer();
    } else {
      pendingFocus = true;
    }
  }

  function selectTerminal(terminalID: string) {
    handle.setActive(terminalID);
    // The remount on the active-tab change drops xterm focus; re-latch it so
    // clicking a tab puts the cursor in that terminal (parity with Ctrl+Tab).
    pendingFocus = true;
  }

  // Repaint the active terminal. RefreshTerminal blips the PTY winsize (rows
  // shrink + restore) to deliver a SIGWINCH so the provider's TUI redraws a
  // corrupted frame — the same recovery a manual window resize triggers, minus
  // the visible resize. The xterm grid is untouched, so the user sees only the
  // provider's own reconciliation. No-op when there is no active terminal.
  function refreshActiveTerminal() {
    const terminalID = handle.activeTerminalID;
    if (!terminalID) return;
    RefreshTerminal(terminalID).catch((err) => {
      console.error('terminal: RefreshTerminal failed', err);
    });
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

  onMount(() => {
    mounted = true;
    return () => {
      mounted = false;
    };
  });

  onMount(async () => {
    const threadId = surface.threadId;
    const workspacePath = surface.workspacePath;
    if (manual || !threadId) return;

    try {
      const list = (await ListTerminals(threadId)) as TerminalSessionSummary[] | null;
      if (!canUseTerminalResult(threadId, workspacePath)) return;
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
  onOpen={() => void openTerminal({ focus: true })}
  onClose={closeTerminal}
  onSelect={selectTerminal}
  onRefresh={refreshActiveTerminal}
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
