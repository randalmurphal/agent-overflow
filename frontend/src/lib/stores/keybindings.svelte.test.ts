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
} from './keybindings.svelte';
import {
  clearCommandRegistry,
  registerCommand,
  type CommandContext,
} from './commandRegistry.svelte';
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

  it('keybindingForCommand returns the last-registered chord', () => {
    setKeybindingsForTest([
      { key: 'mod+k', command: 'palette.open' },
      { key: 'mod+shift+p', command: 'palette.open' },
    ]);
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+p');
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
