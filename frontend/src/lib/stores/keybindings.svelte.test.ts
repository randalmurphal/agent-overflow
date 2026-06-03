import { describe, expect, it, beforeEach, vi } from 'vitest';
import {
  dispatchKey,
  keybindingForCommand,
  loadKeybindings,
  resetKeybindingsStore,
  setKeybindingsForTest,
  saveKeybindings,
  getKeybindingIssues,
  resetKeybindingsToDefaults,
  formatChord,
  eventMatchesKeybindingCommand,
  eventEscapesTerminalToCommand,
} from './keybindings.svelte';
import {
  clearCommandRegistry,
  registerCommand,
  type CommandContext,
} from './commandRegistry.svelte';
import { PANE_NAV_COMMAND_IDS } from './paneNavCommands';
import { setBindingMock } from '../../test/mocks/bindings-app';

function baseCtx(extra: Partial<CommandContext> = {}): CommandContext {
  return {
    paletteOpen: false,
    terminalOpen: false,
    terminalFocus: false,
    approvalPending: false,
    anyModalOpen: false,
    hasActiveThread: false,
    turnActive: false,
    sendInFlight: false,
    hasPendingPrompt: false,
    canForkActiveThread: false,
    canStartDiscussion: false,
    ...extra,
  } as CommandContext;
}

function ev(key: string, mods: Partial<KeyboardEvent> = {}): KeyboardEvent {
  // happy-dom provides KeyboardEvent; Object.assign flips the modifier flags.
  const e = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  Object.defineProperty(e, 'metaKey', { value: mods.metaKey ?? false });
  Object.defineProperty(e, 'ctrlKey', { value: mods.ctrlKey ?? false });
  Object.defineProperty(e, 'shiftKey', { value: mods.shiftKey ?? false });
  Object.defineProperty(e, 'altKey', { value: mods.altKey ?? false });
  return e;
}

describe('keybindings store — dispatch', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
  });

  it('dispatches chord → registered command', () => {
    const run = vi.fn();
    registerCommand({ id: 'palette.open', label: 'Open Palette', run });
    setKeybindingsForTest([{ key: 'mod+k', command: 'palette.open' }]);

    const handled = dispatchKey(ev('k', { metaKey: true }), baseCtx(), { isMac: true });
    expect(handled).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('returns false when no rule matches', () => {
    registerCommand({ id: 'palette.open', label: 'Open Palette', run: vi.fn() });
    setKeybindingsForTest([{ key: 'mod+k', command: 'palette.open' }]);
    const handled = dispatchKey(ev('x', { metaKey: true }), baseCtx(), { isMac: true });
    expect(handled).toBe(false);
  });

  it('respects keybinding-level `when` expression', () => {
    const run = vi.fn();
    registerCommand({ id: 'terminal.close', label: 'Close', run });
    setKeybindingsForTest([
      { key: 'mod+w', command: 'terminal.close', when: 'terminalFocus' },
    ]);
    expect(
      dispatchKey(ev('w', { metaKey: true }), baseCtx({ terminalFocus: false }), { isMac: true }),
    ).toBe(false);
    expect(run).not.toHaveBeenCalled();

    expect(
      dispatchKey(ev('w', { metaKey: true }), baseCtx({ terminalFocus: true }), { isMac: true }),
    ).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('multiple rules with the same chord — later matching rule wins', () => {
    const runA = vi.fn();
    const runB = vi.fn();
    registerCommand({ id: 'a', label: 'A', run: runA });
    registerCommand({ id: 'b', label: 'B', run: runB });
    setKeybindingsForTest([
      { key: 'mod+n', command: 'a', when: 'terminalFocus' },
      { key: 'mod+n', command: 'b', when: '!terminalFocus' },
    ]);
    dispatchKey(ev('n', { metaKey: true }), baseCtx({ terminalFocus: true }), { isMac: true });
    expect(runA).toHaveBeenCalledTimes(1);
    expect(runB).not.toHaveBeenCalled();

    runA.mockClear();
    runB.mockClear();
    dispatchKey(ev('n', { metaKey: true }), baseCtx({ terminalFocus: false }), { isMac: true });
    expect(runB).toHaveBeenCalledTimes(1);
    expect(runA).not.toHaveBeenCalled();
  });

  it('command-level when gate is respected when the keybinding when passes', () => {
    const run = vi.fn();
    registerCommand({ id: 'cmd', label: 'Cmd', when: 'hasActiveThread', run });
    setKeybindingsForTest([{ key: 'mod+x', command: 'cmd' }]);
    expect(dispatchKey(ev('x', { metaKey: true }), baseCtx(), { isMac: true })).toBe(false);
    expect(run).not.toHaveBeenCalled();

    expect(
      dispatchKey(ev('x', { metaKey: true }), baseCtx({ hasActiveThread: true }), { isMac: true }),
    ).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('does not dispatch pane navigation commands while terminal focus is active', () => {
    const run = vi.fn();
    registerCommand({ id: 'pane.focusLeft', label: 'Pane Left', when: '!terminalFocus', run });
    setKeybindingsForTest([{ key: 'alt+h', command: 'pane.focusLeft', when: '!terminalFocus' }]);

    expect(dispatchKey(ev('h', { altKey: true }), baseCtx({ terminalFocus: true }), { isMac: true }))
      .toBe(false);
    expect(run).not.toHaveBeenCalled();

    expect(dispatchKey(ev('h', { altKey: true }), baseCtx({ terminalFocus: false }), { isMac: true }))
      .toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('keybindingForCommand returns the last-registered chord', () => {
    setKeybindingsForTest([
      { key: 'mod+k', command: 'palette.open' },
      { key: 'mod+shift+p', command: 'palette.open' },
    ]);
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+p');
  });

  it('detects whether an event matches an allowed command binding without running it', () => {
    const run = vi.fn();
    registerCommand({ id: 'thread.jump.2', label: 'Jump 2', run });
    setKeybindingsForTest([{ key: 'ctrl+alt+2', command: 'thread.jump.2' }]);

    const allowed = new Set(['thread.jump.2']);
    expect(
      eventMatchesKeybindingCommand(ev('2', { ctrlKey: true, altKey: true }), baseCtx(), allowed, { isMac: false }),
    ).toBe(true);
    expect(run).not.toHaveBeenCalled();
  });

  it('does not allow disabled commands through editable preflight', () => {
    registerCommand({ id: 'thread.jump.2', label: 'Jump 2', when: 'hasActiveThread', run: vi.fn() });
    setKeybindingsForTest([{ key: 'mod+2', command: 'thread.jump.2' }]);

    const allowed = new Set(['thread.jump.2']);
    expect(
      eventMatchesKeybindingCommand(ev('2', { ctrlKey: true }), baseCtx({ hasActiveThread: false }), allowed, { isMac: false }),
    ).toBe(false);
  });
});

describe('eventEscapesTerminalToCommand (terminal key-escape predicate)', () => {
  // Mirrors the real defaults' asymmetry: the vim chord is un-gated (escapes a
  // focused terminal to drive pane nav) while its arrow twin keeps the
  // !terminalFocus rule-gate (stays in the shell as word-motion).
  const PANE_NAV_RULES = [
    { key: 'alt+h', command: 'pane.focusLeft' },
    { key: 'alt+arrowleft', command: 'pane.focusLeft', when: '!terminalFocus' },
  ];

  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
    registerCommand({ id: 'pane.focusLeft', label: 'Pane Left', run: vi.fn() });
  });

  it('lets an un-gated vim chord escape the terminal', () => {
    setKeybindingsForTest(PANE_NAV_RULES);
    expect(
      eventEscapesTerminalToCommand(ev('h', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(true);
  });

  it('keeps a !terminalFocus-gated arrow chord in the shell', () => {
    setKeybindingsForTest(PANE_NAV_RULES);
    expect(
      eventEscapesTerminalToCommand(ev('ArrowLeft', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(false);
  });

  it('ignores keys not bound to a pane-nav command', () => {
    setKeybindingsForTest(PANE_NAV_RULES);
    expect(
      eventEscapesTerminalToCommand(ev('x', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(false);
  });

  it('does not escape a chord bound to a non-pane-nav command, even un-gated', () => {
    registerCommand({ id: 'other.cmd', label: 'Other', run: vi.fn() });
    setKeybindingsForTest([{ key: 'alt+h', command: 'other.cmd' }]);
    expect(
      eventEscapesTerminalToCommand(ev('h', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(false);
  });
});

describe('keybindings store — loading', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
  });

  it('loadKeybindings calls the backend and compiles the rules', async () => {
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+k', command: 'palette.open' },
      { key: 'garbage garbage', command: 'bad' },
    ]);
    await loadKeybindings();
    expect(getKeybindingIssues()).toHaveLength(1);
    expect(getKeybindingIssues()[0]?.rule.command).toBe('bad');
  });

  it('saveKeybindings persists then reloads', async () => {
    setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => [{ key: 'mod+o', command: 'palette.open' }]);
    await saveKeybindings([{ key: 'mod+o', command: 'palette.open' }]);
    expect(keybindingForCommand('palette.open')).toBe('mod+o');
  });

  it('saveKeybindings preserves default binding identity', async () => {
    const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => [
      {
        key: 'mod+x',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.alternate',
        defaultKey: 'mod+shift+o',
      },
    ]);

    await saveKeybindings([
      {
        key: 'mod+x',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.alternate',
        defaultKey: 'mod+shift+o',
      },
    ]);

    expect(update).toHaveBeenCalledTimes(1);
    expect(update.mock.calls[0]?.[0]).toMatchObject([
      {
        key: 'mod+x',
        command: 'thread.new',
        when: '!terminalFocus',
        defaultId: 'thread.new.alternate',
        defaultKey: 'mod+shift+o',
      },
    ]);
  });

  it('resetKeybindingsToDefaults triggers ResetKeybindings then reloads', async () => {
    const reset = vi.fn(async () => {});
    setBindingMock('ResetKeybindings', reset);
    setBindingMock('GetKeybindings', async () => [{ key: 'mod+k', command: 'palette.open' }]);
    await resetKeybindingsToDefaults();
    expect(reset).toHaveBeenCalledTimes(1);
    expect(keybindingForCommand('palette.open')).toBe('mod+k');
  });
});

describe('keybindings store — display formatting', () => {
  it('formats mod as Ctrl on non-macOS hosts', () => {
    expect(formatChord('mod+k', false)).toBe('Ctrl+K');
    expect(formatChord('mod+shift+g', false)).toBe('Ctrl+Shift+G');
  });

  it('formats mod as Command on macOS hosts', () => {
    expect(formatChord('mod+k', true)).toBe('⌘K');
    expect(formatChord('mod+shift+g', true)).toBe('⇧⌘G');
  });
});
