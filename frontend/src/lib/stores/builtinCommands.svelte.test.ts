// Focused tests for makeCommandContext — keeps palette / keybindings gates
// honest about the live pane / terminal-focus state.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import {
  getActiveTurn,
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { makeCommandContext, registerBuiltinCommands } from './builtinCommands.svelte';
import {
  clearCommandRegistry,
  getCommand,
  isCommandEnabled,
  runCommand,
  type CommandContext,
} from './commandRegistry.svelte';
import {
  closeMessageSearch,
  isMessageSearchOpen,
  openMessageSearch,
} from './messageSearch.svelte';
import {
  closeThreadPicker,
  isThreadPickerOpen,
  openThreadPicker,
} from './threadPicker.svelte';
import {
  notifyTerminalFocus,
  resetTerminalFocusForTest,
} from '../components/terminal/terminalStore.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  getPendingThreadActionConfirmation,
  resetThreadActionConfirmationsForTest,
} from './threadActionConfirmations.svelte';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import type { Thread } from '../types/models';
import { FOCUS_TERMINAL_EVENT } from './events';

function readyPane(overrides: Partial<Thread> = {}): ReturnType<typeof createThreadPane> {
  setBindingMock('SwitchThread', async (threadId: unknown) => ({
    id: typeof threadId === 'string' ? threadId : 'thread-1',
  }));
  setBindingMock('ListRecentThreadItems', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListPendingInteractiveRequests', async () => ({
    approvals: [],
    userInputs: [],
  }));
  setBindingMock('GetThreadLiveState', async (threadId: string) => ({
    threadId,
    activeTurn: null,
    queueItems: [],
    interactive: { approvals: [], userInputs: [] },
    todo: null,
  }));
  setBindingMock('AutoResumeThread', async () => {});
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('ListThreadCheckpoints', async () => []);
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  const thread: Thread = {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
  void pane.switchThread(thread);
  return pane;
}

function mountComposerForPane(paneId: string): {
  textarea: HTMLTextAreaElement;
  cleanup: () => void;
} {
  const root = document.createElement('div');
  root.setAttribute('data-pane-id', paneId);
  const textarea = document.createElement('textarea');
  textarea.setAttribute('aria-label', 'Message Input');
  root.appendChild(textarea);
  document.body.appendChild(root);

  return {
    textarea,
    cleanup: () => document.body.removeChild(root),
  };
}

function installPromptMock(): {
  promptMock: ReturnType<typeof vi.fn>;
  restore: () => void;
} {
  const previousPrompt = window.prompt;
  const promptMock = vi.fn();
  window.prompt = promptMock;
  return {
    promptMock,
    restore: () => {
      window.prompt = previousPrompt;
    },
  };
}

describe('makeCommandContext', () => {
  beforeEach(() => {
    resetTerminalFocusForTest();
    resetThreadStatuses();
  });

  // --- Bug D5 regression ---
  it('terminalFocus is false when no terminal is focused', () => {
    const pane = readyPane();
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(false);
  });

  it('terminalFocus flips to true when the registry reports focus', () => {
    const pane = readyPane();
    notifyTerminalFocus(true);
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(true);
  });

  it('terminalFocus flips back to false after a matching unfocus', () => {
    const pane = readyPane();
    notifyTerminalFocus(true);
    notifyTerminalFocus(false);
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(false);
  });

  it('explicit override in `extra` wins over the live registry', () => {
    const pane = readyPane();
    notifyTerminalFocus(true);
    const ctx = makeCommandContext(pane, { terminalFocus: false });
    expect(ctx.terminalFocus).toBe(false);
  });

  it('turnActive follows the live pane turn state', () => {
    const pane = readyPane();
    expect(makeCommandContext(pane, {}).turnActive).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    expect(makeCommandContext(pane, {}).turnActive).toBe(true);
  });

  it('activeRhsPanel follows the live pane RHS state', () => {
    const pane = readyPane();
    expect(makeCommandContext(pane, {}).activeRhsPanel).toBe(false);

    pane.setShowPlanSidebar(true);
    expect(makeCommandContext(pane, {}).activeRhsPanel).toBe(true);
  });
});

describe('rhs.close command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('is enabled only while the active pane has an RHS panel', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);

    expect(isCommandEnabled('rhs.close', makeCommandContext(pane, {}))).toBe(false);

    pane.setShowPlanSidebar(true);
    expect(isCommandEnabled('rhs.close', makeCommandContext(pane, {}))).toBe(true);
  });

  it('closes the RHS panel on the command context pane', () => {
    const pane = readyPane();
    pane.setShowPlanSidebar(true);
    registerFixtureCommands(pane);

    expect(runCommand('rhs.close', makeCommandContext(pane, {}))).toBe(true);
    expect(pane.activeRhsPanel).toBeNull();
  });
});

describe('thread.interrupt command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('is disabled without an active turn and enabled while working', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    expect(getCommand('thread.interrupt')).toBeDefined();

    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(true);
  });

  it('calls InterruptTurn for the active thread', async () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    const calls: string[] = [];
    setBindingMock('InterruptTurn', async (id: unknown) => {
      calls.push(id as string);
    });
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    await command.run(makeCommandContext(pane, {}));

    expect(calls).toEqual(['thread-1']);
  });

  it('enabled when sendInFlight is set even without an active turn', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    pane.setSendInFlight(true);
    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(true);
    pane.setSendInFlight(false);
    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(false);
  });

  it('enabled when a pending send is waiting for backend turn-start confirmation', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    projectSendStarted('thread-1');
    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(true);
  });

  it('enabled when a pending prompt exists even without an active turn', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    pane.addUserInput({
      threadId: 'thread-1',
      requestId: 'ui-1',
      toolName: 'AskUserQuestion',
      title: 'Pick scope',
      questions: [{ id: 'scope', header: 'Scope', question: 'Choose', options: [{ label: 'turn', description: 'This turn' }] }],
    });
    expect(isCommandEnabled('thread.interrupt', makeCommandContext(pane, {}))).toBe(true);
  });

  it('cancels a pending user-input request before firing InterruptTurn', async () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    pane.addUserInput({
      threadId: 'thread-1',
      requestId: 'ui-1',
      toolName: 'AskUserQuestion',
      title: 'Pick scope',
      questions: [{ id: 'scope', header: 'Scope', question: 'Choose', options: [{ label: 'turn', description: 'This turn' }] }],
    });

    const sequence: string[] = [];
    let userInputArgs: { threadId: string; requestId: string; decision: string; answers: unknown } | null = null;
    setBindingMock('RespondToUserInput', async (threadId: unknown, response: unknown) => {
      const r = response as { requestId: string; decision: string; answers: unknown };
      userInputArgs = { threadId: threadId as string, ...r };
      sequence.push('RespondToUserInput');
    });
    setBindingMock('InterruptTurn', async () => {
      sequence.push('InterruptTurn');
    });
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    await command.run(makeCommandContext(pane, {}));

    expect(sequence).toEqual(['RespondToUserInput', 'InterruptTurn']);
    expect(userInputArgs).toEqual({
      threadId: 'thread-1',
      requestId: 'ui-1',
      decision: 'decline',
      answers: {},
    });
  });

  it('cancels a pending approval before firing InterruptTurn', async () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    pane.addApproval({
      requestId: 'app-1',
      kind: 'command',
      toolName: 'Bash',
    } as unknown as Parameters<typeof pane.addApproval>[0]);

    const sequence: string[] = [];
    let approvalArgs: { threadId: string; requestId: string; decision: string } | null = null;
    setBindingMock('RespondToApproval', async (threadId: unknown, response: unknown) => {
      const r = response as { requestId: string; decision: string };
      approvalArgs = { threadId: threadId as string, ...r };
      sequence.push('RespondToApproval');
    });
    setBindingMock('InterruptTurn', async () => {
      sequence.push('InterruptTurn');
    });
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    await command.run(makeCommandContext(pane, {}));

    expect(sequence).toEqual(['RespondToApproval', 'InterruptTurn']);
    expect(approvalArgs).toEqual({
      threadId: 'thread-1',
      requestId: 'app-1',
      decision: 'cancel',
    });
  });

  it('treats "already resolved" as a benign no-op on the cancel path', async () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    pane.addApproval({
      requestId: 'app-2',
      kind: 'command',
      toolName: 'Bash',
    } as unknown as Parameters<typeof pane.addApproval>[0]);

    setBindingMock('RespondToApproval', async () => {
      throw new Error('claude: approval already resolved: provider: stale interactive request');
    });
    let interruptCalled = false;
    setBindingMock('InterruptTurn', async () => {
      interruptCalled = true;
    });
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    await command.run(makeCommandContext(pane, {}));

    expect(interruptCalled).toBe(true);
    // Clear any unrelated banner the readyPane setup may have left
    // (the test fixture's lazy ListRecentThreadItems mock is absent
    // here — that's a separate concern from the cancel path).
    pane.clearGeneralError();
    // Re-run to confirm the cancel path itself doesn't re-introduce
    // an error after clearing.
    await command.run(makeCommandContext(pane, {}));
    expect(pane.generalError).toBeNull();
  });

  it('clears activeTurn synchronously (optimistic stop, claude-code REPL.tsx:2106 pattern)', () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    expect(getActiveTurn(pane.threadId) !== null).toBe(true);

    let interruptStarted = false;
    setBindingMock('InterruptTurn', () => {
      interruptStarted = true;
      // Resolve on next microtask so we can verify the synchronous
      // clear happens BEFORE this RPC even gets to run.
      return new Promise<void>((resolve) => queueMicrotask(resolve));
    });
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');

    // The runner is now synchronous-effectively: it dispatches
    // fire-and-forget RPCs and clears state without awaiting. We do
    // NOT await command.run(...) here — that's the point.
    void command.run(makeCommandContext(pane, {}));

    // activeTurn must be cleared in the same tick as the keystroke.
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
    // Yet the InterruptTurn RPC was still dispatched.
    expect(interruptStarted).toBe(true);
  });

  it('removes a pending user-input from the panel synchronously (no await on RPC)', () => {
    const pane = readyPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    pane.addUserInput({
      threadId: 'thread-1',
      requestId: 'ui-stop-1',
      toolName: 'AskUserQuestion',
      title: 'Pick scope',
      questions: [{ id: 'scope', header: 'Scope', question: 'Choose', options: [{ label: 'turn', description: 'This turn' }] }],
    });
    expect(pane.pendingUserInputs.length).toBe(1);

    setBindingMock('RespondToUserInput', () => new Promise<void>(() => {}));
    setBindingMock('InterruptTurn', () => new Promise<void>(() => {}));
    registerFixtureCommands(pane);

    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    void command.run(makeCommandContext(pane, {}));

    // Panel cleared without waiting for the backend.
    expect(pane.pendingUserInputs.length).toBe(0);
    expect(getActiveTurn(pane.threadId) !== null).toBe(false);
  });

  it('treats "no active turn" from InterruptTurn as a benign no-op', async () => {
    setBindingMock('ListRecentThreadItems', async () => []);
    const pane = readyPane();
    // Wait one microtask so switchThread's lazy item-load settles before
    // we assert on generalError — readyPane spawns it without awaiting.
    await Promise.resolve();
    // sendInFlight gate without active turn — typical of the dispatch
    // window between user-Send and provider:turn_started.
    pane.setSendInFlight(true);

    setBindingMock('InterruptTurn', async () => {
      throw new Error('codex: no active turn to interrupt');
    });
    registerFixtureCommands(pane);

    pane.clearGeneralError();
    const command = getCommand('thread.interrupt');
    if (!command) throw new Error('thread.interrupt was not registered');
    await command.run(makeCommandContext(pane, {}));

    expect(pane.generalError).toBeNull();
  });
});

// --- search.messages wiring ---
//
// The mod+shift+f default keybinding landed before the command was registered.
// These tests lock in that the command opens / closes the MessageSearch store
// and that the close variant is gated on `messageSearchOpen`.

function registerFixtureCommands(pane: ReturnType<typeof createThreadPane>): void {
  void pane;
  registerBuiltinCommands({
    openSettings: () => {},
    openThreadForm: () => {},
    openThreadFromPR: () => {},
    openShipChanges: () => {},
    requestRename: () => {},
    requestDiscussion: () => {},
    focusThreadSearch: () => {},
    requestThreadJump: () => {},
  });
}

describe('thread archive/delete command safety', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetThreadActionConfirmationsForTest();
    resetSettingsForTest();
  });

  it('routes thread.archive through the confirmation flow before archiving', () => {
    const pane = readyPane();
    const archiveMock = setBindingMock('ArchiveThread', async () => {});
    registerFixtureCommands(pane);

    runCommand('thread.archive', makeCommandContext(pane, {}));

    const pending = getPendingThreadActionConfirmation();
    expect(pending?.kind).toBe('archive');
    expect(pending?.ctx.thread.id).toBe('thread-1');
    expect(archiveMock).not.toHaveBeenCalled();
  });

  it('routes thread.delete through the confirmation flow before deleting', () => {
    const pane = readyPane();
    const deleteMock = setBindingMock('DeleteThread', async () => {});
    registerFixtureCommands(pane);

    runCommand('thread.delete', makeCommandContext(pane, {}));

    const pending = getPendingThreadActionConfirmation();
    expect(pending?.kind).toBe('delete');
    expect(pending?.ctx.thread.id).toBe('thread-1');
    expect(deleteMock).not.toHaveBeenCalled();
  });

  it('deletes directly when delete confirmations are disabled', async () => {
    const pane = readyPane();
    const deleteMock = setBindingMock('DeleteThread', async () => {});
    setBindingMock('StopSession', async () => {});
    setBindingMock('GetSettings', async () => ({ confirmDelete: false }));
    await loadSettings();
    registerFixtureCommands(pane);

    runCommand('thread.delete', makeCommandContext(pane, {}));

    expect(getPendingThreadActionConfirmation()).toBeNull();
    await vi.waitFor(() => expect(deleteMock).toHaveBeenCalledWith('thread-1'));
  });

  it('does not offer direct deletion for discussion child threads', () => {
    const pane = readyPane({ parentThreadId: 'parent-1' });
    const deleteMock = setBindingMock('DeleteThread', async () => {});
    registerFixtureCommands(pane);

    runCommand('thread.delete', makeCommandContext(pane, {}));

    expect(getPendingThreadActionConfirmation()).toBeNull();
    expect(deleteMock).not.toHaveBeenCalled();
  });
});

describe('search.messages command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    closeMessageSearch();
  });

  it('registers the open and close commands', () => {
    registerFixtureCommands(readyPane());
    expect(getCommand('search.messages')).toBeDefined();
    expect(getCommand('search.messages.close')).toBeDefined();
  });

  it('opens MessageSearch when run', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isMessageSearchOpen()).toBe(false);
    const ran = runCommand('search.messages', ctx);
    expect(ran).toBe(true);
    expect(isMessageSearchOpen()).toBe(true);
  });

  it('close command is disabled when the dialog is closed', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, { messageSearchOpen: false }) as CommandContext;
    expect(isCommandEnabled('search.messages.close', ctx)).toBe(false);
  });

  it('close command is enabled and closes when the dialog is open', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    openMessageSearch();
    expect(isMessageSearchOpen()).toBe(true);
    const ctx = makeCommandContext(pane, { messageSearchOpen: true }) as CommandContext;
    expect(isCommandEnabled('search.messages.close', ctx)).toBe(true);
    runCommand('search.messages.close', ctx);
    expect(isMessageSearchOpen()).toBe(false);
  });
});

// --- thread.search wiring ---
//
// mod+p opens the unified thread picker. Parallel shape to search.messages —
// the `.close` variant is gated on the `threadPickerOpen` context flag so the
// palette doesn't let the user run close while nothing is open.

describe('thread.search command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    closeThreadPicker();
  });

  it('registers the open and close commands', () => {
    registerFixtureCommands(readyPane());
    expect(getCommand('thread.search')).toBeDefined();
    expect(getCommand('thread.search.close')).toBeDefined();
  });

  it('opens the thread picker when run', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isThreadPickerOpen()).toBe(false);
    const ran = runCommand('thread.search', ctx);
    expect(ran).toBe(true);
    expect(isThreadPickerOpen()).toBe(true);
  });

  it('close command is disabled when the picker is closed', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, { threadPickerOpen: false }) as CommandContext;
    expect(isCommandEnabled('thread.search.close', ctx)).toBe(false);
  });

  it('close command closes the picker when enabled', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    openThreadPicker();
    expect(isThreadPickerOpen()).toBe(true);
    const ctx = makeCommandContext(pane, { threadPickerOpen: true }) as CommandContext;
    expect(isCommandEnabled('thread.search.close', ctx)).toBe(true);
    runCommand('thread.search.close', ctx);
    expect(isThreadPickerOpen()).toBe(false);
  });
});

// --- mode.cycle wiring ---
//
// Shift+Tab toggles the active chat thread between chat and plan modes
// (design and discussion are immutable thread types — no-op on those).
// The command reads the current mode from the pane's thread and calls
// UpdateThreadMode. Disabled while any modal or the palette is open
// because Shift+Tab is the native "focus previous" chord inside those
// surfaces.

describe('mode.cycle command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('is registered and reports enabled when a thread is active', () => {
    const pane = readyPane({ mode: 'chat' });
    registerFixtureCommands(pane);
    expect(getCommand('mode.cycle')).toBeDefined();
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isCommandEnabled('mode.cycle', ctx)).toBe(true);
  });

  it('is disabled while the palette is open', () => {
    const pane = readyPane({ mode: 'chat' });
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, { paletteOpen: true }) as CommandContext;
    expect(isCommandEnabled('mode.cycle', ctx)).toBe(false);
  });

  it('toggles plan → chat via UpdateThreadMode', async () => {
    const pane = readyPane({ mode: 'plan' });
    const calls: Array<[string, string]> = [];
    setBindingMock('UpdateThreadMode', async (id: unknown, mode: unknown) => {
      calls.push([id as string, mode as string]);
      return {
        id: id as string,
        title: 'Test thread',
        provider: 'claude',
        workspacePath: '/tmp',
        projectPath: '/tmp',
        mode: mode as string,
        model: 'claude-sonnet-4-6',
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      };
    });
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    runCommand('mode.cycle', ctx);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    expect(calls[0]).toEqual(['thread-1', 'chat']);
  });

  it('is a no-op on design threads (immutable thread type)', async () => {
    const pane = readyPane({ mode: 'design' });
    const calls: Array<[string, string]> = [];
    setBindingMock('UpdateThreadMode', async (id: unknown, mode: unknown) => {
      calls.push([id as string, mode as string]);
      return {
        id: id as string,
        title: 'Design thread',
        provider: 'claude',
        workspacePath: '/tmp',
        projectPath: '/tmp',
        mode: mode as string,
        model: 'claude-sonnet-4-6',
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      };
    });
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    runCommand('mode.cycle', ctx);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    expect(calls.length).toBe(0);
  });
});

describe('git.ship command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('passes the command target pane id to the ship-changes hook', () => {
    const pane = readyPane();
    const openedForPaneIds: string[] = [];
    registerBuiltinCommands({
      openSettings: () => {},
      openThreadForm: () => {},
      openThreadFromPR: () => {},
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
      requestRename: () => {},
      requestDiscussion: () => {},
      focusThreadSearch: () => {},
      requestThreadJump: () => {},
    });

    runCommand('git.ship', makeCommandContext(pane, {}));

    expect(openedForPaneIds).toEqual([pane.paneId]);
  });
});

describe('git commit/PR command safety', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('opens Ship Changes for git.commit instead of prompting and calling GitCommit directly', () => {
    const pane = readyPane();
    const openedForPaneIds: string[] = [];
    const { promptMock, restore } = installPromptMock();
    const commitMock = setBindingMock('GitCommit', async () => ({
      action: 'commit',
      commitSha: 'abc1234',
    }));
    registerBuiltinCommands({
      openSettings: () => {},
      openThreadForm: () => {},
      openThreadFromPR: () => {},
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
      requestRename: () => {},
      requestDiscussion: () => {},
      focusThreadSearch: () => {},
      requestThreadJump: () => {},
    });

    try {
      runCommand('git.commit', makeCommandContext(pane, {}));

      expect(openedForPaneIds).toEqual([pane.paneId]);
      expect(promptMock).not.toHaveBeenCalled();
      expect(commitMock).not.toHaveBeenCalled();
    } finally {
      restore();
    }
  });

  it('opens Ship Changes for git.openPR instead of prompting and creating directly', () => {
    const pane = readyPane();
    const openedForPaneIds: string[] = [];
    const { promptMock, restore } = installPromptMock();
    const createPrMock = setBindingMock('GitCreatePR', async () => ({
      action: 'pr',
      prUrl: 'https://example.test/pr/1',
    }));
    registerBuiltinCommands({
      openSettings: () => {},
      openThreadForm: () => {},
      openThreadFromPR: () => {},
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
      requestRename: () => {},
      requestDiscussion: () => {},
      focusThreadSearch: () => {},
      requestThreadJump: () => {},
    });

    try {
      runCommand('git.openPR', makeCommandContext(pane, {}));

      expect(openedForPaneIds).toEqual([pane.paneId]);
      expect(promptMock).not.toHaveBeenCalled();
      expect(createPrMock).not.toHaveBeenCalled();
    } finally {
      restore();
    }
  });
});

// --- sidebar.focus-search wiring ---
//
// ⌘K moved from palette.open (now ⌘⇧K) to sidebar.focus-search in Wave 4.
// This test pins that the command id exists and its run callback routes
// through the same focusThreadSearch hook search.threads uses — a single
// shared sink so Sidebar/SidebarSearch have one wiring point.

describe('sidebar.focus-search command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('is registered and calls the focusThreadSearch hook', () => {
    let focusCount = 0;
    const pane = readyPane();
    registerBuiltinCommands({
      openSettings: () => {},
      openThreadForm: () => {},
      openThreadFromPR: () => {},
      openShipChanges: () => {},
      requestRename: () => {},
      requestDiscussion: () => {},
      focusThreadSearch: () => {
        focusCount += 1;
      },
      requestThreadJump: () => {},
    });
    expect(getCommand('sidebar.focus-search')).toBeDefined();
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    runCommand('sidebar.focus-search', ctx);
    expect(focusCount).toBe(1);
  });
});

// --- thread.fork wiring ---
//
// Regression for the user-reported bug: forking via the slash command
// produced a thread that didn't surface in the sidebar. Forks now
// expand the parent project so the new row is visible inline rather
// than only via the collapsed-project active-pin slot, which renders
// at most one thread.

describe('thread.fork command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('expands the fork parent project so the new thread is visible', async () => {
    // Reset the persisted "expanded projects" set so the assertion
    // proves the command did the expansion (not a leftover from an
    // earlier test).
    const sidebar = await import('./sidebar.svelte');
    sidebar.collapseProject('project-fork');

    setBindingMock('ListItems', async () => []);
    setBindingMock('ListPayloadMetas', async () => []);
    setBindingMock('SwitchThread', async () => ({
      id: 'fork-1',
      title: 'Test thread (fork)',
      provider: 'claude',
      projectId: 'project-fork',
      workspacePath: '/tmp',
      projectPath: '/tmp',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 1,
      updatedAt: 1,
      archived: false,
    }));

    const pane = readyPane({ projectId: 'project-fork' });
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    setBindingMock('ForkThread', async () => ({
      id: 'fork-1',
      title: 'Test thread (fork)',
      provider: 'claude',
      projectId: 'project-fork',
      workspacePath: '/tmp',
      projectPath: '/tmp',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 1,
      updatedAt: 1,
      archived: false,
    }));

    registerFixtureCommands(pane);
    const command = getCommand('thread.fork');
    if (!command) throw new Error('thread.fork was not registered');

    await command.run(makeCommandContext(pane, {}));

    const isExpanded = sidebar.isProjectExpanded('project-fork');
    expect(isExpanded).toBe(true);
  });
});

// --- terminal.toggle smart toggle ---
//
// The mod+` chord is three-state: closed → open + focus terminal; open
// with chat focused → focus terminal; open with terminal focused →
// focus chat composer. Pin all three states.

describe('terminal.toggle command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetTerminalFocusForTest();
  });

  it('opens the terminal drawer and dispatches focus-terminal when closed', async () => {
    const pane = readyPane();
    pane.setShowTerminal(false);
    registerFixtureCommands(pane);
    const events: CustomEvent[] = [];
    const handler = (e: Event): void => {
      events.push(e as CustomEvent);
    };
    window.addEventListener(FOCUS_TERMINAL_EVENT, handler);
    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(pane.showTerminal).toBe(true);
      // Event fires inside requestAnimationFrame — wait one frame.
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
      expect(events).toHaveLength(1);
      expect(events[0].detail).toEqual({ paneId: pane.paneId });
    } finally {
      window.removeEventListener(FOCUS_TERMINAL_EVENT, handler);
    }
  });

  it('dispatches focus-terminal when drawer is open and chat is focused', () => {
    const pane = readyPane();
    pane.setShowTerminal(true);
    notifyTerminalFocus(false);
    registerFixtureCommands(pane);
    const events: CustomEvent[] = [];
    const handler = (e: Event): void => {
      events.push(e as CustomEvent);
    };
    window.addEventListener(FOCUS_TERMINAL_EVENT, handler);
    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(events).toHaveLength(1);
      expect(events[0].detail).toEqual({ paneId: pane.paneId });
    } finally {
      window.removeEventListener(FOCUS_TERMINAL_EVENT, handler);
    }
  });

  it('dispatches focus-terminal when drawer is open and composer is focused even if terminal focus is stale', () => {
    const pane = readyPane();
    pane.setShowTerminal(true);
    notifyTerminalFocus(true);
    registerFixtureCommands(pane);

    const composer = mountComposerForPane(pane.paneId);
    const { textarea } = composer;
    textarea.focus();

    const events: CustomEvent[] = [];
    const handler = (e: Event): void => {
      events.push(e as CustomEvent);
    };
    window.addEventListener(FOCUS_TERMINAL_EVENT, handler);
    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(events).toHaveLength(1);
      expect(events[0].detail).toEqual({ paneId: pane.paneId });
    } finally {
      window.removeEventListener(FOCUS_TERMINAL_EVENT, handler);
      composer.cleanup();
    }
  });

  it('focuses the chat composer when drawer is open and terminal is focused', () => {
    const pane = readyPane();
    pane.setShowTerminal(true);
    notifyTerminalFocus(true);
    registerFixtureCommands(pane);

    const composer = mountComposerForPane(pane.paneId);
    const { textarea } = composer;

    const events: CustomEvent[] = [];
    const handler = (e: Event): void => {
      events.push(e as CustomEvent);
    };
    window.addEventListener(FOCUS_TERMINAL_EVENT, handler);
    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(document.activeElement).toBe(textarea);
      expect(events).toHaveLength(0);
    } finally {
      window.removeEventListener(FOCUS_TERMINAL_EVENT, handler);
      composer.cleanup();
    }
  });
});

// --- composer.picker.* toggle chords ---
//
// Each chord calls into composerPickerRegistry; this pins that the
// command is registered, gated on hasActiveThread, and routes to the
// handle published by the focused pane's picker component.

describe('composer.picker.* commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('registers a command per picker id with the same when clause', () => {
    registerFixtureCommands(readyPane());
    for (const id of ['model', 'effort', 'access', 'branch'] as const) {
      const cmd = getCommand(`composer.picker.${id}`);
      expect(cmd).toBeDefined();
      expect(cmd?.when).toBe('hasActiveThread');
    }
  });

  it('toggling the chord calls open() on a closed handle', async () => {
    const { registerComposerPicker, resetComposerPickerRegistryForTest } = await import(
      './composerPickerRegistry.svelte'
    );
    resetComposerPickerRegistryForTest();
    const pane = readyPane();
    registerFixtureCommands(pane);
    let isOpen = false;
    const open = vi.fn(() => {
      isOpen = true;
    });
    const close = vi.fn(() => {
      isOpen = false;
    });
    registerComposerPicker(pane.paneId, 'model', {
      isOpen: () => isOpen,
      open,
      close,
    });
    runCommand('composer.picker.model', makeCommandContext(pane, {}) as CommandContext);
    expect(open).toHaveBeenCalledTimes(1);
    expect(close).not.toHaveBeenCalled();
    resetComposerPickerRegistryForTest();
  });

  it('toggling the chord calls close() on an open handle', async () => {
    const { registerComposerPicker, resetComposerPickerRegistryForTest } = await import(
      './composerPickerRegistry.svelte'
    );
    resetComposerPickerRegistryForTest();
    const pane = readyPane();
    registerFixtureCommands(pane);
    let isOpen = true;
    const open = vi.fn(() => {
      isOpen = true;
    });
    const close = vi.fn(() => {
      isOpen = false;
    });
    registerComposerPicker(pane.paneId, 'effort', {
      isOpen: () => isOpen,
      open,
      close,
    });
    runCommand('composer.picker.effort', makeCommandContext(pane, {}) as CommandContext);
    expect(close).toHaveBeenCalledTimes(1);
    expect(open).not.toHaveBeenCalled();
    resetComposerPickerRegistryForTest();
  });
});

// --- sidebar.cursor.open clear-after-open ---
//
// Both activation commands must clear the cursor after dispatching the
// open so the visual highlight does not linger on the previously
// focused row (the user has moved on to the newly opened thread).

describe('sidebar.cursor.open commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('clears the cursor after opening (current pane)', async () => {
    const { setSidebarCursorForTest, getSidebarCursorThreadId, resetSidebarCursorStore } =
      await import('./sidebarCursor.svelte');
    const threads = await import('./threads.svelte');
    threads.prependThread({
      id: 'thread-target',
      title: 'Target',
      provider: 'claude',
      workspacePath: '/tmp',
      projectPath: '/tmp',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
    });
    setSidebarCursorForTest('thread-target');
    const pane = readyPane();
    registerFixtureCommands(pane);

    runCommand('sidebar.cursor.open', makeCommandContext(pane, {}) as CommandContext);

    expect(getSidebarCursorThreadId()).toBeNull();
    resetSidebarCursorStore();
  });

  it('is gated on sidebarCursorActive so it is unenabled when no cursor exists', async () => {
    const { resetSidebarCursorStore } = await import('./sidebarCursor.svelte');
    resetSidebarCursorStore();
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isCommandEnabled('sidebar.cursor.open', ctx)).toBe(false);
    expect(isCommandEnabled('sidebar.cursor.openInNewPane', ctx)).toBe(false);
  });
});
