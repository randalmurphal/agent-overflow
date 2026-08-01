// Built-in command registration. Wires command ids to mutations that talk to
// the existing stores/bindings. Kept in its own module so tests can register
// a smaller surface without dragging in every dependency the real app uses.
//
// IMPORTANT — context contract. CommandContext is assembled by App.svelte per
// invocation, so these run callbacks must receive the *current* active thread
// via ctx rather than closing over a cached value.

import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import type { TerminalHandle } from '../types/terminal';
import { registerCommand, type CommandContext, type CommandFlags } from './commandRegistry.svelte';
import { providerSupports } from '../providers/catalog';
import { closeCheatSheet, isCheatSheetOpen, openCheatSheet } from './cheatSheet.svelte';
import { closeMessageSearch, isMessageSearchOpen, openMessageSearch } from './messageSearch.svelte';
import { closePalette, isPaletteOpen, openPalette } from './palette.svelte';
import { closeThreadPicker, isThreadPickerOpen, openThreadPicker } from './threadPicker.svelte';
import { addToast } from './toast.svelte';
import { getActiveTurn, isSendInFlight } from './threadStatuses.svelte';
import {
  focusAdjacentPane,
  focusPane,
  getFocusedThreadPaneId,
  getPane,
  moveFocusedPane,
  openThreadFromNavigation,
  openThreadInNewPane,
  openThreadInPane,
  syncThread,
} from './panes.svelte';
import { closeFocusedPaneOrCompanion } from './companionPanes.svelte';
import type { PaneLayoutItem } from './paneLayout.svelte';
import { focusPaneComposerIfEditableActive } from '../components/panes/paneComposerFocus';
import { getThreadById } from './threads.svelte';
import { openTerminalThread } from './threadCreation.svelte';
import {
  archiveThreadAction,
  deleteThreadAction,
  forkThreadAction,
  type ThreadActionCtx,
} from '../components/sidebar/threadRowActions';
import { userFacingError } from '../utils/userFacingError';
import {
  getTerminalFocused,
  getExistingThreadTerminalState,
  terminalStateKeyForPane,
} from '../components/terminal/terminalStore.svelte';
import { runTerminalToggle } from '../components/terminal/terminalToggle';
import {
  ApprovalResponse,
  CloseTerminal,
  GitPull,
  GitPush,
  InterruptTurn,
  OpenTerminal,
  RefreshTerminal,
  RespondToApproval,
  RespondToUserInput,
  TerminalOpenOptions,
  UpdateThreadMode,
  UserInputResponse,
} from './bindings';
import { cycleMode } from '../utils/modeCycle';
import { runInterruptOrRevert } from './revertOnInterrupt.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { getSettings, updateSetting } from './settings.svelte';
import { openReviewCompanion } from './reviewPane.svelte';
import { requestThreadActionConfirmation } from './threadActionConfirmations.svelte';
import {
  clearSidebarCursor,
  getSidebarCursorThreadId,
  stepSidebarCursor,
} from './sidebarCursor.svelte';
import {
  toggleComposerPicker,
  type ComposerPickerId,
} from './composerPickerRegistry.svelte';
import { reportNonBenignInterruptError } from './interruptErrors';
import { PICKER_TOGGLE_INPUT_EVENT } from './events';
import { registerWorkflowCommands } from './workflowCommands.svelte';
import {
  getWorkflowsOverlayTop,
  isWorkflowsOverlayOpen,
} from './workflowsOverlay.svelte';

export interface BuiltinCommandHooks {
  openSettings: () => void;
  openThreadForm: () => void;
  // Opens the new-thread form in a new pane (renamed to disambiguate
  // from panes.svelte#openThreadInNewPane, which opens an existing
  // thread in a new pane).
  openThreadFormInNewPane?: () => void;
  openDesignThreadForm: () => void;
  openDesignThreadFormInNewPane?: () => void;
  openThreadFromPR: () => void;
  openShipChanges: (paneId: string) => void;
  requestRename: (thread: Thread) => void;
  requestDiscussion: (thread: Thread) => void;
  focusThreadSearch: () => void;
  requestThreadJump: (index: number) => void;
}

function withActiveThread(
  ctx: CommandContext,
  run: (thread: Thread, pane: ThreadPane) => void | Promise<void>,
): void {
  const pane = ctx.pane;
  if (!pane || !ctx.hasActiveThread || !pane.thread) {
    addToast('warning', 'Open a thread before running this command.');
    return;
  }
  void run(pane.thread, pane);
}

/**
 * Run a command that requires a real (materialized) thread row. Placeholders
 * are intentionally not materialized by non-content commands.
 */
function withMaterializedThread(
  ctx: CommandContext,
  run: (threadId: string, pane: ThreadPane) => void | Promise<void>,
): void {
  const pane = ctx.pane;
  if (!pane || !ctx.hasActiveThread || !pane.thread) {
    addToast('warning', 'Open a thread before running this command.');
    return;
  }
  const threadId = pane.threadId;
  if (!threadId) {
    addToast('warning', 'Start the thread before running this command.');
    return;
  }
  void run(threadId, pane);
}

function commandThreadActionCtx(thread: Thread, pane: ThreadPane): ThreadActionCtx {
  return {
    thread,
    isActive: pane.threadId === thread.id,
    clearPane: () => pane.clear(),
    switchPane: async (next) => { await openThreadInPane(next, pane); },
    reportError: (msg) => pane.setGeneralError(msg),
    replacePaneThread: (next) => pane.replaceThread(next),
  };
}

// Cycle the focused pane's active terminal tab forward (+1) or back (-1),
// wrapping at the ends. Resolves the same per-pane terminal handle the surface
// mounted (terminalStateKeyForPane), mirroring terminal.refresh. No-op when the
// pane has fewer than two tabs.
function switchTerminalTab(ctx: CommandContext, direction: 1 | -1): void {
  const pane = ctx.pane;
  if (!pane) return;
  const state = getExistingThreadTerminalState(
    terminalStateKeyForPane(pane.threadId, pane.paneId),
  );
  if (!state || state.tabs.length < 2) return;
  const index = state.tabs.findIndex((t) => t.terminalID === state.activeTerminalID);
  if (index < 0) return;
  const count = state.tabs.length;
  const next = state.tabs[(index + direction + count) % count];
  state.setActive(next.terminalID);
  // The surface keys TerminalBody on activeTerminalID, so the switch remounts
  // it and drops xterm focus. Signal the surface to re-focus the body so the
  // cursor follows the user into the tab (parity with clicking a tab).
  pane.requestTerminalFocus();
}

export function registerBuiltinCommands(hooks: BuiltinCommandHooks): void {
  const {
    openSettings,
    openThreadForm,
    openThreadFormInNewPane,
    openDesignThreadForm,
    openDesignThreadFormInNewPane,
    openThreadFromPR,
    openShipChanges,
    requestRename,
    requestDiscussion,
    focusThreadSearch,
    requestThreadJump,
  } = hooks;

  registerCommand({
    id: 'palette.open',
    label: 'Command Palette: Open',
    description: 'Open the command palette. Pressing the same chord while open closes it.',
    icon: '⌘',
    editableReachable: true,
    run: (ctx) => {
      if (isPaletteOpen()) closePalette();
      else openPalette(ctx.paneId);
    },
  });

  registerCommand({
    id: 'palette.close',
    label: 'Command Palette: Close',
    icon: '✕',
    when: 'paletteOpen',
    run: () => closePalette(),
  });

  registerCommand({
    id: 'help.keybindings',
    label: 'Help: Show Keyboard Shortcuts',
    description: 'Open the cheat sheet of every command and its current binding. Pressing the same chord while open closes it.',
    icon: '?',
    editableReachable: true,
    run: () => {
      if (isCheatSheetOpen()) closeCheatSheet();
      else openCheatSheet();
    },
  });

  registerCommand({
    id: 'help.keybindings.close',
    label: 'Help: Close Keyboard Shortcuts',
    when: 'cheatSheetOpen',
    run: () => closeCheatSheet(),
  });

  registerCommand({
    id: 'thread.new',
    label: 'Thread: New',
    icon: '+',
    editableReachable: true,
    run: () => openThreadForm(),
  });

  registerCommand({
    id: 'thread.newPane',
    label: 'Thread: New in New Pane',
    icon: '+',
    editableReachable: true,
    run: () => openThreadFormInNewPane?.(),
  });

  registerCommand({
    id: 'thread.new.design',
    label: 'Thread: New Design',
    icon: '+',
    editableReachable: true,
    run: () => openDesignThreadForm(),
  });

  registerCommand({
    id: 'thread.newPane.design',
    label: 'Thread: New Design in New Pane',
    icon: '+',
    editableReachable: true,
    run: () => openDesignThreadFormInNewPane?.(),
  });

  registerCommand({
    id: 'pane.close',
    label: 'Pane: Close Focused',
    icon: '×',
    when: '!terminalFocus',
    editableReachable: true,
    run: () => {
      closeFocusedPaneOrCompanion();
      const nextFocused = getFocusedThreadPaneId();
      if (nextFocused) focusPaneComposerIfEditableActive(nextFocused);
    },
  });

  // Landing on a thread pane moves DOM focus with the logical focus: latch
  // a terminal-mode thread's xterm, otherwise carry an editing caret to the
  // pane's composer. Companion / take-control stops keep logical focus only —
  // they have no composer, and yanking the caret out of one would surprise.
  function followPaneNavDomFocus(item: PaneLayoutItem): void {
    if (item.kind !== 'thread') return;
    const nextPane = getPane(item.paneId);
    if (!nextPane) return;
    if (nextPane.thread?.mode === 'terminal') nextPane.requestTerminalFocus();
    else focusPaneComposerIfEditableActive(nextPane.paneId);
  }

  // Pane navigation stays reachable from inside a focused terminal: the default
  // vim chords (alt+h/l, alt+shift+h/l) are un-gated here AND in the Go
  // defaults, and TerminalBody's xterm key handler lets those configured chords
  // bubble out instead of writing them to the PTY.
  registerCommand({
    id: 'pane.focusLeft',
    label: 'Pane: Focus Left',
    icon: '←',
    editableReachable: true,
    run: () => {
      const next = focusAdjacentPane(-1);
      if (next) followPaneNavDomFocus(next);
    },
  });

  registerCommand({
    id: 'pane.focusRight',
    label: 'Pane: Focus Right',
    icon: '→',
    editableReachable: true,
    run: () => {
      const next = focusAdjacentPane(1);
      if (next) followPaneNavDomFocus(next);
    },
  });

  registerCommand({
    id: 'pane.moveLeft',
    label: 'Pane: Move Left',
    icon: '⇠',
    editableReachable: true,
    run: () => { moveFocusedPane(-1); },
  });

  registerCommand({
    id: 'pane.moveRight',
    label: 'Pane: Move Right',
    icon: '⇢',
    editableReachable: true,
    run: () => { moveFocusedPane(1); },
  });

  registerCommand({
    id: 'review.toggle',
    label: 'Toggle review pane',
    icon: '▥',
    when: 'hasActiveThread && !anyModalOpen && !terminalFocus',
    editableReachable: true,
    run: (ctx) => ctx.pane?.toggleReviewPane(),
  });

  registerCommand({
    id: 'thread.new.fromPR',
    label: 'Thread: New from Pull/Merge Request',
    icon: '⇠',
    run: () => openThreadFromPR(),
  });

  registerCommand({
    id: 'thread.new.discussion',
    label: 'Thread: Start Discussion',
    icon: '◆',
    when: 'canStartDiscussion',
    run: (ctx) =>
      withActiveThread(ctx, (t, pane) => {
        if (!pane.threadId) {
          addToast('warning', 'Start the thread before adding a discussion.');
          return;
        }
        requestDiscussion(t);
      }),
  });

  registerCommand({
    id: 'thread.rename',
    label: 'Thread: Rename',
    icon: 'A',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (t, pane) => {
        if (!pane.threadId) {
          addToast('warning', 'Start the thread before renaming it.');
          return;
        }
        requestRename(t);
      }),
  });

  registerCommand({
    id: 'thread.archive',
    label: 'Thread: Archive',
    icon: '▤',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t, pane) => {
        if (!pane.threadId) {
          addToast('warning', 'Start the thread before archiving it.');
          return;
        }
        const actionCtx = commandThreadActionCtx(t, pane);
        if (getSettings().confirmArchive) {
          requestThreadActionConfirmation('archive', actionCtx);
          return;
        }
        await archiveThreadAction(actionCtx);
      }),
  });

  registerCommand({
    id: 'thread.delete',
    label: 'Thread: Delete',
    icon: '✕',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t, pane) => {
        if (!pane.threadId) {
          addToast('warning', 'Start the thread before deleting it.');
          return;
        }
        if (t.parentThreadId) {
          addToast('warning', 'Discussion child threads are deleted with their parent.');
          return;
        }
        const actionCtx = commandThreadActionCtx(t, pane);
        if (getSettings().confirmDelete) {
          requestThreadActionConfirmation('delete', actionCtx);
          return;
        }
        await deleteThreadAction(actionCtx);
      }),
  });

  registerCommand({
    id: 'thread.fork',
    label: 'Thread: Fork',
    icon: '⎇',
    when: 'canForkActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t, pane) => {
        if (!pane.threadId) {
          addToast('warning', 'Start the thread before forking it.');
          return;
        }
        await forkThreadAction({
          thread: t,
          isActive: pane.threadId === t.id,
          clearPane: () => pane.clear(),
          switchPane: async (next) => { await openThreadInPane(next, pane); },
          reportError: (msg) => pane.setGeneralError(msg),
        });
      }),
  });

  registerCommand({
    id: 'thread.interrupt',
    label: 'Thread: Interrupt Turn',
    icon: '■',
    when: 'hasActiveThread && (turnActive || sendInFlight || hasPendingPrompt) && !anyModalOpen',
    editableReachable: true,
    run: (ctx) => {
      // Industry-pattern interrupt: clear UI state synchronously,
      // dispatch every backend RPC fire-and-forget, let the natural
      // turn_completed event reconcile state when it lands. The user
      // perceives the stop in the same render tick as the keystroke;
      // backend abort propagation runs in parallel.
      //
      // References:
      //   - claude-code-source-code/src/screens/REPL.tsx:2106-2163
      //     `onCancel`: runs `resetLoadingState()` synchronously,
      //     fires `abortController.abort('user-cancel')` without
      //     awaiting, calls `mrOnTurnComplete(messages, true)` to
      //     synthesize completion locally.
      //   - codex/codex-rs/tui/src/chatwidget.rs:5435-5446 +
      //     submit_op at 10964-10985: Esc → `submit_op(Op::Interrupt)`
      //     is non-blocking (`codex_op_tx.send(...)` then `return true`).
      //     Spinner clears when `EventMsg::TurnAborted` arrives.
      //
      // Errors from the RPCs are absorbed via isBenignInterruptError
      // (no-active-turn / already-resolved races against
      // control_cancel_request from the CLI). Pathological failures
      // surface as a banner so a real provider crash doesn't get
      // swallowed.
      const pane = ctx.pane;
      if (!pane) return;
      const threadID = pane.threadId;
      if (!threadID) return;
      const userInput = pane.pendingUserInputs[0];
      const approval = pane.pendingApprovals[0];

      if (userInput) {
        pane.removeUserInput(userInput.requestId);
        void RespondToUserInput(threadID, new UserInputResponse({
          requestId: userInput.requestId,
          decision: 'decline',
          answers: {},
        })).catch((err) => reportNonBenignInterruptError(pane, err));
        // Approval / user-input cancels are mid-turn responses, not the
        // "stop before the agent answered" affordance — fall through to
        // a plain InterruptTurn rather than the revert path.
        void InterruptTurn(threadID).catch((err) =>
          reportNonBenignInterruptError(pane, err),
        );
      } else if (approval) {
        pane.removeApproval(approval.requestId);
        void RespondToApproval(threadID, new ApprovalResponse({
          requestId: approval.requestId,
          decision: 'cancel',
        })).catch((err) => reportNonBenignInterruptError(pane, err));
        void InterruptTurn(threadID).catch((err) =>
          reportNonBenignInterruptError(pane, err),
        );
      } else {
        const draft = getComposerDraftForPane(pane.paneId);
        runInterruptOrRevert(pane, draft ?? {
          content: '',
          attachments: [],
          terminalChips: [],
        });
      }

      // Optimistic clear — spinner / Stop button / mid-turn input
      // gate all flip in this render tick. The real
      // provider:turn_completed arrives shortly and is idempotent
      // on null activeTurn (settleTurn just re-clears).
      pane.clearActiveTurn();
      pane.setSendInFlight(false);
    },
  });

  registerCommand({
    id: 'mode.cycle',
    label: 'Agent Mode: Toggle Chat ↔ Plan',
    description: 'Toggle the active chat thread between chat and plan agent modes. No-op on design / discussion threads — those types are immutable.',
    icon: '⇆',
    when: 'hasActiveThread && !paletteOpen && !anyModalOpen',
    editableReachable: true,
    run: (ctx) =>
      withActiveThread(ctx, async (t, pane) => {
        // Design and discussion threads have immutable types — silently
        // skip the toggle there rather than firing a backend rejection.
        if (t.mode === 'design' || t.mode === 'discussion') return;
        const next = cycleMode(t.mode);
        if (pane.hasDraftPlaceholder) {
          pane.setDraftPlaceholderMode(next);
          return;
        }
        try {
          const updated = (await UpdateThreadMode(t.id, next)) as Thread;
          syncThread(updated);
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  for (let i = 1; i <= 9; i += 1) {
    const index = i;
    registerCommand({
      id: `thread.jump.${i}`,
      label: `Thread: Jump to ${i}`,
      icon: String(i),
      editableReachable: true,
      run: () => requestThreadJump(index),
    });
  }

  // Sidebar visual cursor — a floating highlight over a sidebar row
  // that does NOT take DOM focus. mod+j / mod+k step it; mod+enter /
  // mod+shift+enter activate it. First press lands on the focused
  // pane's thread (cold-start anchor); subsequent presses wrap.
  registerCommand({
    id: 'sidebar.cursor.down',
    label: 'Sidebar: Move Cursor Down',
    description: 'Move the sidebar selection cursor down one row.',
    icon: '↓',
    editableReachable: true,
    run: (ctx) => stepSidebarCursor(1, ctx.pane?.threadId ?? null),
  });

  registerCommand({
    id: 'sidebar.cursor.up',
    label: 'Sidebar: Move Cursor Up',
    description: 'Move the sidebar selection cursor up one row.',
    icon: '↑',
    editableReachable: true,
    run: (ctx) => stepSidebarCursor(-1, ctx.pane?.threadId ?? null),
  });

  registerCommand({
    id: 'sidebar.cursor.open',
    label: 'Sidebar: Open Cursor Thread',
    description: 'Open the thread under the sidebar cursor in the focused pane.',
    icon: '↵',
    when: 'sidebarCursorActive',
    editableReachable: true,
    run: (ctx) => {
      const id = getSidebarCursorThreadId();
      if (!id) return;
      const thread = getThreadById(id);
      if (!thread) return;
      const targetPane = ctx.pane ?? null;
      clearSidebarCursor();
      void openThreadFromNavigation(thread, targetPane);
    },
  });

  registerCommand({
    id: 'sidebar.cursor.openInNewPane',
    label: 'Sidebar: Open Cursor Thread in New Pane',
    description: 'Open the thread under the sidebar cursor in a new pane.',
    icon: '↵',
    when: 'sidebarCursorActive',
    editableReachable: true,
    run: () => {
      const id = getSidebarCursorThreadId();
      if (!id) return;
      const thread = getThreadById(id);
      if (!thread) return;
      clearSidebarCursor();
      void openThreadInNewPane(thread);
    },
  });

  // Composer toolbar pickers — each chord toggles its menu (open if
  // closed, close if open). The actual menu component publishes a
  // handle via composerPickerRegistry on mount; the chord routes to
  // whichever pane currently has focus.
  const composerPickers: Array<[ComposerPickerId, string]> = [
    ['model', 'Composer: Toggle Model Picker'],
    ['effort', 'Composer: Toggle Effort Picker'],
    ['access', 'Composer: Toggle Access Toggle'],
    ['branch', 'Composer: Toggle Branch Picker'],
  ];
  for (const [pickerId, label] of composerPickers) {
    registerCommand({
      id: `composer.picker.${pickerId}`,
      label,
      icon: '⌃',
      when: 'hasActiveThread',
      editableReachable: true,
      run: (ctx) => {
        toggleComposerPicker(ctx.paneId, pickerId);
      },
    });
  }

  // mod+/ within any open picker toggles focus between its search
  // input and its result list. Pickers register a per-mount handler
  // by dispatching a custom event on the window; we route the chord
  // through that handler so each picker owns its own focus logic.
  registerCommand({
    id: 'picker.toggleInput',
    label: 'Picker: Toggle Input Focus',
    description: 'Move focus between the picker search input and its result list.',
    when: 'anyPickerOpen',
    editableReachable: true,
    run: () => {
      if (typeof window === 'undefined') return;
      window.dispatchEvent(new CustomEvent(PICKER_TOGGLE_INPUT_EVENT));
    },
  });

  registerCommand({
    id: 'search.threads',
    label: 'Threads: Focus Search',
    icon: '⌕',
    run: () => focusThreadSearch(),
  });

  // Alias command reachable from the ⌘K global chord. Same behaviour as
  // search.threads; kept as a separate id so the keybindings cheat sheet
  // lists "Sidebar: Focus Search" under its own row (matches what users
  // expect when they glance at the sidebar ⌘K hint).
  registerCommand({
    id: 'sidebar.focus-search',
    label: 'Sidebar: Focus Search',
    description: 'Move focus to the sidebar search input.',
    icon: '⌕',
    editableReachable: true,
    run: () => focusThreadSearch(),
  });

  registerCommand({
    id: 'search.messages',
    label: 'Search: Messages',
    description: 'Full-text search across every thread title and message. Pressing the same chord while open closes it.',
    icon: '⌕',
    // Reachable from the composer: you fire this while typing a message, so the
    // chord must survive textarea focus (the sibling sidebar.focus-search above
    // opts in the same way). Without it, App.svelte's editable-target gate
    // swallows the chord.
    editableReachable: true,
    run: (ctx) => {
      if (isMessageSearchOpen()) closeMessageSearch();
      else openMessageSearch(ctx.paneId);
    },
  });

  registerCommand({
    id: 'search.in-thread',
    label: 'Find in Thread',
    description: 'Search the messages in the current thread. Pressing the same chord while open closes it.',
    icon: '⌕',
    // Reachable while the composer textarea is focused, like search.messages.
    editableReachable: true,
    run: (ctx) => {
      if (isMessageSearchOpen()) {
        closeMessageSearch();
        return;
      }
      if (!ctx.hasActiveThread) {
        addToast('warning', 'Open a thread to search within it.');
        return;
      }
      openMessageSearch(ctx.paneId, 'thread');
    },
  });

  registerCommand({
    id: 'search.messages.close',
    label: 'Search: Close Messages',
    when: 'messageSearchOpen',
    run: () => closeMessageSearch(),
  });

  registerCommand({
    id: 'thread.search',
    label: 'Thread: Open Picker',
    description: 'Fuzzy-search threads across every project by title. Pressing the same chord while open closes it.',
    icon: '⌖',
    run: (ctx) => {
      if (isThreadPickerOpen()) closeThreadPicker();
      else openThreadPicker(ctx.paneId);
    },
  });

  registerCommand({
    id: 'thread.search.close',
    label: 'Thread: Close Picker',
    when: 'threadPickerOpen',
    run: () => closeThreadPicker(),
  });

  registerCommand({
    id: 'settings.open',
    label: 'Settings: Open',
    icon: '⚙',
    run: () => openSettings(),
  });

  registerCommand({
    id: 'settings.toggleLowPowerMode',
    label: 'Settings: Toggle Low Power Mode',
    description:
      'Minimize rendering work: instant scroll placement, chunked text reveal, static working indicator. For weaker machines or when running GPU-heavy apps alongside.',
    icon: '⚙',
    run: () => {
      void updateSetting('lowPowerMode', !getSettings().lowPowerMode);
    },
  });

  registerCommand({
    id: 'terminal.toggle',
    label: 'Terminal: Toggle',
    description:
      'Open the terminal and focus it; if it is already open, close it.',
    icon: '▶',
    when: 'hasActiveThread',
    // Reachable from editable targets so the chord still fires while the
    // xterm <textarea> holds focus — that's the press that closes it.
    editableReachable: true,
    run: (ctx) =>
      withActiveThread(ctx, (_thread, pane) => runTerminalToggle(pane)),
  });

  registerCommand({
    id: 'diff.panel.toggle',
    label: 'Review: Toggle Workspace',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, (threadId, pane) => {
        if (pane.showReviewPane) {
          pane.setShowReviewPane(false);
          return;
        }
        void openReviewCompanion(pane.paneId, threadId, { scope: 'workspace' });
      }),
  });

  registerCommand({
    id: 'review.open',
    label: 'Review: Open',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, (threadId, pane) => {
        void openReviewCompanion(pane.paneId, threadId, { scope: 'workspace' });
      }),
  });

  registerCommand({
    id: 'review.close',
    label: 'Review: Close',
    icon: '■',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        pane.setShowReviewPane(false);
      }),
  });

  registerCommand({
    id: 'terminal.new',
    label: 'Terminal: Show',
    icon: '▶',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        pane.setShowTerminal(true);
      }),
  });

  registerCommand({
    id: 'terminal.close',
    label: 'Terminal: Hide',
    icon: '■',
    when: 'terminalOpen',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        pane.setShowTerminal(false);
      }),
  });

  registerCommand({
    id: 'terminal.newPane',
    label: 'Terminal: New in New Pane',
    icon: '▶',
    // `terminalFocus || hasActiveThread`, not bare `hasActiveThread`: the
    // xterm escape predicate (TERMINAL_ESCAPE_COMMAND_IDS) evaluates this
    // `when` against a synthetic context that sets only `terminalFocus`, so
    // the command must stay enabled there for mod+shift+~ to bubble out of a
    // focused terminal (the natural place to press it). The
    // `hasActiveThread` arm requires a thread everywhere else — the terminal
    // roots at that thread's project/workspace; a no-thread invocation would
    // mint a project-less terminal with no sidebar surface (the standalone
    // Terminals group was removed). `editableReachable` is the other half of
    // the escape: App.svelte only dispatches editable-target chords for
    // editableReachable commands, so without it the chord would no-op from
    // the xterm <textarea>.
    when: 'terminalFocus || hasActiveThread',
    editableReachable: true,
    run: (ctx) => {
      const thread = ctx.pane?.thread;
      void openTerminalThread({
        projectId: thread?.projectId,
        cwd: thread?.workspacePath,
      });
    },
  });

  registerCommand({
    id: 'terminal.refresh',
    label: 'Terminal: Refresh',
    description: 'Repaint the active terminal.',
    icon: '↻',
    // `terminalFocus || terminalOpen`, not bare `terminalOpen`: the xterm escape
    // predicate (TERMINAL_ESCAPE_COMMAND_IDS) evaluates this `when` against a
    // synthetic context that sets only `terminalFocus`, so the command must stay
    // enabled there for alt+shift+r to bubble out of a focused terminal. The
    // `terminalOpen` arm keeps it palette-visible when a terminal is open but
    // unfocused.
    when: 'terminalFocus || terminalOpen',
    // Reachable from editable targets so the chord fires while the xterm
    // <textarea> holds focus — that's the in-terminal recovery press.
    editableReachable: true,
    run: (ctx) => {
      const pane = ctx.pane;
      if (!pane) return;
      // Resolve the focused pane's terminal state under the same key the surface
      // mounted it with (see terminalStateKeyForPane), then nudge the active
      // terminal's PTY so the provider redraws. No-op when the pane has no
      // terminal open.
      const state = getExistingThreadTerminalState(
        terminalStateKeyForPane(pane.threadId, pane.paneId),
      );
      const terminalID = state?.activeTerminalID;
      if (!terminalID) return;
      RefreshTerminal(terminalID).catch((err) => {
        console.error('terminal: RefreshTerminal failed', err);
      });
    },
  });

  // Terminal tab management. Each is `editableReachable` and a member of the
  // frontend TERMINAL_ESCAPE_COMMAND_IDS set, so the chord escapes a focused
  // xterm to run here (like terminal.refresh) instead of being encoded to the
  // PTY. The `terminalFocus || terminalOpen` gate mirrors terminal.refresh: the
  // xterm escape predicate evaluates this `when` against a synthetic
  // terminalFocus-only context, so the command must stay enabled there for the
  // chord to bubble out; the `terminalOpen` arm keeps it palette-runnable when a
  // terminal is open but unfocused.
  registerCommand({
    id: 'terminal.newTab',
    label: 'Terminal: New Tab',
    description: 'Open a new terminal tab in the focused terminal.',
    icon: '＋',
    when: 'terminalFocus || terminalOpen',
    editableReachable: true,
    run: (ctx) => {
      const pane = ctx.pane;
      const threadId = pane?.threadId;
      if (!pane || !threadId) return;
      const state = getExistingThreadTerminalState(
        terminalStateKeyForPane(threadId, pane.paneId),
      );
      if (!state) return;
      OpenTerminal(threadId, new TerminalOpenOptions({ cwd: pane.thread?.workspacePath }))
        .then((th) => {
          const handle = th as TerminalHandle;
          if (handle?.summary) {
            state.addTab(handle.summary);
            // addTab makes the new tab active → the surface remounts
            // TerminalBody; request focus so the cursor lands in the new tab.
            pane.requestTerminalFocus();
          }
        })
        .catch((err) => {
          console.error('terminal: OpenTerminal (newTab) failed', err);
          addToast('error', `Could not open terminal: ${userFacingError(err)}`);
        });
    },
  });

  registerCommand({
    id: 'terminal.closeTab',
    label: 'Terminal: Close Tab',
    description: 'Close the active terminal tab.',
    icon: '×',
    when: 'terminalFocus || terminalOpen',
    editableReachable: true,
    run: (ctx) => {
      const pane = ctx.pane;
      if (!pane) return;
      const state = getExistingThreadTerminalState(
        terminalStateKeyForPane(pane.threadId, pane.paneId),
      );
      const terminalID = state?.activeTerminalID;
      if (!state || !terminalID) return;
      CloseTerminal(terminalID).catch((err) => {
        console.error('terminal: CloseTerminal (closeTab) failed', err);
      });
      // Collapsing the drawer when the last tab closes is owned by the
      // tabs-length $effect in TerminalSurface, so removeTab is all we do here.
      state.removeTab(terminalID);
      // removeTab promotes a sibling to active (a remount). Request focus so
      // the cursor follows into it; when the last tab closed there is nothing
      // to focus (the surface collapses instead), so skip it.
      if (state.activeTerminalID) pane.requestTerminalFocus();
    },
  });

  registerCommand({
    id: 'terminal.nextTab',
    label: 'Terminal: Next Tab',
    description: 'Switch to the next terminal tab.',
    icon: '›',
    when: 'terminalFocus || terminalOpen',
    editableReachable: true,
    run: (ctx) => switchTerminalTab(ctx, 1),
  });

  registerCommand({
    id: 'terminal.prevTab',
    label: 'Terminal: Previous Tab',
    description: 'Switch to the previous terminal tab.',
    icon: '‹',
    when: 'terminalFocus || terminalOpen',
    editableReachable: true,
    run: (ctx) => switchTerminalTab(ctx, -1),
  });

  registerCommand({
    id: 'git.commit',
    label: 'Git: Commit All',
    icon: '✓',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        openShipChanges(pane.paneId);
      }),
  });

  registerCommand({
    id: 'git.push',
    label: 'Git: Push',
    icon: '↑',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, async (threadId) => {
        try {
          await GitPush(threadId);
          addToast('success', 'Pushed.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'git.pull',
    label: 'Git: Pull',
    icon: '↓',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, async (threadId) => {
        try {
          await GitPull(threadId);
          addToast('success', 'Pulled.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'git.openPR',
    label: 'Git: Open Pull/Merge Request',
    icon: '⇥',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        openShipChanges(pane.paneId);
      }),
  });

  registerCommand({
    id: 'git.ship',
    label: 'Git: Ship Changes (commit → push → PR/MR)',
    icon: '⇪',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        openShipChanges(pane.paneId);
      }),
  });

  // Workflows overlay + `/workflow` composer context. Kept in their own module
  // so this file doesn't grow another surface's worth of registrations.
  registerWorkflowCommands();
}

/**
 * Compute the CommandContext the registry expects. Centralised here so the
 * palette and the keybindings dispatcher see an identical view of the app.
 */
export function makeCommandContext(pane: ThreadPane | null, extra: Partial<CommandFlags>): CommandContext {
  const thread = pane?.thread ?? null;
  const flags: CommandFlags = {
    paletteOpen: false,
    terminalOpen: pane?.showTerminal ?? false,
    // terminalFocus mirrors whether THIS pane's xterm element has DOM focus.
    // The TerminalBody component bumps the per-pane registry on focus/blur
    // events. With no pane there's no terminal to gate against, so false —
    // never fall through to "any terminal anywhere is focused", which would
    // suppress an unrelated pane's chords.
    terminalFocus: pane ? getTerminalFocused(pane.paneId) : false,
    approvalPending: pane ? pane.pendingApprovals.length > 0 : false,
    anyModalOpen: false,
    hasActiveThread: thread !== null,
    turnActive: getActiveTurn(pane?.threadId ?? null) !== null,
    sendInFlight: isSendInFlight(pane?.threadId ?? null, pane?.sendInFlight ?? false),
    hasPendingPrompt: pane ? pane.pendingApprovals.length > 0 || pane.pendingUserInputs.length > 0 : false,
    canForkActiveThread: !!thread?.sessionRef && providerSupports(thread?.provider, 'fork'),
    canStartDiscussion:
      !!thread && thread.mode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
    sidebarCursorActive: getSidebarCursorThreadId() !== null,
    anyPickerOpen: false,
    // Overlay scoping for the §8 chords. Derived here rather than passed in by
    // App.svelte so every context builder — palette, per-keypress dispatch,
    // tests — sees the same answer as the overlay itself.
    workflowsOverlayOpen: isWorkflowsOverlayOpen(),
    workflowsRunDetail: isWorkflowsOverlayOpen() && getWorkflowsOverlayTop().level === 'run',
    ...extra,
  };
  return {
    ...flags,
    pane,
    paneId: pane?.paneId ?? null,
    flags,
  } as CommandContext;
}
