// Built-in command registration. Wires command ids to mutations that talk to
// the existing stores/bindings. Kept in its own module so tests can register
// a smaller surface without dragging in every dependency the real app uses.
//
// IMPORTANT — context contract. CommandContext is assembled by App.svelte per
// invocation, so these run callbacks must receive the *current* active thread
// via ctx rather than closing over a cached value.

import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import { registerCommand, type CommandContext, type CommandFlags } from './commandRegistry.svelte';
import { closeCheatSheet, isCheatSheetOpen, openCheatSheet } from './cheatSheet.svelte';
import { closeMessageSearch, isMessageSearchOpen, openMessageSearch } from './messageSearch.svelte';
import { closePalette, isPaletteOpen, openPalette } from './palette.svelte';
import { closeThreadPicker, isThreadPickerOpen, openThreadPicker } from './threadPicker.svelte';
import { addToast } from './toast.svelte';
import { getActiveTurn, isSendInFlight } from './threadStatuses.svelte';
import {
  closeFocusedPane,
  focusAdjacentPane,
  getFocusedPaneId,
  moveFocusedPane,
  openThreadFromNavigation,
  openThreadInNewPane,
  openThreadInPane,
  syncThread,
} from './panes.svelte';
import {
  focusPaneComposer,
  focusPaneComposerIfEditableActive,
  isPaneComposerFocused,
} from '../components/panes/paneComposerFocus';
import { getThreadById } from './threads.svelte';
import {
  archiveThreadAction,
  deleteThreadAction,
  forkThreadAction,
  type ThreadActionCtx,
} from '../components/sidebar/threadRowActions';
import { userFacingError } from '../utils/userFacingError';
import { getTerminalFocused } from '../components/terminal/terminalStore.svelte';
import {
  ApprovalResponse,
  GitPull,
  GitPush,
  InterruptTurn,
  RespondToApproval,
  RespondToUserInput,
  UpdateThreadMode,
  UserInputResponse,
} from './bindings';
import { cycleMode } from '../utils/modeCycle';
import { runInterruptOrRevert } from './revertOnInterrupt.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { getSettings } from './settings.svelte';
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
import { FOCUS_TERMINAL_EVENT, PICKER_TOGGLE_INPUT_EVENT } from './events';

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
 * Run a command that requires a real (materialized) thread row. On a
 * placeholder, this triggers materialization first — same pattern as
 * the composer toolbar pickers. Used by panel-open commands (terminal,
 * diff) whose downstream code keys on a real `pane.threadId`.
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
  void (async () => {
    const threadId = pane.threadId ?? (await pane.ensureMaterializedThread());
    if (!threadId) return;
    await run(threadId, pane);
  })();
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
      closeFocusedPane();
      const nextFocused = getFocusedPaneId();
      if (nextFocused) focusPaneComposerIfEditableActive(nextFocused);
    },
  });

  registerCommand({
    id: 'pane.focusLeft',
    label: 'Pane: Focus Left',
    icon: '←',
    when: '!terminalFocus',
    editableReachable: true,
    run: () => {
      const next = focusAdjacentPane(-1);
      if (next) focusPaneComposerIfEditableActive(next.paneId);
    },
  });

  registerCommand({
    id: 'pane.focusRight',
    label: 'Pane: Focus Right',
    icon: '→',
    when: '!terminalFocus',
    editableReachable: true,
    run: () => {
      const next = focusAdjacentPane(1);
      if (next) focusPaneComposerIfEditableActive(next.paneId);
    },
  });

  registerCommand({
    id: 'pane.moveLeft',
    label: 'Pane: Move Left',
    icon: '⇠',
    when: '!terminalFocus',
    editableReachable: true,
    run: () => { moveFocusedPane(-1); },
  });

  registerCommand({
    id: 'pane.moveRight',
    label: 'Pane: Move Right',
    icon: '⇢',
    when: '!terminalFocus',
    editableReachable: true,
    run: () => { moveFocusedPane(1); },
  });

  registerCommand({
    id: 'rhs.close',
    label: 'Right Sidebar: Close',
    icon: '×',
    when: 'activeRhsPanel && !anyModalOpen && !terminalFocus',
    editableReachable: true,
    run: (ctx) => ctx.pane?.closeRhsPanel(),
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
      withActiveThread(ctx, (t) => {
        requestDiscussion(t);
      }),
  });

  registerCommand({
    id: 'thread.rename',
    label: 'Thread: Rename',
    icon: 'A',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (t) => {
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
      withActiveThread(ctx, async (t) => {
        // Design and discussion threads have immutable types — silently
        // skip the toggle there rather than firing a backend rejection.
        if (t.mode === 'design' || t.mode === 'discussion') return;
        const next = cycleMode(t.mode);
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
    run: (ctx) => {
      if (isMessageSearchOpen()) closeMessageSearch();
      else openMessageSearch(ctx.paneId);
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
    id: 'terminal.toggle',
    label: 'Terminal: Smart Toggle',
    description:
      'Open the terminal if closed; if open, flip focus between the terminal and the chat composer.',
    icon: '▶',
    when: 'hasActiveThread',
    editableReachable: true,
    run: (ctx) =>
      withMaterializedThread(ctx, (_id, pane) => {
        const paneId = pane.paneId;
        if (!pane.showTerminal) {
          pane.setShowTerminal(true);
          requestAnimationFrame(() => {
            window.dispatchEvent(
              new CustomEvent(FOCUS_TERMINAL_EVENT, { detail: { paneId } }),
            );
          });
          return;
        }
        if (getTerminalFocused() && !isPaneComposerFocused(paneId)) {
          focusPaneComposer(paneId);
        } else {
          window.dispatchEvent(
            new CustomEvent(FOCUS_TERMINAL_EVENT, { detail: { paneId } }),
          );
        }
      }),
  });

  registerCommand({
    id: 'diff.panel.toggle',
    label: 'Diffs: Toggle Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) => withMaterializedThread(ctx, (_id, pane) => pane.toggleDiffPanel()),
  });

  registerCommand({
    id: 'diff.panel.open',
    label: 'Diffs: Open Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, (_id, pane) => {
        pane.setDiffPanelOpen(true);
      }),
  });

  registerCommand({
    id: 'diff.panel.close',
    label: 'Diffs: Close Panel',
    icon: '■',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, (_t, pane) => {
        pane.setDiffPanelOpen(false);
      }),
  });

  registerCommand({
    id: 'terminal.new',
    label: 'Terminal: Show',
    icon: '▶',
    when: 'hasActiveThread',
    run: (ctx) =>
      withMaterializedThread(ctx, (_id, pane) => {
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
      withActiveThread(ctx, async (t) => {
        try {
          await GitPush(t.id);
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
      withActiveThread(ctx, async (t) => {
        try {
          await GitPull(t.id);
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
    // terminalFocus mirrors whether an xterm element has DOM focus. The
    // TerminalBody component bumps the registry on focus/blur events.
    terminalFocus: getTerminalFocused(),
    approvalPending: pane ? pane.pendingApprovals.length > 0 : false,
    anyModalOpen: false,
    hasActiveThread: thread !== null,
    activeRhsPanel: pane?.activeRhsPanel !== null && pane?.activeRhsPanel !== undefined,
    turnActive: getActiveTurn(pane?.threadId ?? null) !== null,
    sendInFlight: isSendInFlight(pane?.threadId ?? null, pane?.sendInFlight ?? false),
    hasPendingPrompt: pane ? pane.pendingApprovals.length > 0 || pane.pendingUserInputs.length > 0 : false,
    canForkActiveThread: !!thread?.sessionRef,
    canStartDiscussion:
      !!thread && thread.mode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
    sidebarCursorActive: getSidebarCursorThreadId() !== null,
    anyPickerOpen: false,
    ...extra,
  };
  return {
    ...flags,
    pane,
    paneId: pane?.paneId ?? null,
    flags,
  } as CommandContext;
}
