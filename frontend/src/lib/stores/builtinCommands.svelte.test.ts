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
