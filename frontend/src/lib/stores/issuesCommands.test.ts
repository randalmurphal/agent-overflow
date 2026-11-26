import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { clearCommandRegistry, getCommand, isCommandEnabled, runCommand, type CommandContext } from './commandRegistry.svelte';
import { makeCommandContext } from './builtinCommands.svelte';
import { isIssuesPaneFocused, registerIssuesBackHandler, registerIssuesCommands, resetIssuesCommandsForTest } from './issuesCommands';
import { evaluateWhen, tryParseWhen } from './keybindingParser';
import { focusPane } from './panes.svelte';
import { openTool } from './toolPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { grantBackendScopes, pairWithScopes, resetToLocalPage } from '../../test/helpers/scopes';
import { HOME_BACKEND } from '../transport/backendKey';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';

function ctx(flags: Record<string, boolean> = {}): CommandContext {
  return { pane: null, paneId: null, flags: { issuesPaneFocused: true, toolDetailOpen: true, ...flags } } as unknown as CommandContext;
}

describe('issues commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetPaneLayoutForTest();
    setPaneLayoutItemsForTest([]);
    registerIssuesCommands();
  });

  afterEach(() => {
    resetToLocalPage();
    resetIssuesCommandsForTest();
  });

  it('offers Issues: Open only while a computer lets this window read issue trackers', async () => {
    await pairWithScopes(['threads:read']);
    expect(isCommandEnabled('issues.toggle', makeCommandContext(null, {}))).toBe(false);
    await grantBackendScopes(HOME_BACKEND, ['threads:read', 'tracker:read']);
    expect(isCommandEnabled('issues.toggle', makeCommandContext(null, {}))).toBe(true);
  });

  it('refuses to open Issues in a window no computer lets read issue trackers', async () => {
    await pairWithScopes(['threads:read']);
    expect(() => openTool('issues')).toThrow(/tracker:read/);
  });

  it('steps back out of an open ticket on Escape, a surface dismissal gated on the pane and an open ticket', () => {
    const command = getCommand('issues.escape');
    expect(command?.dismissesSurface).toBe(true);
    const when = tryParseWhen(command!.when!)!;
    expect(evaluateWhen(when, ctx().flags)).toBe(true);
    expect(evaluateWhen(when, ctx({ toolDetailOpen: false }).flags)).toBe(false);
    expect(evaluateWhen(when, ctx({ issuesPaneFocused: false }).flags)).toBe(false);
    expect(evaluateWhen(when, ctx({ overlayOpen: true }).flags)).toBe(false);
    expect(evaluateWhen(when, ctx({ paneFindOpen: true }).flags)).toBe(false);
    const escape = KEYBINDING_DEFAULTS.find((rule) => rule.command === 'issues.escape');
    expect(escape?.key).toBe('esc');
    expect(escape?.when).toBe('issuesPaneFocused && toolDetailOpen');

    setPaneLayoutItemsForTest([{ id: 'tool-issues', paneId: 'tool-issues', kind: 'issues', widthPx: 400 }]);
    focusPane('tool-issues');
    const steps: string[] = [];
    const first = registerIssuesBackHandler('tool-issues', () => { steps.push('first'); });
    const second = registerIssuesBackHandler('tool-issues', () => { steps.push('second'); });
    runCommand('issues.escape', ctx());
    second();
    runCommand('issues.escape', ctx());
    first();
    runCommand('issues.escape', ctx());
    expect(steps).toEqual(['second', 'first']);
  });

  it('steps back in the focused pane only: the Issues pane or a ticket pane', () => {
    setPaneLayoutItemsForTest([
      { id: 'tool-issues', paneId: 'tool-issues', kind: 'issues', widthPx: 400 },
      { id: 'ticket-a', paneId: 'ticket-a', kind: 'ticket', widthPx: 400 },
    ]);
    const steps: string[] = [];
    const disposers = [
      registerIssuesBackHandler('tool-issues', () => { steps.push('issues'); }),
      registerIssuesBackHandler('ticket-a', () => { steps.push('ticket'); }),
    ];
    focusPane('ticket-a');
    runCommand('issues.escape', ctx());
    focusPane('tool-issues');
    runCommand('issues.escape', ctx());
    expect(steps).toEqual(['ticket', 'issues']);
    for (const dispose of disposers) dispose();
  });

  it('knows when the Issues pane or a ticket pane is the focused pane', () => {
    setPaneLayoutItemsForTest([
      { id: 'tool-issues', paneId: 'tool-issues', kind: 'issues', widthPx: 400 },
      { id: 'ticket-a', paneId: 'ticket-a', kind: 'ticket', widthPx: 400 },
    ]);
    expect(isIssuesPaneFocused()).toBe(false);
    focusPane('tool-issues');
    expect(isIssuesPaneFocused()).toBe(true);
    focusPane('ticket-a');
    expect(isIssuesPaneFocused()).toBe(true);
  });
});
