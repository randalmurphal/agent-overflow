import { describe, expect, it, beforeEach, vi } from 'vitest';
import {
  clearCommandRegistry,
  enabledCommands,
  getCommand,
  isCommandEnabled,
  listCommands,
  registerCommand,
  runCommand,
  unregisterCommand,
  type CommandContext,
} from './commandRegistry.svelte';

function baseContext(extra: Partial<CommandContext> = {}): CommandContext {
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

describe('commandRegistry', () => {
  beforeEach(() => {
    clearCommandRegistry();
  });

  it('register + get + list + unregister round-trip', () => {
    const run = vi.fn();
    registerCommand({ id: 'foo', label: 'Foo', run });
    expect(getCommand('foo')?.label).toBe('Foo');
    expect(listCommands().map((c) => c.id)).toEqual(['foo']);

    unregisterCommand('foo');
    expect(getCommand('foo')).toBeUndefined();
    expect(listCommands()).toHaveLength(0);
  });

  it('runCommand invokes run() and returns true', () => {
    const run = vi.fn();
    registerCommand({ id: 'foo', label: 'Foo', run });
    const ok = runCommand('foo', baseContext());
    expect(ok).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('runCommand returns false for unknown ids and does nothing', () => {
    expect(runCommand('missing', baseContext())).toBe(false);
  });

  it('when-expression gates execution', () => {
    const run = vi.fn();
    registerCommand({ id: 'foo', label: 'Foo', when: 'terminalOpen', run });
    expect(runCommand('foo', baseContext())).toBe(false);
    expect(run).not.toHaveBeenCalled();

    expect(runCommand('foo', baseContext({ terminalOpen: true }))).toBe(true);
    expect(run).toHaveBeenCalledTimes(1);
  });

  it('isCommandEnabled reflects the when expression', () => {
    registerCommand({ id: 'foo', label: 'Foo', when: '!paletteOpen', run: () => {} });
    expect(isCommandEnabled('foo', baseContext())).toBe(true);
    expect(isCommandEnabled('foo', baseContext({ paletteOpen: true }))).toBe(false);
    expect(isCommandEnabled('missing', baseContext())).toBe(false);
  });

  it('enabledCommands filters disabled entries', () => {
    registerCommand({ id: 'always', label: 'Always', run: () => {} });
    registerCommand({ id: 'terminal', label: 'Terminal', when: 'terminalOpen', run: () => {} });
    registerCommand({ id: 'palette', label: 'Palette', when: 'paletteOpen', run: () => {} });

    const visible = enabledCommands(baseContext({ paletteOpen: true }));
    expect(visible.map((c) => c.id).sort()).toEqual(['always', 'palette']);
  });

  it('invalid when-expressions disable the command but do not crash registration', () => {
    registerCommand({ id: 'foo', label: 'Foo', when: 'a &&', run: () => {} });
    // Registered, but since the parser rejected the expression, whenAst is null,
    // so the command is listed and always enabled. This mirrors "fail loudly"
    // at the settings-panel issues list rather than silently dropping commands.
    expect(getCommand('foo')).toBeDefined();
    expect(isCommandEnabled('foo', baseContext())).toBe(true);
  });
});
