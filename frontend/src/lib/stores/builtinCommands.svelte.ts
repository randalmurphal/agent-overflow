// Built-in command registration. Wires command ids to mutations that talk to
// the existing stores/bindings. Kept in its own module so tests can register
// a smaller surface without dragging in every dependency the real app uses.
//
// IMPORTANT — context contract. CommandContext is assembled by App.svelte per
// invocation, so these run callbacks must receive the *current* active thread
// via ctx rather than closing over a cached value.

import type { ThreadPane } from './thread.svelte';
import type { Thread } from '../types/models';
import { registerCommand, type CommandContext } from './commandRegistry.svelte';
import { closeCheatSheet, openCheatSheet } from './cheatSheet.svelte';
import { closeMessageSearch, openMessageSearch } from './messageSearch.svelte';
import { closePalette, openPalette } from './palette.svelte';
import { addToast } from './toast.svelte';
import { removeThread, prependThread, replaceThread } from './threads.svelte';
import { getTerminalFocused } from '../components/terminal/terminalStore.svelte';
import {
  ArchiveThread,
  DeleteThread,
  ForkThread,
  StopSession,
  GitCommit,
  GitPull,
  GitPush,
  GitCreatePR,
  UnarchiveThread,
} from './bindings';

export interface BuiltinCommandHooks {
  pane: ThreadPane;
  openSettings: () => void;
  openThreadForm: () => void;
  openThreadFromPR: () => void;
  openShipChanges: () => void;
  requestRename: (thread: Thread) => void;
  requestDiscussion: (thread: Thread) => void;
  focusThreadSearch: () => void;
  requestThreadJump: (index: number) => void;
  requestThreadStep: (delta: number) => void;
}

function withActiveThread(
  ctx: CommandContext,
  pane: ThreadPane,
  run: (thread: Thread) => void | Promise<void>,
): void {
  if (!ctx.hasActiveThread || !pane.thread) {
    addToast('warning', 'No active thread');
    return;
  }
  void run(pane.thread);
}

export function registerBuiltinCommands(hooks: BuiltinCommandHooks): void {
  const {
    pane,
    openSettings,
    openThreadForm,
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
    run: () => openPalette(),
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
    id: 'thread.new.fromPR',
    label: 'Thread: New from GitHub PR',
    icon: '⇠',
    run: () => openThreadFromPR(),
  });

  registerCommand({
    id: 'thread.new.discussion',
    label: 'Thread: Start Discussion',
    icon: '◆',
    when: 'canStartDiscussion',
    run: (ctx) =>
      withActiveThread(ctx, pane, (t) => {
        requestDiscussion(t);
      }),
  });

  registerCommand({
    id: 'thread.rename',
    label: 'Thread: Rename',
    icon: 'A',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, (t) => {
        requestRename(t);
      }),
  });

  registerCommand({
    id: 'thread.archive',
    label: 'Thread: Archive',
    icon: '▤',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        try {
          await StopSession(t.id).catch(() => {});
          await ArchiveThread(t.id);
          removeThread(t.id);
          pane.clear();
          addToast('info', 'Thread archived');
        } catch (err) {
          addToast('error', `Failed to archive thread: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'thread.unarchive',
    label: 'Thread: Unarchive (restore)',
    icon: '▣',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        if (!t.archived) {
          addToast('info', 'Thread is not archived');
          return;
        }
        try {
          const restored = (await UnarchiveThread(t.id)) as Thread;
          replaceThread(restored);
          pane.replaceThread(restored);
          addToast('info', 'Thread unarchived');
        } catch (err) {
          addToast('error', `Failed to unarchive thread: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'thread.delete',
    label: 'Thread: Delete',
    icon: '✕',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        try {
          await StopSession(t.id).catch(() => {});
          await DeleteThread(t.id);
          removeThread(t.id);
          pane.clear();
          addToast('info', 'Thread deleted');
        } catch (err) {
          addToast('error', `Failed to delete thread: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'thread.fork',
    label: 'Thread: Fork',
    icon: '⎇',
    when: 'canForkActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        try {
          const forked = (await ForkThread(t.id)) as Thread;
          prependThread(forked);
          await pane.switchThread(forked);
          addToast('info', `Forked "${t.title}" into a new thread`);
        } catch (err) {
          addToast('error', `Failed to fork thread: ${err}`);
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

  registerCommand({
    id: 'search.messages',
    label: 'Search: Messages',
    description: 'Full-text search across every thread title and message.',
    icon: '⌕',
    run: () => openMessageSearch(),
  });

  registerCommand({
    id: 'search.messages.close',
    label: 'Search: Close Messages',
    when: 'messageSearchOpen',
    run: () => closeMessageSearch(),
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
    run: (ctx) => withActiveThread(ctx, pane, () => pane.toggleTerminal()),
  });

  registerCommand({
    id: 'diff.panel.toggle',
    label: 'Diffs: Toggle Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) => withActiveThread(ctx, pane, () => pane.toggleDiffPanel()),
  });

  registerCommand({
    id: 'diff.panel.open',
    label: 'Diffs: Open Panel',
    icon: '±',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, () => {
        pane.setDiffPanelOpen(true);
      }),
  });

  registerCommand({
    id: 'diff.panel.close',
    label: 'Diffs: Close Panel',
    icon: '■',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, () => {
        pane.setDiffPanelOpen(false);
      }),
  });

  registerCommand({
    id: 'terminal.new',
    label: 'Terminal: Show',
    icon: '▶',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, () => {
        pane.setShowTerminal(true);
      }),
  });

  registerCommand({
    id: 'terminal.close',
    label: 'Terminal: Hide',
    icon: '■',
    when: 'terminalOpen',
    run: (ctx) =>
      withActiveThread(ctx, pane, () => {
        pane.setShowTerminal(false);
      }),
  });

  registerCommand({
    id: 'git.commit',
    label: 'Git: Commit All',
    icon: '✓',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        const subject = window.prompt('Commit subject');
        if (!subject) return;
        try {
          await GitCommit(t.id, subject, '');
          addToast('success', 'Commit created');
        } catch (err) {
          addToast('error', `Commit failed: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'git.push',
    label: 'Git: Push',
    icon: '↑',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        try {
          await GitPush(t.id);
          addToast('success', 'Pushed');
        } catch (err) {
          addToast('error', `Push failed: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'git.pull',
    label: 'Git: Pull',
    icon: '↓',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        try {
          await GitPull(t.id);
          addToast('success', 'Pulled');
        } catch (err) {
          addToast('error', `Pull failed: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'git.openPR',
    label: 'Git: Open Pull Request',
    icon: '⇥',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, async (t) => {
        const title = window.prompt('PR title', t.title);
        if (!title) return;
        try {
          await GitCreatePR(t.id, title, '', false);
          addToast('success', 'Pull request opened');
        } catch (err) {
          addToast('error', `PR failed: ${err}`);
        }
      }),
  });

  registerCommand({
    id: 'git.ship',
    label: 'Git: Ship Changes (commit → push → PR)',
    icon: '⇪',
    when: 'hasActiveThread',
    run: (ctx) =>
      withActiveThread(ctx, pane, () => {
        openShipChanges();
      }),
  });
}

/**
 * Compute the CommandContext the registry expects. Centralised here so the
 * palette and the keybindings dispatcher see an identical view of the app.
 */
export function makeCommandContext(pane: ThreadPane, extra: Partial<CommandContext>): CommandContext {
  const thread = pane.thread;
  return {
    paletteOpen: false,
    terminalOpen: pane.showTerminal,
    // terminalFocus mirrors whether an xterm element has DOM focus. The
    // TerminalBody component bumps the registry on focus/blur events.
    terminalFocus: getTerminalFocused(),
    approvalPending: pane.pendingApprovals.length > 0,
    anyModalOpen: false,
    hasActiveThread: thread !== null,
    canForkActiveThread: !!thread?.sessionRef,
    canStartDiscussion:
      !!thread && thread.interactionMode !== 'discussion' && !thread.discussionId && !thread.parentThreadId,
    ...extra,
  } as CommandContext;
}

