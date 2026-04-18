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
    interactionMode: 'default',
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
