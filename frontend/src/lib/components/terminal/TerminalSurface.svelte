<script lang="ts">
  import { threadMachine } from '../../stores/attachedBackends.svelte';
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
  import { addErrorToast } from '../../stores/toast.svelte';
  import { userFacingError } from '../../utils/userFacingError';
  import { errString } from '../../utils/errors';
  import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
  import { forgetTerminal } from '../../transport/entityIndex';
  import { endTerminalTab, startTerminal } from '../../stores/eventsTerminal';
  import { isDraftPlaceholderId } from '../../stores/draftPlaceholderId';
  import {
    getThreadTerminalState,
    registerTerminalSurface,
    TERMINAL_FOCUS_REGION,
    terminalFocusRegion,
    terminalStateKeyForPane,
    type ThreadTerminalStateHandle,
  } from './terminalStore.svelte';
  import TerminalTabStrip from './TerminalTabStrip.svelte';
  import TerminalBody from './TerminalBody.svelte';
  import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
  import { TERMINAL_FIND_OVERLAY_CLASS, terminalFindKey } from './terminalFind.svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { intendFocus, paneFocusScope, type FocusIntent } from '../../utils/focusIntent';
  import { paneFindSurface } from '../panes/paneFindRegion';
  import { hasPaneFindProvider, isPaneFindOpen, openPaneFind } from '../../stores/paneFind.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';

  interface Props {
    surface: ThreadTerminalSurfaceContext;
    /** Whether to offer the ▾ collapse affordance. True in the bottom drawer;
     *  false in a full terminal pane, where there is nothing to collapse into. */
    collapsible?: boolean;
  }

  let { surface, collapsible = true }: Props = $props();

  let bodyEl: { focus: () => void; focusTarget: () => HTMLElement | null } | undefined = $state(undefined);
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
  //     it directly, since they already hold this $state. Each takes its
  //     intent at the click, before any call it waits on, so a focus choice
  //     made during that call (a dialog) cancels it.
  let pendingFocus = $state.raw<FocusIntent | null>(null);

  // A focus this surface asks for itself: the latest focus intent, which a
  // later choice outside the pane, or a press outside its terminal, cancels.
  function intendTerminalFocus(): FocusIntent {
    return intendFocus(paneFocusScope(surface.paneId, TERMINAL_FOCUS_REGION));
  }

  $effect(() => {
    const intent = pendingFocus;
    if (!intent || !bodyEl) return;
    const el = bodyEl;
    pendingFocus = null;
    // Defer one frame past the mount flush so the terminal claims focus after
    // any focus changes that ran during this commit in its pane (e.g. the
    // composer's own mount/autofocus), rather than being immediately
    // overridden by them. A focus choice made since the intent (another
    // pane, a dialog) canceled it, and that focus stays.
    requestAnimationFrame(() => {
      intent.land(() => el.focus());
    });
  });

  // Consume the pane's focus intent reactively. The read-and-clear runs on
  // mount (cold drawer open: intent set before mount) AND re-runs whenever the
  // intent flips on a live surface (warm nav-into via pane.focusLeft/Right),
  // because consumeFocusRequest reads the pane's `pendingTerminalFocus` $state.
  // It returns the intent at most once per requestTerminalFocus(), so a remount can't
  // re-grab focus; the rAF effect above lands it once TerminalBody binds.
  $effect(() => {
    const requested = surface.consumeFocusRequest();
    if (requested) pendingFocus = requested;
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
      // Nobody is watching this surface any more; the diagnostic is the record.
      reportFrontendDiagnostic('terminal close of a stale open failed', errString(err));
    }
  }

  async function openTerminal(opts: { focus?: boolean } = {}) {
    const threadId = surface.threadId;
    if (!threadId) return;
    const workspacePath = surface.workspacePath;
    // The ＋ button passes focus:true so the freshly opened tab grabs the
    // cursor; its intent is the click's. The mount auto-open leaves it unset:
    // its focus is driven by the pane's consume-once intent (see pendingFocus
    // above), not here.
    const intent = opts.focus ? intendTerminalFocus() : null;
    let opened = false;
    try {
      let usable = false;
      const th = await startTerminal(
        async () => (await OpenTerminal(threadId, new TerminalOpenOptions({ cwd: workspacePath }))) as TerminalHandle,
        (answer) => {
          usable = canUseTerminalResult(threadId, workspacePath);
          if (usable && answer?.summary) handle.addTab(answer.summary);
        },
      );
      if (!usable) {
        // A thread's shell keeps running, as when its pane closes after the
        // open, and the thread's next surface lists it. A draft placeholder's
        // shells end with its surface (closeDraftPlaceholderTerminals), which
        // this late open missed.
        if (th?.terminalID && isDraftPlaceholderId(threadId)) {
          await closeStaleOpenedTerminal(th.terminalID);
        }
        return;
      }
      const terminalID = th?.summary?.terminalID;
      if (!terminalID) return;
      if (!handle.tabs.some((tab) => tab.terminalID === terminalID)) {
        // Its exit overtook the answer, so the tab never showed and the
        // collapse that follows a last tab's removal did not run.
        if (handle.tabs.length === 0) collapseDrawer();
        return;
      }
      opened = true;
      if (intent) pendingFocus = intent;
    } catch (err) {
      console.error('terminal: OpenTerminal failed', err);
      // A surface that is gone asked for nothing any more: its thread may have
      // been deleted while the open was in flight.
      if (!canUseTerminalResult(threadId, workspacePath)) return;
      addErrorToast(`Could not open terminal: ${userFacingError(err)}`, err);
    } finally {
      if (!opened) intent?.end();
    }
  }

  async function closeTerminal(terminalID: string) {
    // The cursor follows into the sibling a close leaves; the intent is the
    // click's, taken before the close is answered.
    const intent = intendTerminalFocus();
    const threadId = surface.threadId;
    const workspacePath = surface.workspacePath;
    try {
      await CloseTerminal(terminalID);
    } catch (err) {
      // The shell may still run: its tab stays, and the refusal is shown
      // while the surface that asked remains.
      intent.end();
      if (threadId && canUseTerminalResult(threadId, workspacePath)) {
        addErrorToast(`Could not close terminal: ${userFacingError(err)}`, err);
      }
      return;
    }
    handle.removeTab(terminalID);
    // Closing the last tab leaves nothing to render. In the drawer, collapse
    // it (the user re-opens via the header button or ⌘J). A terminal thread's
    // pane ends with its last terminal: its computer deletes the thread, and
    // that deletion closes the pane. When a sibling tab remains, removeTab
    // promotes it to active (a remount); re-latch focus so the cursor follows
    // into it.
    if (handle.tabs.length === 0) {
      intent.end();
      collapseDrawer();
    } else {
      pendingFocus = intent;
    }
  }

  function selectTerminal(terminalID: string) {
    handle.setActive(terminalID);
    // The remount on the active-tab change drops xterm focus; re-latch it so
    // clicking a tab puts the cursor in that terminal (parity with Ctrl+Tab).
    pendingFocus = intendTerminalFocus();
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
      addErrorToast(`Could not refresh terminal: ${userFacingError(err)}`, err);
    });
  }

  function collapseDrawer() {
    surface.setVisible(false);
  }

  // Each tab has its own find (terminalFind.svelte.ts). The pane's find
  // commands reach the active tab's through the surface declaration; the
  // tab strip's button is the keyboard-free entry, since mod+f inside the
  // xterm belongs to the shell. It shows once the active tab's find has
  // registered. A close from the bar returns focus to the terminal (the
  // bar's fallback focus).
  function activeFindKey(): string | null {
    const terminalID = handle.activeTerminalID;
    return terminalID ? terminalFindKey(surface.paneId, terminalID) : null;
  }

  function openFind() {
    const key = activeFindKey();
    if (key) openPaneFind(key);
  }

  // Under compact the open bar sits over the tab strip.
  const stripCovered = $derived.by(() => {
    const key = activeFindKey();
    return isCompactLayout() && key !== null && isPaneFindOpen(key);
  });

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

  // Make the tabs match the thread's terminal list, at mount and again when
  // the thread's computer reconnects or loses terminal events. A terminal the
  // list no longer names has ended, and goes the way its exit would have: its
  // tab is removed, focus follows to the next tab, and the drawer collapses
  // with the last one (the effect above). A failed read keeps the tabs.
  async function syncTerminals(
    threadId: string,
    workspacePath: string | undefined,
    activate: boolean,
  ): Promise<void> {
    try {
      await handle.syncTabs(
        async () => (await ListTerminals(threadId)) as TerminalSessionSummary[] | null,
        {
          activate,
          current: () => canUseTerminalResult(threadId, workspacePath),
          remove: (terminalID) => {
            forgetTerminal(terminalID, threadMachine(threadId, null));
            endTerminalTab(handle, threadId, terminalID);
          },
        },
      );
    } catch (err) {
      reportFrontendDiagnostic('terminal list failed', errString(err));
    }
  }

  onMount(() => {
    const threadId = surface.threadId;
    const workspacePath = surface.workspacePath;
    if (!threadId) return;
    // A re-read leaves the active tab alone, like a terminal another client opens.
    return registerTerminalSurface(threadId, () => syncTerminals(threadId, workspacePath, false));
  });

  onMount(async () => {
    const threadId = surface.threadId;
    const workspacePath = surface.workspacePath;
    if (!threadId) return;

    await syncTerminals(threadId, workspacePath, true);
    if (!canUseTerminalResult(threadId, workspacePath)) return;
    // Auto-open a first terminal if none exist yet so the surface is not empty:
    // a new terminal thread's first shell, or a drawer being opened. A terminal
    // thread whose shells have ended is refused by the backend (OpenTerminal),
    // so a pane mounting while its delete runs cannot bring it back.
    if (handle.tabs.length === 0) {
      await openTerminal();
    }
  });
</script>

<div
  class="relative flex flex-1 min-h-0 min-w-0 flex-col"
  data-testid="terminal-surface"
  {@attach paneFindSurface(surface.paneId, activeFindKey)}
>
  <!-- Under compact the open find bar covers the strip, which takes no
       taps or focus until it closes. -->
  <div class="contents" inert={stripCovered}>
    <TerminalTabStrip backend={threadMachine(surface.threadId ?? '', null)}
      {handle}
      onOpen={() => void openTerminal({ focus: true })}
      onClose={closeTerminal}
      onSelect={selectTerminal}
      onRefresh={refreshActiveTerminal}
      onFind={hasPaneFindProvider(activeFindKey()) ? openFind : undefined}
      onCollapse={collapsible ? collapseDrawer : undefined}
      workspacePath={surface.workspacePath}
    />
  </div>

  {#if handle.activeTerminalID}
    {@const findKey = terminalFindKey(surface.paneId, handle.activeTerminalID)}
    {#key handle.activeTerminalID}
      <!-- Static under compact, so the bar places against the surface and
           covers the strip there. -->
      <div class="relative compact:static flex flex-1 min-h-0 min-w-0 flex-col" data-terminal-find-host>
        <TerminalBody
          bind:this={bodyEl}
          {handle}
          terminalID={handle.activeTerminalID}
          paneId={surface.paneId}
          {findKey}
        />
        <!-- The bar floats over the grid (over the strip under compact), so
             opening it changes neither the terminal's rows nor the PTY's
             size. Focus in it is focus in the terminal (terminalFocusRegion). -->
        <div class={TERMINAL_FIND_OVERLAY_CLASS} data-terminal-find-overlay {@attach terminalFocusRegion(surface.paneId)}>
          <PaneFindBar paneId={findKey} title="Terminal" floating fallbackFocus={() => bodyEl?.focusTarget() ?? null} />
        </div>
      </div>
    {/key}
  {:else}
    <div class="flex-1 min-h-0 flex items-center justify-center text-fg-muted text-sm">
      No active terminal.
    </div>
  {/if}
</div>
