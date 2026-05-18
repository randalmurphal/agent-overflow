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
import { closeCheatSheet, openCheatSheet } from './cheatSheet.svelte';
import { closeMessageSearch, openMessageSearch } from './messageSearch.svelte';
import { closePalette, openPalette } from './palette.svelte';
import { closeThreadPicker, openThreadPicker } from './threadPicker.svelte';
import { addToast } from './toast.svelte';
import { getActiveTurn, isSendInFlight } from './threadStatuses.svelte';
import {
  closeFocusedPane,
  focusAdjacentPane,
  moveFocusedPane,
  openThreadInPane,
  syncThread,
} from './panes.svelte';
import { removeThread } from './threads.svelte';
import { forkThreadAction } from '../components/sidebar/threadRowActions';
import { userFacingError } from '../utils/userFacingError';
import { getTerminalFocused } from '../components/terminal/terminalStore.svelte';
import {
  ApprovalResponse,
  ArchiveThread,
  DeleteThread,
  StopSession,
  GitCommit,
  GitPull,
  GitPush,
  GitCreatePR,
  InterruptTurn,
  RespondToApproval,
  RespondToUserInput,
  UnarchiveThread,
  UpdateThreadMode,
  UserInputResponse,
} from './bindings';
import { cycleMode } from '../utils/modeCycle';
import { runInterruptOrRevert } from './revertOnInterrupt.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';

export interface BuiltinCommandHooks {
  openSettings: () => void;
  openThreadForm: () => void;
  openThreadInNewPane?: () => void;
  openThreadFromPR: () => void;
  openShipChanges: (paneId: string) => void;
  requestRename: (thread: Thread) => void;
  requestDiscussion: (thread: Thread) => void;
  focusThreadSearch: () => void;
  requestThreadJump: (index: number) => void;
  requestThreadStep: (delta: number) => void;
}

import { reportNonBenignInterruptError } from './interruptErrors';

function withActiveThread(
  ctx: CommandContext,
  run: (thread: Thread) => void | Promise<void>,
): void {
  const pane = ctx.pane;
  if (!ctx.hasActiveThread || !pane.thread) {
    addToast('warning', 'Open a thread before running this command.');
    return;
  }
  void run(pane.thread);
}

export function registerBuiltinCommands(hooks: BuiltinCommandHooks): void {
  const {
    openSettings,
    openThreadForm,
    openThreadInNewPane,
    openThreadFromPR,
    openShipChanges,
    requestRename,
    requestDiscussion,
    focusThreadSearch,
    requestThreadJump,
    requestThreadStep,
  } = hooks;

  registerCommand({
    id: 'palette.open',
    label: 'Command Palette: Open',
    icon: '⌘',
    run: (ctx) => openPalette(ctx.paneId),
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
    description: 'Open the cheat sheet of every command and its current binding.',
    icon: '?',
    run: () => openCheatSheet(),
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
    run: () => openThreadForm(),
  });

  registerCommand({
    id: 'thread.newPane',
    label: 'Thread: New in New Pane',
    icon: '+',
    run: () => openThreadInNewPane?.(),
  });

  registerCommand({
    id: 'pane.close',
    label: 'Pane: Close Focused',
    icon: '×',
    when: '!terminalFocus',
    run: () => closeFocusedPane(),
  });

  registerCommand({
    id: 'pane.focusLeft',
    label: 'Pane: Focus Left',
    icon: '←',
    when: '!terminalFocus',
    run: () => { focusAdjacentPane(-1); },
  });

  registerCommand({
    id: 'pane.focusRight',
    label: 'Pane: Focus Right',
    icon: '→',
    when: '!terminalFocus',
    run: () => { focusAdjacentPane(1); },
  });

  registerCommand({
    id: 'pane.moveLeft',
    label: 'Pane: Move Left',
    icon: '⇠',
    when: '!terminalFocus',
    run: () => { moveFocusedPane(-1); },
  });

  registerCommand({
    id: 'pane.moveRight',
    label: 'Pane: Move Right',
    icon: '⇢',
    when: '!terminalFocus',
    run: () => { moveFocusedPane(1); },
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
      withActiveThread(ctx, async (t) => {
        const pane = ctx.pane;
        try {
          // StopSession can fail if the provider process is already gone;
          // that's fine — we log and continue so the archive still lands.
          // Swallowing silently would hide stuck-session errors that
          // otherwise require a user-visible retry.
          await StopSession(t.id).catch((stopErr) => {
            console.error('Failed to stop session before archive:', stopErr);
          });
          await ArchiveThread(t.id);
          removeThread(t.id);
          pane.clear();
          addToast('info', 'Thread archived.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'thread.unarchive',
    label: 'Thread: Unarchive (restore)',
    icon: '▣',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t) => {
        if (!t.archived) {
          addToast('info', 'Thread is not archived.');
          return;
        }
        try {
          const restored = (await UnarchiveThread(t.id)) as Thread;
          syncThread(restored);
          addToast('info', 'Thread unarchived.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'thread.delete',
    label: 'Thread: Delete',
    icon: '✕',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t) => {
        const pane = ctx.pane;
        try {
          // Same rationale as thread.archive above — log instead of
          // silently swallowing a StopSession failure so the cleanup
          // doesn't become invisible debt.
          await StopSession(t.id).catch((stopErr) => {
            console.error('Failed to stop session before delete:', stopErr);
          });
          await DeleteThread(t.id);
          removeThread(t.id);
          pane.clear();
          addToast('info', 'Thread deleted.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'thread.fork',
    label: 'Thread: Fork',
    icon: '⎇',
    when: 'canForkActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t) => {
        const pane = ctx.pane;
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

  registerCommand({
    id: 'thread.previous',
    label: 'Thread: Previous',
    icon: '↑',
    run: () => requestThreadStep(-1),
  });

  registerCommand({
    id: 'thread.next',
    label: 'Thread: Next',
    icon: '↓',
    run: () => requestThreadStep(1),
  });

  for (let i = 1; i <= 9; i += 1) {
    const index = i;
    registerCommand({
      id: `thread.jump.${i}`,
      label: `Thread: Jump to ${i}`,
      icon: String(i),
      run: () => requestThreadJump(index),
    });
  }

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
    run: () => focusThreadSearch(),
  });

  registerCommand({
    id: 'search.messages',
    label: 'Search: Messages',
    description: 'Full-text search across every thread title and message.',
    icon: '⌕',
    run: (ctx) => openMessageSearch(ctx.paneId),
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
    description: 'Fuzzy-search threads across every project by title.',
    icon: '⌖',
    run: (ctx) => openThreadPicker(ctx.paneId),
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
    label: 'Terminal: Toggle',
    icon: '▶',
    when: 'hasActiveThread',
    run: (ctx) => withActiveThread(ctx, () => ctx.pane.toggleTerminal()),
  });

  registerCommand({
    id: 'diff.panel.toggle',
    label: 'Diffs: Toggle Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) => withActiveThread(ctx, () => ctx.pane.toggleDiffPanel()),
  });

  registerCommand({
    id: 'diff.panel.open',
    label: 'Diffs: Open Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, () => {
        ctx.pane.setDiffPanelOpen(true);
      }),
  });

  registerCommand({
    id: 'diff.panel.close',
    label: 'Diffs: Close Panel',
    icon: '■',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, () => {
        ctx.pane.setDiffPanelOpen(false);
      }),
  });

  registerCommand({
    id: 'terminal.new',
    label: 'Terminal: Show',
    icon: '▶',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, () => {
        ctx.pane.setShowTerminal(true);
      }),
  });

  registerCommand({
    id: 'terminal.close',
    label: 'Terminal: Hide',
    icon: '■',
    when: 'terminalOpen',
    run: (ctx) =>
      withActiveThread(ctx, () => {
        ctx.pane.setShowTerminal(false);
      }),
  });

  registerCommand({
    id: 'git.commit',
    label: 'Git: Commit All',
    icon: '✓',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, async (t) => {
        const subject = window.prompt('Commit subject');
        if (!subject) return;
        try {
          await GitCommit(t.id, subject, '');
          addToast('success', 'Commit created.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
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
      withActiveThread(ctx, async (t) => {
        const title = window.prompt('Pull/merge request title', t.title);
        if (!title) return;
        try {
          await GitCreatePR(t.id, title, '', false);
          addToast('success', 'Request opened.');
        } catch (err) {
          addToast('error', userFacingError(err));
        }
      }),
  });

  registerCommand({
    id: 'git.ship',
    label: 'Git: Ship Changes (commit → push → PR/MR)',
    icon: '⇪',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, () => {
        openShipChanges(ctx.paneId);
      }),
  });
}

/**
 * Compute the CommandContext the registry expects. Centralised here so the
 * palette and the keybindings dispatcher see an identical view of the app.
 */
export function makeCommandContext(pane: ThreadPane, extra: Partial<CommandFlags>): CommandContext {
  const thread = pane.thread;
  const flags: CommandFlags = {
    paletteOpen: false,
    terminalOpen: pane.showTerminal,
    // terminalFocus mirrors whether an xterm element has DOM focus. The
    // TerminalBody component bumps the registry on focus/blur events.
    terminalFocus: getTerminalFocused(),
    approvalPending: pane.pendingApprovals.length > 0,
    anyModalOpen: false,
    hasActiveThread: thread !== null,
    turnActive: getActiveTurn(pane.threadId) !== null,
    sendInFlight: isSendInFlight(pane.threadId, pane.sendInFlight),
    hasPendingPrompt: pane.pendingApprovals.length > 0 || pane.pendingUserInputs.length > 0,
    canForkActiveThread: !!thread?.sessionRef,
    canStartDiscussion:
      !!thread && thread.mode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
    ...extra,
  };
  return {
    ...flags,
    pane,
    paneId: pane.paneId,
    flags,
  } as CommandContext;
}
