// Focused tests for makeCommandContext — keeps palette / keybindings gates
// honest about the live pane / terminal-focus state.

import { describe, expect, it, afterEach, beforeEach, vi } from 'vitest';
import { tick } from 'svelte';
import { createThreadPane } from './thread.svelte';
import {
  isSidebarCollapsed,
  resetSidebarLayoutForTest,
  setSidebarCollapsed,
} from './sidebarLayout.svelte';
import { resetSidebarCursorStore, setSidebarCursorForTest } from './sidebarCursor.svelte';
import { resetAppStorageForTest } from './appStorage';
import {
  getActiveTurn,
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from './threadStatuses.svelte';
import { makeCommandContext, registerBuiltinCommands } from './builtinCommands.svelte';
import { pairViewOnly, resetToLocalPage } from '../../test/helpers/scopes';
import {
  clearCommandRegistry,
  getCommand,
  isCommandEnabled,
  runCommand,
  type CommandContext,
} from './commandRegistry.svelte';
import { focusPane, registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { PANE_NAV_COMMAND_IDS, TERMINAL_ESCAPE_COMMAND_IDS } from './paneNavCommands';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import {
  closeMessageSearch,
  getMessageSearchMode,
  getMessageSearchTargetPaneId,
  isMessageSearchOpen,
  openMessageSearch,
} from './messageSearch.svelte';
import { getToasts } from './toast.svelte';
import {
  closeThreadPicker,
  isThreadPickerOpen,
  openThreadPicker,
} from './threadPicker.svelte';
import { closeAccountSwitcher, isAccountSwitcherOpen } from './accountSwitcher.svelte';
import { setPageGrantsFromBootstrap } from '../transport/scopes';
import {
  notifyTerminalFocus,
  resetTerminalFocusForTest,
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
  terminalStateKeyForPane,
} from '../components/terminal/terminalStore.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  getPendingThreadActionConfirmation,
  resetThreadActionConfirmationsForTest,
} from './threadActionConfirmations.svelte';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import {
  isWorkflowsOverlayOpen,
  resetWorkflowsOverlayForTest,
} from './workflowsOverlay.svelte';
import {
  getSettingsSection,
  isSettingsOpen,
  openSettingsOverlay,
  resetSettingsOverlayForTest,
} from './settingsOverlay.svelte';
import { openTerminalThread } from './threadCreation.svelte';
import {
  registerPaneTitleRename,
  resetPaneTitleRenameForTest,
} from './paneTitleRename';
import type { Project, Thread } from '../types/models';
import type { TerminalSessionSummary } from '../types/terminal';

// builtinCommands imports openTerminalThread directly (like runTerminalToggle);
// stub it so terminal.newPane's wiring can be asserted without standing up the
// real StartTerminal binding + pane registry. builtinCommands is the only
// consumer of threadCreation in this test's module graph, so a minimal factory
// is safe.
vi.mock('./threadCreation.svelte', () => ({ openTerminalThread: vi.fn() }));

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
  setBindingMock('ListThreadSliceAround', async () => ({
    items: [],
    oldestTurnIndex: -1,
    hasMore: false,
  }));
  setBindingMock('ListItems', async () => []);
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
    notifyTerminalFocus(pane.paneId, true);
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(true);
  });

  it('terminalFocus flips back to false after a matching unfocus', () => {
    const pane = readyPane();
    notifyTerminalFocus(pane.paneId, true);
    notifyTerminalFocus(pane.paneId, false);
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(false);
  });

  it('explicit override in `extra` wins over the live registry', () => {
    const pane = readyPane();
    notifyTerminalFocus(pane.paneId, true);
    const ctx = makeCommandContext(pane, { terminalFocus: false });
    expect(ctx.terminalFocus).toBe(false);
  });

  // Pane-scoped registry: a terminal focused in another pane must not leak into
  // this pane's context, and a null pane never reports terminal focus. Both
  // would be true under the old module-global counter — the exact cross-pane
  // suppression this refactor removes.
  it('terminalFocus stays false when only another pane has terminal focus', () => {
    const pane = readyPane();
    notifyTerminalFocus('some-other-pane', true);
    const ctx = makeCommandContext(pane, {});
    expect(ctx.terminalFocus).toBe(false);
  });

  it('terminalFocus is false for a null pane even while another terminal is focused', () => {
    notifyTerminalFocus('some-other-pane', true);
    const ctx = makeCommandContext(null, {});
    expect(ctx.terminalFocus).toBe(false);
  });

  it('turnActive follows the live pane turn state', () => {
    const pane = readyPane();
    expect(makeCommandContext(pane, {}).turnActive).toBe(false);

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 123 });
    expect(makeCommandContext(pane, {}).turnActive).toBe(true);
  });

  // The fork palette/keybinding gate (thread.fork's `when: 'canForkActiveThread'`)
  // ANDs a live session reference with the provider's fork capability. claude-tui
  // drives the real TUI from outside and can't fork from AO, so the flag stays
  // false even with a session — the same providerSupports('fork') gate the
  // sidebar context menu uses.
  it('canForkActiveThread requires a session AND a fork-capable provider', () => {
    expect(makeCommandContext(readyPane(), {}).canForkActiveThread).toBe(false);
    expect(
      makeCommandContext(readyPane({ sessionRef: 'sess-1' }), {}).canForkActiveThread,
    ).toBe(true);
    expect(
      makeCommandContext(readyPane({ provider: 'claude-tui', sessionRef: 'sess-1' }), {})
        .canForkActiveThread,
    ).toBe(false);
  });
});

describe('review.toggle command', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('toggles the review companion on the command context pane', () => {
    const pane = readyPane();
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 1 }]);
    registerFixtureCommands(pane);

    expect(isCommandEnabled('review.toggle', makeCommandContext(pane, {}))).toBe(true);
    expect(runCommand('review.toggle', makeCommandContext(pane, {}))).toBe(true);
    expect(pane.showReviewPane).toBe(true);

    expect(runCommand('review.toggle', makeCommandContext(pane, {}))).toBe(true);
    expect(pane.showReviewPane).toBe(false);
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

type BuiltinHooks = Parameters<typeof registerBuiltinCommands>[0];

/**
 * Every hook stubbed to a no-op, with per-test overrides on top. One factory
 * so adding a hook to BuiltinCommandHooks touches this file once instead of
 * once per registration site.
 */
function makeBuiltinHooks(overrides: Partial<BuiltinHooks> = {}): BuiltinHooks {
  return {
    openThreadForm: () => {},
    openThreadFromPR: () => {},
    openShipChanges: () => {},
    requestDiscussion: () => {},
    focusThreadSearch: () => {},
    requestThreadJump: () => {},
    ...overrides,
  };
}

function registerFixtureCommands(pane: ReturnType<typeof createThreadPane>): void {
  void pane;
  registerBuiltinCommands(makeBuiltinHooks());
}

describe('thread rename command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetPaneTitleRenameForTest();
  });

  afterEach(() => {
    resetPaneTitleRenameForTest();
  });

  it('opens the rename editor registered for the command target pane', () => {
    const target = readyPane();
    const targetStart = vi.fn();
    const otherStart = vi.fn();
    registerPaneTitleRename(target.paneId, { start: targetStart });
    registerPaneTitleRename('other-pane', { start: otherStart });
    registerFixtureCommands(target);

    expect(runCommand('thread.rename', makeCommandContext(target, {}))).toBe(true);
    expect(targetStart).toHaveBeenCalledOnce();
    expect(otherStart).not.toHaveBeenCalled();
  });

  it('keeps a replacement handle registered when the older handle releases', () => {
    const pane = readyPane();
    const oldStart = vi.fn();
    const currentStart = vi.fn();
    const releaseOld = registerPaneTitleRename(pane.paneId, { start: oldStart });
    registerPaneTitleRename(pane.paneId, { start: currentStart });
    releaseOld();
    registerFixtureCommands(pane);

    expect(runCommand('thread.rename', makeCommandContext(pane, {}))).toBe(true);
    expect(oldStart).not.toHaveBeenCalled();
    expect(currentStart).toHaveBeenCalledOnce();
  });

  it('warns when the target pane has no mounted rename editor', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const before = getToasts().length;

    expect(runCommand('thread.rename', makeCommandContext(pane, {}))).toBe(true);

    expect(getToasts().slice(before)).toEqual([
      expect.objectContaining({
        type: 'warning',
        message: 'The thread title is not editable here.',
      }),
    ]);
  });
});

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

describe('terminal.newPane command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetTerminalFocusForTest();
    vi.mocked(openTerminalThread).mockClear();
  });

  it('stays enabled while a terminal is focused (the terminalFocus arm of its when)', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    expect(getCommand('terminal.newPane')).toBeDefined();
    // `when: 'terminalFocus || hasActiveThread'` — the terminalFocus arm is
    // what lets the mod+shift+~ chord escape a focused xterm (the escape
    // predicate evaluates the `when` against a synthetic terminalFocus-only
    // context), proven here by the command remaining enabled under focus.
    const ctx = makeCommandContext(pane, { terminalFocus: true }) as CommandContext;
    expect(isCommandEnabled('terminal.newPane', ctx)).toBe(true);
  });

  it('opens a fresh terminal rooted at the active thread project + workspace', () => {
    const pane = readyPane({ projectId: 'proj-9', workspacePath: '/ws/9' });
    registerFixtureCommands(pane);

    const ran = runCommand('terminal.newPane', makeCommandContext(pane, {}));

    expect(ran).toBe(true);
    expect(vi.mocked(openTerminalThread)).toHaveBeenCalledWith({
      projectId: 'proj-9',
      cwd: '/ws/9',
    });
  });

  it('is disabled with no thread context (a project-less terminal would have no sidebar surface)', () => {
    // ctx.pane is null (no focused pane, no focused terminal). Running here
    // used to mint a home terminal for the standalone Terminals group; that
    // group is gone, so the command gates on hasActiveThread instead and must
    // not create a thread nothing in the sidebar can ever show.
    registerFixtureCommands(readyPane());
    const ctx = makeCommandContext(null, {}) as CommandContext;

    expect(isCommandEnabled('terminal.newPane', ctx)).toBe(false);
    runCommand('terminal.newPane', ctx);
    expect(vi.mocked(openTerminalThread)).not.toHaveBeenCalled();
  });
});

describe('terminal tab management commands', () => {
  function termSummary(terminalID: string): TerminalSessionSummary {
    return {
      terminalID,
      threadID: 'thread-1',
      shell: '/bin/bash',
      cwd: '/tmp',
      rows: 24,
      cols: 80,
      pid: 1,
      startedAt: 0,
      running: true,
      exitCode: 0,
      exitReason: '',
    };
  }

  // The commands resolve the focused pane's terminal handle the same way
  // terminal.refresh does (terminalStateKeyForPane). Returns the live handle so
  // a test can seed tabs before running a command and assert against it after.
  function handleForPane(pane: ReturnType<typeof createThreadPane>) {
    return getThreadTerminalState(terminalStateKeyForPane(pane.threadId, pane.paneId));
  }

  beforeEach(() => {
    clearCommandRegistry();
    resetTerminalFocusForTest();
    resetThreadTerminalStatesForTest();
  });

  it('newTab/closeTab/nextTab/prevTab are registered, editableReachable, and enabled under terminalFocus', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, { terminalFocus: true }) as CommandContext;
    for (const id of ['terminal.newTab', 'terminal.closeTab', 'terminal.nextTab', 'terminal.prevTab']) {
      expect(getCommand(id)?.editableReachable).toBe(true);
      expect(isCommandEnabled(id, ctx)).toBe(true);
    }
  });

  it('all four are members of TERMINAL_ESCAPE_COMMAND_IDS (so they escape a focused xterm)', () => {
    for (const id of [
      'terminal.newTab',
      'terminal.closeTab',
      'terminal.nextTab',
      'terminal.prevTab',
      'terminal.newPane',
    ]) {
      expect(TERMINAL_ESCAPE_COMMAND_IDS.has(id)).toBe(true);
    }
  });

  it('terminal.newPane is editableReachable so Ctrl+Shift+~ fires from inside a focused xterm', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    // Escape-set membership (above) lets the chord bubble out of the xterm, but
    // App.svelte only dispatches editable-target chords for editableReachable
    // commands — so this flag on the REAL registration is the other half of the
    // in-terminal new-pane fix. Pinned here (not just in the keybindings fixture)
    // so a regression in the actual registerCommand call is caught.
    expect(getCommand('terminal.newPane')?.editableReachable).toBe(true);
  });

  it('terminal.newTab opens a tab in the focused pane and adds it to the surface', async () => {
    const pane = readyPane({ workspacePath: '/ws/9' });
    registerFixtureCommands(pane);
    const handle = handleForPane(pane);
    handle.addTab(termSummary('term-existing'));
    const openMock = setBindingMock('OpenTerminal', async () => ({
      terminalID: 'term-new',
      summary: termSummary('term-new'),
    }));

    const ran = runCommand('terminal.newTab', makeCommandContext(pane, { terminalFocus: true }));
    expect(ran).toBe(true);
    expect(openMock).toHaveBeenCalledOnce();
    // OpenTerminal resolves on a microtask; addTab runs in its .then.
    await Promise.resolve();
    await Promise.resolve();
    expect(handle.tabs.map((t) => t.terminalID)).toEqual(['term-existing', 'term-new']);
    // The new tab is now active → focus is requested so the cursor lands in it.
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
  });

  it('terminal.closeTab closes the active tab and removes it from the surface', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const handle = handleForPane(pane);
    handle.addTab(termSummary('term-a'));
    handle.addTab(termSummary('term-b')); // addTab activates the newest
    const closeMock = setBindingMock('CloseTerminal', async () => {});

    runCommand('terminal.closeTab', makeCommandContext(pane, { terminalFocus: true }));
    expect(closeMock).toHaveBeenCalledWith('term-b');
    expect(handle.tabs.map((t) => t.terminalID)).toEqual(['term-a']);
    // term-a is promoted to active → focus follows the user into it.
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
  });

  it('terminal.closeTab does not request focus when the last tab is closed', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const handle = handleForPane(pane);
    handle.addTab(termSummary('only'));
    setBindingMock('CloseTerminal', async () => {});

    runCommand('terminal.closeTab', makeCommandContext(pane, { terminalFocus: true }));
    expect(handle.tabs).toHaveLength(0);
    // Nothing remains to focus (the surface collapses instead), so no intent.
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
  });

  it('terminal.nextTab / prevTab cycle the active tab with wraparound', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const handle = handleForPane(pane);
    handle.addTab(termSummary('a'));
    handle.addTab(termSummary('b'));
    handle.addTab(termSummary('c')); // active = c (last)
    const ctx = makeCommandContext(pane, { terminalFocus: true });

    runCommand('terminal.nextTab', ctx); // c → wrap → a
    expect(handle.activeTerminalID).toBe('a');
    runCommand('terminal.prevTab', ctx); // a → wrap → c
    expect(handle.activeTerminalID).toBe('c');
    runCommand('terminal.prevTab', ctx); // c → b
    expect(handle.activeTerminalID).toBe('b');
    // A switch remounts the body, so the cursor must follow into the new tab.
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
  });

  it('terminal.nextTab is a no-op with a single tab', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const handle = handleForPane(pane);
    handle.addTab(termSummary('only'));

    runCommand('terminal.nextTab', makeCommandContext(pane, { terminalFocus: true }));
    expect(handle.activeTerminalID).toBe('only');
    // No switch happened (< 2 tabs), so no focus intent is set.
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
  });

  it('newTab/closeTab/nextTab are safe no-ops when the pane has no terminal state', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    // No handleForPane(pane) call → getExistingThreadTerminalState returns null,
    // exercising the `if (!state) return` guards. The commands stay enabled and
    // run, but must no-op without throwing or hitting a binding.
    const openMock = setBindingMock('OpenTerminal', async () => ({
      terminalID: 'unexpected',
      summary: termSummary('unexpected'),
    }));
    const closeMock = setBindingMock('CloseTerminal', async () => {});
    const ctx = makeCommandContext(pane, { terminalFocus: true });

    expect(() => {
      expect(runCommand('terminal.nextTab', ctx)).toBe(true);
      expect(runCommand('terminal.closeTab', ctx)).toBe(true);
      expect(runCommand('terminal.newTab', ctx)).toBe(true);
    }).not.toThrow();
    expect(openMock).not.toHaveBeenCalled();
    expect(closeMock).not.toHaveBeenCalled();
    // Every command bailed at its state guard before mutating, so none of them
    // requested terminal focus.
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
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

// --- search.in-thread wiring ---
//
// mod+f opens the SAME MessageSearch overlay as search.messages, but in
// thread-scoped mode (Find in thread). It reuses the toggle-on-rechord shape
// and additionally refuses to open without an active thread — there is
// nothing to scope to — surfacing a warning toast instead.

describe('search.in-thread command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    closeMessageSearch();
  });

  it('registers the command', () => {
    registerFixtureCommands(readyPane());
    expect(getCommand('search.in-thread')).toBeDefined();
  });

  it('opens MessageSearch in thread mode scoped to the active pane', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isMessageSearchOpen()).toBe(false);
    const ran = runCommand('search.in-thread', ctx);
    expect(ran).toBe(true);
    expect(isMessageSearchOpen()).toBe(true);
    // The distinguishing behavior vs search.messages: thread mode + a pane id
    // so the overlay knows which thread to scope SearchThreadItems to.
    expect(getMessageSearchMode()).toBe('thread');
    expect(getMessageSearchTargetPaneId()).toBe(pane.paneId);
  });

  it('toggles closed when the same chord runs while open', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    runCommand('search.in-thread', ctx);
    expect(isMessageSearchOpen()).toBe(true);
    runCommand('search.in-thread', ctx);
    expect(isMessageSearchOpen()).toBe(false);
  });

  it('refuses to open without an active thread and warns instead', () => {
    registerFixtureCommands(readyPane());
    const before = getToasts().length;
    // A null pane yields hasActiveThread:false / paneId:null — opening an
    // unscoped in-thread find would search nothing, so the run bails.
    const ctx = makeCommandContext(null, {}) as CommandContext;
    runCommand('search.in-thread', ctx);
    expect(isMessageSearchOpen()).toBe(false);
    const added = getToasts().slice(before);
    expect(added.some((t) => t.type === 'warning')).toBe(true);
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

// --- provider.switchAccount wiring ---
//
// mod+shift+u toggles the account-switcher picker. Unlike thread.search there
// is no `.close` twin — Modal's own Escape closes it — so the single command
// has to toggle. Its `when` is the view-only gate: every provider-account RPC
// needs `access:admin`, so the command must be DISABLED in a
// view-only session rather than opening a picker that can't act.

describe('provider.switchAccount command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    closeAccountSwitcher();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    closeAccountSwitcher();
    setPageGrantsFromBootstrap(false);
  });

  it('toggles the picker open and closed on repeated runs', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    const ctx = makeCommandContext(pane, {}) as CommandContext;

    expect(isAccountSwitcherOpen()).toBe(false);
    expect(runCommand('provider.switchAccount', ctx)).toBe(true);
    expect(isAccountSwitcherOpen()).toBe(true);
    expect(runCommand('provider.switchAccount', ctx)).toBe(true);
    expect(isAccountSwitcherOpen()).toBe(false);
  });

  it('stays reachable from an editable target, like the composer pickers', () => {
    registerFixtureCommands(readyPane());
    expect(getCommand('provider.switchAccount')?.editableReachable).toBe(true);
  });

  it('is disabled — and refuses to run — without the access:admin grant', () => {
    const pane = readyPane();
    registerFixtureCommands(pane);
    // A page served over the network holds no grant of its own, so the
    // provider-account surface is out of reach.
    setPageGrantsFromBootstrap(true);
    const ctx = makeCommandContext(pane, {}) as CommandContext;

    expect(ctx.flags.accessAdmin).toBe(false);
    expect(isCommandEnabled('provider.switchAccount', ctx)).toBe(false);
    expect(runCommand('provider.switchAccount', ctx)).toBe(false);
    expect(isAccountSwitcherOpen()).toBe(false);
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

  it('is a no-op on discussion threads (immutable thread type)', async () => {
    const pane = readyPane({ mode: 'discussion' });
    const calls: Array<[string, string]> = [];
    setBindingMock('UpdateThreadMode', async (id: unknown, mode: unknown) => {
      calls.push([id as string, mode as string]);
      return {
        id: id as string,
        title: 'Discussion thread',
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
    registerBuiltinCommands(makeBuiltinHooks({
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
    }));

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
    registerBuiltinCommands(makeBuiltinHooks({
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
    }));

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
    registerBuiltinCommands(makeBuiltinHooks({
      openShipChanges: (paneId) => {
        openedForPaneIds.push(paneId);
      },
    }));

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
    registerBuiltinCommands(makeBuiltinHooks({
      focusThreadSearch: () => {
        focusCount += 1;
      },
    }));
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

// --- terminal.toggle (VS Code-style visibility toggle) ---
//
// The mod+` chord is a two-state toggle: closed → open + focus the
// terminal; open → close it, regardless of where focus currently sits.
// On close, focus returns to the composer ONLY when the terminal itself
// held focus — so the caret doesn't strand on <body> after the drawer
// unmounts, and isn't yanked away when the user toggled from elsewhere.

describe('terminal.toggle command', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetTerminalFocusForTest();
  });

  it('opens the terminal drawer and latches a focus request when closed', () => {
    const pane = readyPane();
    pane.setShowTerminal(false);
    registerFixtureCommands(pane);

    runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
    expect(pane.showTerminal).toBe(true);
    // Opening latches a one-shot focus intent synchronously (no rAF). The
    // drawer consumes it in onMount — whenever its lazily-loaded chunk
    // resolves — so the intent survives the cold-open mount delay.
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
    // Read-and-clear: a second consume (e.g. a drawer remount) returns false,
    // so focus is never re-grabbed without a fresh open.
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
  });

  it('closes the terminal and returns focus to the composer when the terminal was focused', () => {
    const pane = readyPane();
    pane.setShowTerminal(true);
    notifyTerminalFocus(pane.paneId, true);
    registerFixtureCommands(pane);

    const composer = mountComposerForPane(pane.paneId);

    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(pane.showTerminal).toBe(false);
      // Focus handed back to the composer so the unmount doesn't orphan it.
      expect(document.activeElement).toBe(composer.textarea);
      // A close never latches a focus request — it only ever closes.
      expect(pane.consumeTerminalFocusRequest()).toBe(false);
    } finally {
      composer.cleanup();
    }
  });

  it('closes the terminal without stealing focus when the terminal was not focused', () => {
    const pane = readyPane();
    pane.setShowTerminal(true);
    notifyTerminalFocus(pane.paneId, false);
    registerFixtureCommands(pane);

    // A composer exists, but focus is parked on an unrelated element. The
    // toggle must NOT pull focus into the composer here — the terminal
    // didn't hold focus, so there's nothing to rescue. This guards the
    // `if (terminalHadFocus)` condition against being dropped.
    const composer = mountComposerForPane(pane.paneId);
    const sentinel = document.createElement('button');
    document.body.appendChild(sentinel);
    sentinel.focus();

    try {
      runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);
      expect(pane.showTerminal).toBe(false);
      expect(document.activeElement).toBe(sentinel);
      expect(pane.consumeTerminalFocusRequest()).toBe(false);
    } finally {
      document.body.removeChild(sentinel);
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

// --- placeholder command gating ---
//
// `terminal.toggle` / `terminal.new` may run on placeholders and bind to the synthetic
// placeholder id. Diff still requires a materialized row because its backend
// bindings hit persisted thread data.

function placeholderPane(
  paneId = 'placeholder-cmd',
): ReturnType<typeof createThreadPane> {
  const pane = createThreadPane({ paneId });
  const project: Project = {
    id: 'project-placeholder-cmd',
    path: '/tmp/placeholder-cmd',
    name: 'Placeholder',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
  pane.startDraftPlaceholder(project, 'chat');
  return pane;
}

describe('thread-bound commands on placeholders', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetThreadStatuses();
  });

  it('terminal.toggle on a placeholder opens without creating a thread', () => {
    const pane = placeholderPane('term-toggle-draft');
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called for terminal.toggle on a placeholder');
    });
    registerFixtureCommands(pane);

    expect(pane.threadId).toBeNull();
    expect(pane.thread?.isDraft).toBe(true);
    expect(pane.showTerminal).toBe(false);

    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(ctx.hasActiveThread).toBe(true);
    runCommand('terminal.toggle', ctx);

    expect(create).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(true);
  });

  it('terminal.new on a placeholder opens without creating a thread', () => {
    const pane = placeholderPane('term-new-draft');
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called for terminal.new on a placeholder');
    });
    registerFixtureCommands(pane);

    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(false);

    runCommand('terminal.new', makeCommandContext(pane, {}) as CommandContext);

    expect(create).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
    expect(pane.showTerminal).toBe(true);
  });

  it('terminal.toggle on a real thread does not call CreateThread', async () => {
    const pane = readyPane();
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called when the pane already has a real thread');
    });
    registerFixtureCommands(pane);

    expect(pane.threadId).toBe('thread-1');
    expect(pane.showTerminal).toBe(false);

    runCommand('terminal.toggle', makeCommandContext(pane, {}) as CommandContext);

    await vi.waitFor(() => expect(pane.showTerminal).toBe(true));
    expect(create).not.toHaveBeenCalled();
  });

  it('diff.panel.toggle on a placeholder does not create a thread', () => {
    const pane = placeholderPane('diff-toggle-draft');
    const create = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread must not be called for diff.panel.toggle on a placeholder');
    });
    registerFixtureCommands(pane);

    expect(pane.threadId).toBeNull();
    expect(pane.showReviewPane).toBe(false);

    runCommand('diff.panel.toggle', makeCommandContext(pane, {}) as CommandContext);

    expect(create).not.toHaveBeenCalled();
    expect(pane.threadId).toBeNull();
    expect(pane.showReviewPane).toBe(false);
  });
});

// --- pane.focusLeft / focusRight: focus-into-terminal ---
//
// Navigating INTO a terminal pane must latch the pane's terminal-focus intent
// so the xterm grabs the keyboard (a terminal has no composer for the editable-
// focus helper to target). Navigating into a chat pane must NOT latch it.
describe('pane navigation into a terminal pane', () => {
  function makeThread(over: Partial<Thread> = {}): Thread {
    return {
      id: 'thread-x',
      title: 'T',
      provider: 'claude',
      workspacePath: '/w',
      projectPath: '/p',
      projectId: 'p1',
      mode: 'chat',
      model: 'claude-sonnet-4-6',
      createdAt: 0,
      updatedAt: 0,
      archived: false,
      ...over,
    };
  }

  // main (chat, focused) | right (caller-supplied). focusRight lands on `right`.
  function twoPaneLayout(rightThread: Thread) {
    const left = createThreadPane({ paneId: 'main' });
    left.replaceThread(makeThread({ id: 'left', mode: 'chat' }));
    const right = createThreadPane({ paneId: 'right' });
    right.replaceThread(rightThread);
    registerPaneForTest('main', left);
    registerPaneForTest('right', right);
    setPaneLayoutItemsForTest([
      { id: 'i-main', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'i-right', paneId: 'right', kind: 'thread', widthPx: 1 },
    ]);
    focusPane('main');
    registerFixtureCommands(left);
    return { left, right };
  }

  beforeEach(() => {
    clearCommandRegistry();
    resetPanesForTest();
    resetPaneLayoutForTest();
  });

  it('latches terminal focus when focusRight lands on a terminal pane', () => {
    const { left, right } = twoPaneLayout(makeThread({ id: 'term', mode: 'terminal' }));
    const ran = runCommand('pane.focusRight', makeCommandContext(left, {}));
    expect(ran).toBe(true);
    // requestTerminalFocus was called → the read-and-clear intent is set.
    expect(right.consumeTerminalFocusRequest()).toBe(true);
  });

  it('does not latch terminal focus when focusRight lands on a chat pane', () => {
    const { left, right } = twoPaneLayout(makeThread({ id: 'chat2', mode: 'chat' }));
    runCommand('pane.focusRight', makeCommandContext(left, {}));
    expect(right.consumeTerminalFocusRequest()).toBe(false);
  });

  it('every PANE_NAV_COMMAND_IDS entry is a registered builtin command', () => {
    // The xterm escape predicate (eventEscapesTerminalToCommand) and the Go
    // un-gated alt-chord defaults both key off this hand-maintained set. If a
    // pane-nav command id is renamed in builtinCommands without updating the
    // set (or vice versa), its chord would silently fall back to the PTY — this
    // pins the set to the registry so that drift fails loudly.
    registerFixtureCommands(createThreadPane({ paneId: 'reg' }));
    const missing = [...PANE_NAV_COMMAND_IDS].filter((id) => !getCommand(id));
    expect(missing).toEqual([]);
  });

  it('every TERMINAL_ESCAPE_COMMAND_IDS entry is a registered builtin command', () => {
    // The terminal key handler lets exactly these ids bubble out of a focused
    // xterm (pane nav + terminal.refresh). If terminal.refresh were renamed in
    // builtinCommands without updating the set (or vice versa), alt+shift+r would
    // silently fall back to the PTY — pin the set to the registry so drift fails.
    registerFixtureCommands(createThreadPane({ paneId: 'reg' }));
    const missing = [...TERMINAL_ESCAPE_COMMAND_IDS].filter((id) => !getCommand(id));
    expect(missing).toEqual([]);
  });

  it('terminal.refresh nudges the focused pane\'s active terminal via RefreshTerminal', async () => {
    resetThreadTerminalStatesForTest();
    registerFixtureCommands(createThreadPane({ paneId: 'reg' }));

    // Seed the focused pane's terminal state under the same key the surface uses
    // (pane id here, since the bare pane has no thread) with TWO tabs, then make
    // the first one active. addTab activates the last-added tab, so selecting
    // term-1 here proves refresh targets the *active* terminal, not "the only
    // tab" or "the last tab".
    const pane = createThreadPane({ paneId: 'term-pane' });
    const state = getThreadTerminalState(pane.threadId ?? pane.paneId);
    const seedTab = (terminalID: string) =>
      state.addTab({
        terminalID,
        threadID: 'thread-x',
        shell: '/bin/bash',
        cwd: '/tmp',
        rows: 24,
        cols: 80,
        pid: 1,
        startedAt: 0,
        running: true,
        exitCode: 0,
        exitReason: '',
      });
    seedTab('term-1');
    seedTab('term-2');
    state.setActive('term-1');
    expect(state.activeTerminalID).toBe('term-1');

    let refreshedID: string | null = null;
    setBindingMock('RefreshTerminal', async (id: unknown) => {
      refreshedID = id as string;
    });

    getCommand('terminal.refresh')!.run(makeCommandContext(pane, {}));
    await Promise.resolve();

    expect(refreshedID).toBe('term-1');
  });

  it('terminal.refresh is a no-op when the focused pane has no terminal open', () => {
    resetThreadTerminalStatesForTest();
    registerFixtureCommands(createThreadPane({ paneId: 'reg' }));

    const pane = createThreadPane({ paneId: 'empty-pane' });
    let called = false;
    setBindingMock('RefreshTerminal', async () => {
      called = true;
    });

    getCommand('terminal.refresh')!.run(makeCommandContext(pane, {}));
    expect(called).toBe(false);
  });
});

// --- sidebar.toggle + the sidebar-relative command guard (t3 7.2) ---
//
// Collapsing unmounts the sidebar's rendered tree, which is what
// `getVisibleSidebarThreadIds` walks and what the search input lives
// in. Commands that target either must bring the sidebar back rather
// than act on rows nobody can see.

describe('sidebar.toggle command', () => {
  function register(hooks: Partial<BuiltinHooks> = {}): void {
    registerBuiltinCommands(makeBuiltinHooks(hooks));
  }

  beforeEach(() => {
    clearCommandRegistry();
    resetAppStorageForTest();
    resetSidebarLayoutForTest();
    resetSidebarCursorStore();
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
  });

  it('is registered and toggles the collapsed state both ways', () => {
    register();
    expect(getCommand('sidebar.toggle')).toBeDefined();
    const ctx = makeCommandContext(null, {}) as CommandContext;

    expect(isSidebarCollapsed()).toBe(false);
    runCommand('sidebar.toggle', ctx);
    expect(isSidebarCollapsed()).toBe(true);
    runCommand('sidebar.toggle', ctx);
    expect(isSidebarCollapsed()).toBe(false);
  });

  it('is enabled with no thread open — the sidebar is app chrome', () => {
    register();
    expect(isCommandEnabled('sidebar.toggle', makeCommandContext(null, {}) as CommandContext))
      .toBe(true);
  });

  it('sidebar.focus-search expands before focusing so the input exists', async () => {
    let focusCount = 0;
    let collapsedAtFocus: boolean | null = null;
    register({
      focusThreadSearch: () => {
        focusCount += 1;
        collapsedAtFocus = isSidebarCollapsed();
      },
    });
    setSidebarCollapsed(true);

    runCommand('sidebar.focus-search', makeCommandContext(null, {}) as CommandContext);
    expect(focusCount).toBe(0); // deferred until the expand has flushed
    await tick();
    expect(focusCount).toBe(1);
    expect(collapsedAtFocus).toBe(false);
    expect(isSidebarCollapsed()).toBe(false);
  });

  it('sidebar.focus-search stays synchronous when already expanded', () => {
    let focusCount = 0;
    register({ focusThreadSearch: () => { focusCount += 1; } });
    runCommand('sidebar.focus-search', makeCommandContext(null, {}) as CommandContext);
    expect(focusCount).toBe(1);
  });

  it('thread.jump.N expands before resolving the Nth rendered row', async () => {
    const jumps: number[] = [];
    register({ requestThreadJump: (index) => jumps.push(index) });
    setSidebarCollapsed(true);

    runCommand('thread.jump.3', makeCommandContext(null, {}) as CommandContext);
    expect(jumps).toEqual([]);
    await tick();
    expect(jumps).toEqual([3]);
    expect(isSidebarCollapsed()).toBe(false);
  });

  it('sidebar cursor activate chords go inert while the sidebar is hidden', () => {
    register();
    setSidebarCursorForTest('thread-9');
    expect(
      (makeCommandContext(null, {}) as CommandContext).flags.sidebarCursorActive,
    ).toBe(true);

    setSidebarCollapsed(true);
    expect(
      (makeCommandContext(null, {}) as CommandContext).flags.sidebarCursorActive,
    ).toBe(false);

    // Expanding hands the cursor back rather than discarding it.
    setSidebarCollapsed(false);
    expect(
      (makeCommandContext(null, {}) as CommandContext).flags.sidebarCursorActive,
    ).toBe(true);
  });
});

// --- settings.open / settings.close wiring ---
//
// Settings moved from a PaneHost globalSurface to a layered overlay, which is
// what gave it an Esc close at all. The chord is `esc` gated on `settingsOpen`
// (internal/keybindings Defaults), so the gate and the editable-target opt-in
// are the two properties that decide whether Esc actually works.

describe('settings commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetWorkflowsOverlayForTest();
    resetSettingsOverlayForTest();
    registerBuiltinCommands(makeBuiltinHooks());
  });

  it('opens the settings overlay on its General tab', () => {
    expect(runCommand('settings.open', makeCommandContext(null, {}) as CommandContext)).toBe(true);
    expect(isSettingsOpen()).toBe(true);
    expect(getSettingsSection()).toBe('general');
  });

  // `settingsOpen` is DERIVED in makeCommandContext, not supplied by the
  // caller: `settings.close` is gated on it, so a builder that forgot to pass
  // it would leave Esc silently inert on an open surface.
  it('derives settingsOpen from the store and gates settings.close on it', () => {
    const closedCtx = makeCommandContext(null, {}) as CommandContext;
    expect(closedCtx.flags.settingsOpen).toBe(false);
    expect(isCommandEnabled('settings.close', closedCtx)).toBe(false);
    expect(runCommand('settings.close', closedCtx)).toBe(false);

    openSettingsOverlay('general');
    const openCtx = makeCommandContext(null, {}) as CommandContext;
    expect(openCtx.flags.settingsOpen).toBe(true);
    expect(isCommandEnabled('settings.close', openCtx)).toBe(true);

    expect(runCommand('settings.close', openCtx)).toBe(true);
    expect(isSettingsOpen()).toBe(false);
  });

  // Settings is mostly text fields; App.svelte only dispatches editable-target
  // chords for editableReachable commands, so without the flag Esc would be
  // inert from inside every input on the surface.
  it('keeps settings.close reachable from a focused text field', () => {
    expect(getCommand('settings.close')?.editableReachable).toBe(true);
  });

  // Both surfaces are full-height layers over the pane strip with their own
  // focus trap, so opening either closes the other. Both directions live in
  // the stores now: `openSettingsOverlay` calls `closeWorkflowsOverlay`, and
  // the settings store arms the reverse hook at module init.
  it('is mutually exclusive with the workflows overlay, both directions', () => {
    openSettingsOverlay('general');
    runCommand('workflows.toggle', makeCommandContext(null, {}) as CommandContext);
    expect(isWorkflowsOverlayOpen()).toBe(true);
    expect(isSettingsOpen()).toBe(false);

    runCommand('settings.open', makeCommandContext(null, {}) as CommandContext);
    expect(isSettingsOpen()).toBe(true);
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });
});

// Capability gating: a command whose RPCs ride an execute-tier scope is
// DISABLED without the grant — absent from the palette, chord falling
// through — rather than running and reporting a refusal.
describe('capability-gated commands', () => {
  afterEach(() => {
    resetToLocalPage();
  });

  it('enables the create/git/terminal commands on the local page', () => {
    const pane = readyPane();
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(isCommandEnabled('thread.new', ctx)).toBe(true);
    expect(isCommandEnabled('thread.delete', ctx)).toBe(true);
    expect(isCommandEnabled('git.push', ctx)).toBe(true);
    expect(isCommandEnabled('terminal.new', ctx)).toBe(true);
  });

  it('disables them for a view-only session', async () => {
    await pairViewOnly();
    const pane = readyPane();
    const ctx = makeCommandContext(pane, {}) as CommandContext;
    expect(ctx.flags.threadsOperate).toBe(false);
    expect(ctx.flags.gitOperate).toBe(false);
    expect(ctx.flags.terminalOperate).toBe(false);
    expect(isCommandEnabled('thread.new', ctx)).toBe(false);
    expect(isCommandEnabled('thread.newPane', ctx)).toBe(false);
    expect(isCommandEnabled('thread.new.fromPR', ctx)).toBe(false);
    expect(isCommandEnabled('thread.delete', ctx)).toBe(false);
    expect(isCommandEnabled('thread.fork', ctx)).toBe(false);
    expect(isCommandEnabled('git.commit', ctx)).toBe(false);
    expect(isCommandEnabled('git.push', ctx)).toBe(false);
    expect(isCommandEnabled('git.ship', ctx)).toBe(false);
    expect(isCommandEnabled('terminal.new', ctx)).toBe(false);
    expect(isCommandEnabled('terminal.toggle', ctx)).toBe(false);
  });

  // The xterm escape predicate evaluates `when` against a synthetic context
  // carrying only terminalFocus, so a capability term on a tab command would
  // trap the chord in the PTY for everybody. terminal.newPane keeps its
  // escape arm un-gated for the same reason.
  it('keeps the terminal-escape commands enabled under the focused-only context', () => {
    const focusedOnly = { flags: { terminalFocus: true } } as unknown as CommandContext;
    for (const id of TERMINAL_ESCAPE_COMMAND_IDS) {
      expect(isCommandEnabled(id, focusedOnly)).toBe(true);
    }
  });
});
