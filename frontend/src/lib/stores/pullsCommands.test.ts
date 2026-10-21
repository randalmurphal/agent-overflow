import { beforeEach, describe, expect, it } from 'vitest';
import { clearCommandRegistry, getCommand, runCommand, type CommandContext } from './commandRegistry.svelte';
import {
  isPullsPaneFocused,
  registerPullsBackHandler,
  registerPullsCommands,
  resetPullsCommandsForTest,
  stepPullsBack,
} from './pullsCommands.svelte';
import { focusPane, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { isToolOpen, resetToolPanesForTest } from './toolPanes.svelte';
import { resetAppStorageForTest } from './appStorage';
import { evaluateWhen, parseWhen } from './keybindingParser';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';

function ctx(flags: Record<string, boolean> = {}): CommandContext {
  return { pane: null, paneId: null, flags: { pullsPaneFocused: true, toolDetailOpen: true, ...flags } } as unknown as CommandContext;
}

describe('pulls commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetAppStorageForTest();
    resetPullsCommandsForTest();
    resetToolPanesForTest();
    resetPanesForTest();
    resetPaneLayoutForTest();
    setPaneLayoutItemsForTest([]);
    registerPullsCommands();
  });

  it('pulls.toggle opens the pane and closes it again', () => {
    runCommand('pulls.toggle', ctx());
    expect(isToolOpen('pulls')).toBe(true);
    runCommand('pulls.toggle', ctx());
    expect(isToolOpen('pulls')).toBe(false);
  });

  it('pulls.escape is a surface dismissal gated on the pane and an open detail', () => {
    const command = getCommand('pulls.escape');
    expect(command?.dismissesSurface).toBe(true);
    const when = parseWhen(command!.when!);
    expect(evaluateWhen(when, ctx().flags)).toBe(true);
    expect(evaluateWhen(when, ctx({ toolDetailOpen: false }).flags)).toBe(false);
    expect(evaluateWhen(when, ctx({ pullsPaneFocused: false }).flags)).toBe(false);
  });

  it('pulls.escape runs the pane\'s back handler, and the handler can be restored', () => {
    const steps: string[] = [];
    const first = registerPullsBackHandler(() => { steps.push('first'); return true; });
    const second = registerPullsBackHandler(() => { steps.push('second'); return true; });
    runCommand('pulls.escape', ctx());
    second();
    runCommand('pulls.escape', ctx());
    first();
    expect(stepPullsBack()).toBe(false);
    expect(steps).toEqual(['second', 'first']);
  });

  it('knows when the pulls pane is the focused pane', () => {
    setPaneLayoutItemsForTest([{ id: 'tool-pulls', paneId: 'tool-pulls', kind: 'pulls', widthPx: 400 }]);
    expect(isPullsPaneFocused()).toBe(false);
    focusPane('tool-pulls');
    expect(isPullsPaneFocused()).toBe(true);
  });

  it('ships default chords for both commands', () => {
    const byCommand = new Map(KEYBINDING_DEFAULTS.map((rule) => [rule.command, rule]));
    expect(byCommand.get('pulls.toggle')?.key).toBe('mod+shift+p');
    const escape = byCommand.get('pulls.escape');
    expect(escape?.key).toBe('esc');
    expect(escape?.when).toBe('pullsPaneFocused && toolDetailOpen');
  });
});
