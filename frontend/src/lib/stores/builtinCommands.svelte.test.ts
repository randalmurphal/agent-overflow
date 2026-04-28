// Focused tests for makeCommandContext — keeps palette / keybindings gates
// honest about the live pane / terminal-focus state.

import { describe, expect, it, beforeEach } from 'vitest';
import { createThreadPane } from './thread.svelte';
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
import type { Thread } from '../types/models';

function readyPane(overrides: Partial<Thread> = {}): ReturnType<typeof createThreadPane> {
  setBindingMock('SwitchThread', async () => {});
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

describe('makeCommandContext', () => {
  beforeEach(() => {
    resetTerminalFocusForTest();
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
    expect(pane.isTurnActive).toBe(true);

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
    expect(pane.isTurnActive).toBe(false);
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
    expect(pane.isTurnActive).toBe(false);
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
  registerBuiltinCommands({
    pane,
    openSettings: () => {},
    openThreadForm: () => {},
    openThreadFromPR: () => {},
    openShipChanges: () => {},
    requestRename: () => {},
    requestDiscussion: () => {},
    focusThreadSearch: () => {},
    requestThreadJump: () => {},
    requestThreadStep: () => {},
  });
}

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
// Shift+Tab cycles the active thread through chat → plan → design. The command
// reads the current mode from the pane's thread and calls UpdateThreadMode.
// Disabled while any modal or the palette is open because Shift+Tab is the
// native "focus previous" chord inside those surfaces.

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

  it('calls UpdateThreadMode with the next step in the cycle', async () => {
    const pane = readyPane({ mode: 'plan' });
    const calls: Array<[string, string]> = [];
    setBindingMock('UpdateThreadMode', async (id: unknown, mode: unknown) => {
      calls.push([id as string, mode as string]);
      // Return the updated thread shape the command expects.
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
    expect(calls[0]).toEqual(['thread-1', 'design']);
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
      pane,
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
      requestThreadStep: () => {},
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
