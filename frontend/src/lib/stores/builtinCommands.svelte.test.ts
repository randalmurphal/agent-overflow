// Focused tests for makeCommandContext — keeps palette / keybindings gates
// honest about the live pane / terminal-focus state.

import { describe, expect, it, beforeEach } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
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
