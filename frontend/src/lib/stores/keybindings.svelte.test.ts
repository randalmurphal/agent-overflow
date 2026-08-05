import { afterEach, describe, expect, it, beforeEach, vi } from 'vitest';
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
  encodeChordFromEvent,
  eventMatchesKeybindingCommand,
  eventEscapesTerminalToCommand,
  findDuplicateChordRow,
  isKeybindingCaptureTarget,
  chordHintForCommand,
  chordHintSuffix,
  getKeybindingRules,
  getResolvedKeybindings,
  isUnboundChord,
  withReboundRow,
  KEYBINDING_CAPTURE_ATTR,
  UNBOUND_CHORD,
  type KeybindingRule,
} from './keybindings.svelte';
import {
  clearCommandRegistry,
  registerCommand,
  type CommandContext,
} from './commandRegistry.svelte';
import { PANE_NAV_COMMAND_IDS, TERMINAL_ESCAPE_COMMAND_IDS } from './paneNavCommands';
import { setBindingMock } from '../../test/mocks/bindings-app';

type TestKeyMods = {
  code?: string;
  metaKey?: boolean;
  ctrlKey?: boolean;
  shiftKey?: boolean;
  altKey?: boolean;
};

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

function ev(key: string, mods: TestKeyMods = {}): KeyboardEvent {
  // happy-dom provides KeyboardEvent; define readonly keyboard fields directly.
  const e = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  Object.defineProperty(e, 'code', { value: mods.code ?? '' });
  Object.defineProperty(e, 'metaKey', { value: mods.metaKey ?? false });
  Object.defineProperty(e, 'ctrlKey', { value: mods.ctrlKey ?? false });
  Object.defineProperty(e, 'shiftKey', { value: mods.shiftKey ?? false });
  Object.defineProperty(e, 'altKey', { value: mods.altKey ?? false });
  return e;
}

// Dispatch the chord from a real node so `event.target` is the node the
// browser would report — the guard's whole input.
function dispatchFromNode(
  node: Element,
  key: string,
  mods: TestKeyMods = {},
  ctx: CommandContext = baseCtx(),
): boolean {
  const event = ev(key, mods);
  let handled = false;
  const listener = (e: Event) => {
    handled = dispatchKey(e as KeyboardEvent, ctx, { isMac: false });
  };
  window.addEventListener('keydown', listener);
  try {
    node.dispatchEvent(event);
  } finally {
    window.removeEventListener('keydown', listener);
  }
  return handled;
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

  it('dispatches macOS Option-letter chords when event.key is the produced glyph', () => {
    const run = vi.fn();
    registerCommand({ id: 'pane.focusLeft', label: 'Pane Left', run });
    setKeybindingsForTest([{ key: 'alt+h', command: 'pane.focusLeft' }]);

    expect(dispatchKey(ev('˙', { code: 'KeyH', altKey: true }), baseCtx(), { isMac: true }))
      .toBe(true);
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
  // Mirrors the configurable terminal escape rule: an un-gated pane-nav chord
  // escapes a focused terminal, while a user-bound chord still gated on
  // !terminalFocus stays in the shell as word-motion.
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

  it('lets macOS Option-letter glyph events escape the terminal for un-gated pane-nav chords', () => {
    registerCommand({ id: 'pane.focusRight', label: 'Pane Right', run: vi.fn() });
    registerCommand({ id: 'pane.moveRight', label: 'Move Right', run: vi.fn() });
    setKeybindingsForTest([
      { key: 'alt+h', command: 'pane.focusLeft' },
      { key: 'alt+l', command: 'pane.focusRight' },
      { key: 'alt+shift+l', command: 'pane.moveRight' },
    ]);

    const escapes = (event: KeyboardEvent): boolean =>
      eventEscapesTerminalToCommand(event, PANE_NAV_COMMAND_IDS, { isMac: true });
    expect(escapes(ev('˙', { code: 'KeyH', altKey: true }))).toBe(true);
    expect(escapes(ev('¬', { code: 'KeyL', altKey: true }))).toBe(true);
    expect(escapes(ev('Ò', { code: 'KeyL', altKey: true, shiftKey: true }))).toBe(true);
  });

  it('keeps a !terminalFocus-gated arrow chord in the shell', () => {
    setKeybindingsForTest(PANE_NAV_RULES);
    expect(
      eventEscapesTerminalToCommand(ev('ArrowLeft', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(false);
  });

  it('lets a settings-shaped un-gated arrow rebind escape the terminal', () => {
    setKeybindingsForTest([
      {
        key: 'alt+arrowleft',
        command: 'pane.focusLeft',
        defaultId: 'pane.focusLeft.vim',
        defaultKey: 'alt+h',
      },
    ]);
    expect(
      eventEscapesTerminalToCommand(ev('ArrowLeft', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(true);
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

  // terminal.refresh (alt+shift+r) must escape a focused terminal the same way
  // the pane-nav chords do, so the in-terminal repaint press reaches the app
  // instead of being encoded to the PTY as a meta sequence. This needs BOTH the
  // escape-set membership AND a command `when` that holds under the synthetic
  // terminalFocus-only context the predicate evaluates against; the next three
  // tests pin each half so a regression in either fails loudly.
  function registerTerminalRefresh(when = 'terminalFocus || terminalOpen'): void {
    registerCommand({
      id: 'terminal.refresh',
      label: 'Refresh',
      when,
      editableReachable: true,
      run: vi.fn(),
    });
    setKeybindingsForTest([
      { key: 'alt+shift+r', command: 'terminal.refresh', when: 'terminalFocus' },
    ]);
  }

  it('lets alt+shift+r escape the terminal via the terminal-escape set', () => {
    registerTerminalRefresh();
    expect(
      eventEscapesTerminalToCommand(
        ev('r', { altKey: true, shiftKey: true }),
        TERMINAL_ESCAPE_COMMAND_IDS,
        { isMac: false },
      ),
    ).toBe(true);
  });

  it('would NOT escape alt+shift+r through the pane-nav-only set — escape-set entry is load-bearing', () => {
    registerTerminalRefresh();
    expect(
      eventEscapesTerminalToCommand(
        ev('r', { altKey: true, shiftKey: true }),
        PANE_NAV_COMMAND_IDS,
        { isMac: false },
      ),
    ).toBe(false);
  });

  it('requires the command `when` to hold under the synthetic terminalFocus ctx — bare terminalOpen would not escape', () => {
    // The predicate evaluates against `{ terminalFocus: true }` only, so a
    // command gated solely on `terminalOpen` evaluates false there and stays in
    // the PTY. This is exactly why the real command uses `terminalFocus || ...`.
    registerTerminalRefresh('terminalOpen');
    expect(
      eventEscapesTerminalToCommand(
        ev('r', { altKey: true, shiftKey: true }),
        TERMINAL_ESCAPE_COMMAND_IDS,
        { isMac: false },
      ),
    ).toBe(false);
  });

  // Terminal tab/pane management chords must escape a focused terminal too. The
  // tab chords are terminalFocus-gated (enabled under the synthetic ctx, so they
  // escape); newPane is un-gated (escapes regardless, like the vim chords). Note
  // ctrl+tab is LITERAL ctrl, not mod, so it must NOT be normalized to Cmd+Tab.
  function registerTerminalManagement(): void {
    for (const id of ['terminal.newTab', 'terminal.closeTab', 'terminal.nextTab', 'terminal.prevTab']) {
      registerCommand({
        id,
        label: id,
        when: 'terminalFocus || terminalOpen',
        editableReachable: true,
        run: vi.fn(),
      });
    }
    registerCommand({ id: 'terminal.newPane', label: 'New Pane', editableReachable: true, run: vi.fn() });
    setKeybindingsForTest([
      { key: 'mod+shift+t', command: 'terminal.newTab', when: 'terminalFocus' },
      { key: 'mod+shift+w', command: 'terminal.closeTab', when: 'terminalFocus' },
      { key: 'ctrl+tab', command: 'terminal.nextTab', when: 'terminalFocus' },
      { key: 'ctrl+shift+tab', command: 'terminal.prevTab', when: 'terminalFocus' },
      { key: 'mod+shift+~', command: 'terminal.newPane' },
    ]);
  }

  it('lets the tab/pane management chords escape via the terminal-escape set', () => {
    registerTerminalManagement();
    const escapes = (e: KeyboardEvent): boolean =>
      eventEscapesTerminalToCommand(e, TERMINAL_ESCAPE_COMMAND_IDS, { isMac: false });
    expect(escapes(ev('t', { ctrlKey: true, shiftKey: true }))).toBe(true); // newTab
    expect(escapes(ev('w', { ctrlKey: true, shiftKey: true }))).toBe(true); // closeTab
    expect(escapes(ev('Tab', { ctrlKey: true }))).toBe(true); // nextTab
    expect(escapes(ev('Tab', { ctrlKey: true, shiftKey: true }))).toBe(true); // prevTab
    expect(escapes(ev('~', { ctrlKey: true, shiftKey: true }))).toBe(true); // newPane (un-gated)
    // Plain Tab carries no ctrl, so it matches no escape chord and stays with the
    // shell for completion — the ctrl+tab binding must not steal bare Tab.
    expect(escapes(ev('Tab', {}))).toBe(false);
  });

  it('newPane escapes a focused mac terminal through the WKWebView Cmd+Shift shape', () => {
    registerTerminalManagement();
    // Cmd+Shift stripping delivers key '`' — the escape check must still
    // recognize the mod+shift+~ chord so the pane opens from inside an xterm.
    expect(
      eventEscapesTerminalToCommand(
        ev('`', { metaKey: true, shiftKey: true, code: 'Backquote' }),
        TERMINAL_ESCAPE_COMMAND_IDS,
        { isMac: true },
      ),
    ).toBe(true);
  });

  it('would NOT escape the tab chords through the pane-nav-only set — escape-set entry is load-bearing', () => {
    registerTerminalManagement();
    expect(
      eventEscapesTerminalToCommand(
        ev('t', { ctrlKey: true, shiftKey: true }),
        PANE_NAV_COMMAND_IDS,
        { isMac: false },
      ),
    ).toBe(false);
  });

  // On Chromium platforms mod+shift+` produces event.key '~' (the shifted
  // glyph), which is why the default binding is spelled `mod+shift+~`. On
  // macOS WebKit the layout's Cmd table strips Shift, so Cmd+Shift+` arrives
  // as event.key '`' and matches via the code-based fallback. Pin the full
  // dispatch for all three shapes so neither the glyph spelling, the mod
  // resolution, nor the fallback regresses.
  it('dispatches mod+shift+~ to terminal.newPane on both platforms', () => {
    const run = vi.fn();
    registerCommand({ id: 'terminal.newPane', label: 'New Pane', editableReachable: true, run });
    setKeybindingsForTest([{ key: 'mod+shift+~', command: 'terminal.newPane' }]);

    // Windows/Linux: Ctrl+Shift+` → key '~'.
    expect(dispatchKey(ev('~', { ctrlKey: true, shiftKey: true, code: 'Backquote' }), baseCtx(), { isMac: false })).toBe(true);
    // macOS Chromium shape: Cmd+Shift+` → key '~'.
    expect(dispatchKey(ev('~', { metaKey: true, shiftKey: true, code: 'Backquote' }), baseCtx(), { isMac: true })).toBe(true);
    // macOS WKWebView shape: Cmd+Shift stripping → key '`', code Backquote.
    expect(dispatchKey(ev('`', { metaKey: true, shiftKey: true, code: 'Backquote' }), baseCtx(), { isMac: true })).toBe(true);
    expect(run).toHaveBeenCalledTimes(3);

    // The bare backtick (shift not producing '~', or the unshifted key) must
    // not fire it.
    expect(dispatchKey(ev('`', { ctrlKey: true, code: 'Backquote' }), baseCtx(), { isMac: false })).toBe(false);
    expect(run).toHaveBeenCalledTimes(3);
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

describe('keybindings store — chord capture', () => {
  it('captures macOS Option-letter chords by physical key instead of the produced glyph', () => {
    expect(encodeChordFromEvent(ev('˙', { code: 'KeyH', altKey: true }), true)).toBe('alt+h');
    expect(encodeChordFromEvent(ev('¬', { code: 'KeyL', altKey: true }), true)).toBe('alt+l');
    expect(encodeChordFromEvent(ev('Ò', { code: 'KeyL', altKey: true, shiftKey: true }), true))
      .toBe('alt+shift+l');
  });

  it('does not normalize Option-letter produced glyphs off macOS', () => {
    expect(encodeChordFromEvent(ev('Ò', { code: 'KeyL', altKey: true, shiftKey: true }), false))
      .not.toBe('alt+shift+l');
  });

  it('captures macOS Cmd+Shift punctuation as the shifted glyph despite Cmd stripping', () => {
    // WKWebView delivers key '`' for Cmd+Shift+` (the Cmd table strips
    // Shift); the captured chord must be the portable shifted-glyph
    // spelling, matching what Chromium platforms report natively.
    expect(encodeChordFromEvent(ev('`', { code: 'Backquote', metaKey: true, shiftKey: true }), true))
      .toBe('mod+shift+~');
    // Letters are untouched — no table entry, key is already correct.
    expect(encodeChordFromEvent(ev('n', { code: 'KeyN', metaKey: true, shiftKey: true }), true))
      .toBe('mod+shift+n');
    // Off macOS the glyph arrives correctly and needs no normalization.
    expect(encodeChordFromEvent(ev('~', { code: 'Backquote', ctrlKey: true, shiftKey: true }), false))
      .toBe('mod+shift+~');
  });
});

describe('keybindings store — duplicate chord detection', () => {
  // A command can ship two default rows (thread.new is bound to both mod+n
  // and mod+shift+o). Rebinding one onto the other's chord would leave two
  // rows of the same command showing the same chord — indistinguishable in
  // settings, and only the first reachable at dispatch.
  const primary = {
    key: 'mod+n',
    command: 'thread.new',
    when: '!terminalFocus',
    defaultId: 'thread.new.primary',
    defaultKey: 'mod+n',
  };
  const alternate = {
    key: 'mod+shift+o',
    command: 'thread.new',
    when: '!terminalFocus',
    defaultId: 'thread.new.alternate',
    defaultKey: 'mod+shift+o',
  };
  const rows = [primary, alternate];

  it('reports the sibling row a rebind would collide with', () => {
    expect(findDuplicateChordRow(rows, primary, 'mod+shift+o')).toBe(alternate);
    expect(findDuplicateChordRow(rows, alternate, 'mod+n')).toBe(primary);
  });

  it('compares chords canonically, not textually', () => {
    expect(findDuplicateChordRow(rows, primary, 'Shift+Mod+O')).toBe(alternate);
  });

  it('allows a free chord', () => {
    expect(findDuplicateChordRow(rows, primary, 'mod+x')).toBeNull();
  });

  it('is not a self-collision when a row re-captures its own chord', () => {
    expect(findDuplicateChordRow(rows, primary, 'mod+n')).toBeNull();
  });

  it('ignores rows of another command or another when-context', () => {
    const otherCommand = { ...alternate, command: 'thread.newPane', defaultId: 'thread.newPane' };
    expect(findDuplicateChordRow([primary, otherCommand], primary, 'mod+shift+o')).toBeNull();
    const otherContext = { ...alternate, when: 'terminalFocus' };
    expect(findDuplicateChordRow([primary, otherContext], primary, 'mod+shift+o')).toBeNull();
  });

  it('matches legacy rows that carry no defaultId', () => {
    const legacyPrimary = { key: 'mod+n', command: 'thread.new', when: '!terminalFocus' };
    const legacyAlternate = { key: 'mod+shift+o', command: 'thread.new', when: '!terminalFocus' };
    expect(findDuplicateChordRow([legacyPrimary, legacyAlternate], legacyPrimary, 'mod+shift+o'))
      .toBe(legacyAlternate);
  });
});

// --- the unbound state ---
//
// Absent means "use the shipped default"; UNBOUND_CHORD means "the user
// cleared it". These pin that the two never collapse into each other, and
// they exercise SEQUENCES (bind → unbind → rebind, unbind → reset, unbind
// across a store reload), because a state-only pass would miss an unbind
// that fails to clear what a previous bind stored.
describe('keybindings store — unbound bindings', () => {
  const PALETTE_ROW: KeybindingRule = {
    key: 'mod+shift+k',
    command: 'palette.open',
    defaultId: 'palette.open',
    defaultKey: 'mod+shift+k',
  };

  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
  });

  it('classifies keys: empty and whitespace are unbound, a chord is not', () => {
    expect(isUnboundChord(UNBOUND_CHORD)).toBe(true);
    expect(isUnboundChord('   ')).toBe(true);
    expect(isUnboundChord(null)).toBe(true);
    expect(isUnboundChord(undefined)).toBe(true);
    expect(isUnboundChord('mod+k')).toBe(false);
  });

  it('compiles no dispatchable rule and reports no issue for an unbound row', () => {
    setKeybindingsForTest([{ ...PALETTE_ROW, key: UNBOUND_CHORD }]);
    expect(getResolvedKeybindings()).toHaveLength(0);
    // NOT an issue — the user asked for this. An unparseable chord still is.
    expect(getKeybindingIssues()).toHaveLength(0);
    // The row itself survives so settings can offer the default back.
    expect(getKeybindingRules()).toHaveLength(1);
  });

  it('keeps reporting a genuinely broken chord as an issue', () => {
    setKeybindingsForTest([{ ...PALETTE_ROW, key: 'garbage garbage' }]);
    expect(getKeybindingIssues()).toHaveLength(1);
  });

  it('does not fire the default chord for an unbound command', () => {
    const run = vi.fn();
    registerCommand({ id: 'palette.open', label: 'Open Palette', run });

    setKeybindingsForTest([PALETTE_ROW]);
    expect(dispatchKey(ev('k', { ctrlKey: true, shiftKey: true }), baseCtx(), { isMac: false }))
      .toBe(true);
    expect(run).toHaveBeenCalledTimes(1);

    setKeybindingsForTest([{ ...PALETTE_ROW, key: UNBOUND_CHORD }]);
    expect(dispatchKey(ev('k', { ctrlKey: true, shiftKey: true }), baseCtx(), { isMac: false }))
      .toBe(false);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('reports no chord instead of falling back to the default', () => {
    setKeybindingsForTest([{ ...PALETTE_ROW, key: UNBOUND_CHORD }]);
    expect(keybindingForCommand('palette.open')).toBeNull();
    expect(chordHintForCommand('palette.open')).toBeNull();
    expect(chordHintSuffix('palette.open')).toBe('');
  });

  it('formats a hint suffix for a bound command', () => {
    setKeybindingsForTest([PALETTE_ROW]);
    expect(chordHintSuffix('palette.open', false)).toBe(' (Ctrl+Shift+K)');
    expect(chordHintForCommand('palette.open', false)).toBe('Ctrl+Shift+K');
  });

  it('leaves a sibling row of the same command bound when one row is cleared', () => {
    // thread.new ships two default rows; clearing one must not silence the
    // command, and the hint must show the row that is still bound.
    const run = vi.fn();
    registerCommand({ id: 'thread.new', label: 'New Thread', run });
    setKeybindingsForTest([
      { key: 'mod+n', command: 'thread.new', defaultId: 'thread.new.primary', defaultKey: 'mod+n' },
      { key: UNBOUND_CHORD, command: 'thread.new', defaultId: 'thread.new.alternate', defaultKey: 'mod+shift+o' },
    ]);
    expect(keybindingForCommand('thread.new')).toBe('mod+n');
    expect(dispatchKey(ev('n', { ctrlKey: true }), baseCtx(), { isMac: false })).toBe(true);
    expect(dispatchKey(ev('o', { ctrlKey: true, shiftKey: true }), baseCtx(), { isMac: false }))
      .toBe(false);
  });

  it('does not let an unbound row escape a focused terminal', () => {
    registerCommand({ id: 'pane.focusLeft', label: 'Pane Left', run: vi.fn() });
    setKeybindingsForTest([
      { key: UNBOUND_CHORD, command: 'pane.focusLeft', defaultId: 'pane.focusLeft.vim', defaultKey: 'alt+h' },
    ]);
    expect(
      eventEscapesTerminalToCommand(ev('h', { altKey: true }), PANE_NAV_COMMAND_IDS, { isMac: false }),
    ).toBe(false);
  });

  it('never reports a collision for a clear', () => {
    // Two unbound rows are not "indistinguishable in the table" the way two
    // rows sharing a real chord are — neither is reachable.
    const alternate = { ...PALETTE_ROW, key: UNBOUND_CHORD, command: 'thread.new', defaultId: 'a' };
    const primary = { ...PALETTE_ROW, key: UNBOUND_CHORD, command: 'thread.new', defaultId: 'b' };
    expect(findDuplicateChordRow([primary, alternate], primary, UNBOUND_CHORD)).toBeNull();
    // And an unbound row never blocks a rebind onto a real chord.
    expect(findDuplicateChordRow([primary, alternate], primary, 'mod+z')).toBeNull();
  });

  // --- transitions, not states ---

  it('survives bind → unbind → rebind through the persisted round-trip', async () => {
    let stored: KeybindingRule[] = [];
    setBindingMock('UpdateKeybindings', async (payload: KeybindingRule[]) => {
      stored = payload.map((r) => ({ ...r }));
    });
    // Model the Go merge closely enough for the sequence: the default row
    // unless an override for its identity was stored.
    setBindingMock('GetKeybindings', async () =>
      [stored.find((r) => r.defaultId === 'palette.open') ?? PALETTE_ROW]
        .map((r) => ({ ...r, defaultId: 'palette.open', defaultKey: 'mod+shift+k' })));

    const current = (): KeybindingRule => getKeybindingRules()[0];

    await loadKeybindings();
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+k');

    // bind
    await saveKeybindings(withReboundRow(getKeybindingRules(), current(), 'mod+o'));
    expect(keybindingForCommand('palette.open')).toBe('mod+o');

    // unbind — the previous bind's chord must be gone AND must not fall back
    await saveKeybindings(withReboundRow(getKeybindingRules(), current(), UNBOUND_CHORD));
    expect(keybindingForCommand('palette.open')).toBeNull();
    expect(stored).toMatchObject([{ key: UNBOUND_CHORD, defaultId: 'palette.open' }]);

    // rebind out of unbound
    await saveKeybindings(withReboundRow(getKeybindingRules(), current(), 'mod+shift+z'));
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+z');

    // unbind again, then restore the default by writing the default chord —
    // the override drops out of the payload entirely.
    await saveKeybindings(withReboundRow(getKeybindingRules(), current(), UNBOUND_CHORD));
    expect(keybindingForCommand('palette.open')).toBeNull();
    await saveKeybindings(withReboundRow(getKeybindingRules(), current(), 'mod+shift+k'));
    expect(stored).toEqual([]);
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+k');
  });

  it('survives unbind → reset-to-defaults', async () => {
    setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => [{ ...PALETTE_ROW, key: UNBOUND_CHORD }]);
    await loadKeybindings();
    expect(keybindingForCommand('palette.open')).toBeNull();

    setBindingMock('ResetKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => [PALETTE_ROW]);
    await resetKeybindingsToDefaults();
    expect(keybindingForCommand('palette.open')).toBe('mod+shift+k');
  });

  it('an unbind persisted before a reload is still unbound after it', async () => {
    setBindingMock('GetKeybindings', async () => [{ ...PALETTE_ROW, key: UNBOUND_CHORD }]);
    await loadKeybindings();
    expect(keybindingForCommand('palette.open')).toBeNull();
    // Reload from the same backing store — "absent" must not creep back in.
    await loadKeybindings();
    expect(keybindingForCommand('palette.open')).toBeNull();
    expect(getKeybindingRules()).toHaveLength(1);
  });

  it('persists a clear but never an identity-less one', async () => {
    const update = setBindingMock('UpdateKeybindings', vi.fn(async () => {}));
    setBindingMock('GetKeybindings', async () => []);

    await saveKeybindings([
      // Cleared default row: a real override, must be written.
      { ...PALETTE_ROW, key: UNBOUND_CHORD },
      // Unchanged default: nothing to persist.
      { key: 'mod+b', command: 'sidebar.toggle', defaultId: 'sidebar.toggle', defaultKey: 'mod+b' },
      // Unbound with no default to clear: it silences nothing, and the
      // backend rejects it — dropping it here is what keeps a dropped chord
      // from reaching Update as a deliberate clear.
      { key: UNBOUND_CHORD, command: 'orphan.command' },
    ]);

    expect(update.mock.calls[0]?.[0]).toMatchObject([
      { key: UNBOUND_CHORD, command: 'palette.open', defaultId: 'palette.open' },
    ]);
    expect(update.mock.calls[0]?.[0]).toHaveLength(1);
  });

  it('withReboundRow replaces by identity and appends when the row is new', () => {
    const rows: KeybindingRule[] = [
      PALETTE_ROW,
      { key: 'mod+b', command: 'sidebar.toggle', defaultId: 'sidebar.toggle', defaultKey: 'mod+b' },
    ];
    expect(withReboundRow(rows, PALETTE_ROW, UNBOUND_CHORD)).toEqual([
      { ...PALETTE_ROW, key: UNBOUND_CHORD },
      rows[1],
    ]);

    const absent: KeybindingRule = {
      key: 'mod+p',
      command: 'thread.search',
      defaultId: 'thread.search',
      defaultKey: 'mod+p',
    };
    expect(withReboundRow(rows, absent, UNBOUND_CHORD)).toHaveLength(3);
    expect(withReboundRow(rows, absent, UNBOUND_CHORD)[2]).toMatchObject({
      key: UNBOUND_CHORD,
      command: 'thread.search',
      defaultId: 'thread.search',
    });
  });
});

// The chord-recorder guard. While the settings capture control has the
// keystroke, the keystroke is the DATA being recorded — recording mod+b
// must record mod+b, not collapse the sidebar (t3 bug dfbda8436).
describe('keybindings store — chord-recorder guard', () => {
  let host: HTMLDivElement;

  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
    host = document.createElement('div');
    document.body.appendChild(host);
  });

  afterEach(() => {
    host.remove();
  });

  function capturingButton(): HTMLButtonElement {
    const button = document.createElement('button');
    button.setAttribute(KEYBINDING_CAPTURE_ATTR, '');
    host.appendChild(button);
    return button;
  }

  it('recognises the capture control and its descendants', () => {
    const button = capturingButton();
    const inner = document.createElement('span');
    button.appendChild(inner);
    expect(isKeybindingCaptureTarget(button)).toBe(true);
    expect(isKeybindingCaptureTarget(inner)).toBe(true);
  });

  it('does not treat an ordinary node as a capture target', () => {
    const plain = document.createElement('button');
    host.appendChild(plain);
    expect(isKeybindingCaptureTarget(plain)).toBe(false);
    expect(isKeybindingCaptureTarget(null)).toBe(false);
  });

  it('refuses to run a bound command while a chord is being recorded', () => {
    const run = vi.fn();
    registerCommand({ id: 'sidebar.toggle', label: 'Toggle Sidebar', run });
    setKeybindingsForTest([{ key: 'mod+b', command: 'sidebar.toggle' }]);

    expect(dispatchFromNode(capturingButton(), 'b', { ctrlKey: true })).toBe(false);
    expect(run).not.toHaveBeenCalled();
  });

  it('still runs the same chord from anywhere else', () => {
    const run = vi.fn();
    registerCommand({ id: 'sidebar.toggle', label: 'Toggle Sidebar', run });
    setKeybindingsForTest([{ key: 'mod+b', command: 'sidebar.toggle' }]);

    const plain = document.createElement('button');
    host.appendChild(plain);
    expect(dispatchFromNode(plain, 'b', { ctrlKey: true })).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('stops claiming the chord for the editable-reachable path too', () => {
    registerCommand({
      id: 'sidebar.toggle',
      label: 'Toggle Sidebar',
      editableReachable: true,
      run: vi.fn(),
    });
    setKeybindingsForTest([{ key: 'mod+b', command: 'sidebar.toggle' }]);
    const ids = new Set(['sidebar.toggle']);

    const captured = ev('b', { ctrlKey: true });
    Object.defineProperty(captured, 'target', { value: capturingButton() });
    expect(eventMatchesKeybindingCommand(captured, baseCtx(), ids, { isMac: false })).toBe(false);

    const plainEvent = ev('b', { ctrlKey: true });
    Object.defineProperty(plainEvent, 'target', { value: document.createElement('button') });
    expect(eventMatchesKeybindingCommand(plainEvent, baseCtx(), ids, { isMac: false })).toBe(true);
  });
});
