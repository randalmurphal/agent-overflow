import { beforeEach, describe, expect, it, vi } from 'vitest';
import { focusPane, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { getWorkflowCurrentLevel, openWorkflowIntake, pushWorkflowLevel, resetWorkflowsPane, setWorkflowArmedAction } from './workflowsPane.svelte';
import { clearCommandRegistry, listCommands, type CommandContext } from './commandRegistry.svelte';
import { dispatchKey, resetKeybindingsStore, setKeybindingsForTest } from './keybindings.svelte';
import { registerWorkflowCommands, setWorkflowActionKeyHandler, workflowDefaultKeybindings } from './workflowCommands.svelte';

function commandContext(workflowsFocus: boolean): CommandContext {
  const flags = {
    paletteOpen: false, terminalOpen: false, terminalFocus: false, approvalPending: false,
    anyModalOpen: false, hasActiveThread: false, turnActive: false, sendInFlight: false,
    hasPendingPrompt: false, canForkActiveThread: false, canStartDiscussion: false,
    sidebarCursorActive: false, anyPickerOpen: false, workflowsFocus,
  };
  return { ...flags, pane: null, paneId: null, flags };
}

describe('workflow keyboard scoping', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetWorkflowsPane();
    resetKeybindingsStore();
    clearCommandRegistry();
    setPaneLayoutItemsForTest([{ id: 'workflows', paneId: 'workflows', kind: 'workflows', widthPx: 500 }]);
    focusPane('workflows');
  });
  it.each([
    ['escape', 'workflow.back'],
    ['backspace', 'workflow.back'],
    ['j', 'workflow.next'],
    ['arrowright', 'workflow.next'],
    ['arrowdown', 'workflow.next'],
    ['k', 'workflow.previous'],
    ['arrowleft', 'workflow.previous'],
    ['arrowup', 'workflow.previous'],
    ['a', 'workflow.action.a'],
    ['r', 'workflow.action.r'],
    ['t', 'workflow.action.t'],
    ['enter', 'workflow.action.enter'],
    ['4', 'workflow.answer.4'],
  ])('registers the %s default as %s, scoped to workflowsFocus', (key, command) => {
    const rule = workflowDefaultKeybindings.find((entry) => entry.key === key);
    expect(rule).toMatchObject({ command, when: 'workflowsFocus', defaultKey: key });
    expect(rule?.defaultId).toMatch(/^workflows-pane-\d+$/);
  });

  it('does not claim ordinary typing', () => {
    const unregister = registerWorkflowCommands();
    expect(workflowDefaultKeybindings.some((rule) => rule.key === 'x')).toBe(false);
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'x' }), commandContext(true), { isMac: false })).toBe(false);
    unregister();
  });

  it('registers no editable-reachable commands, so typing in fields never triggers actions', () => {
    // App.svelte's window handler only dispatches editableReachable commands
    // while an input/textarea has focus; bare workflow keys must stay outside
    // that set or typing "a" in the answer field would fire Approve.
    const unregister = registerWorkflowCommands();
    const workflowCommands = listCommands().filter((command) => command.id.startsWith('workflow.'));
    expect(workflowCommands.length).toBeGreaterThan(0);
    expect(workflowCommands.filter((command) => command.editableReachable)).toEqual([]);
    unregister();
  });

  it('dispatches runtime defaults only with workflows focus', () => {
    const action = vi.fn();
    const unregister = registerWorkflowCommands();
    setWorkflowActionKeyHandler(action);
    const event = new KeyboardEvent('keydown', { key: 'a' });
    const focused = commandContext(true);
    const elsewhere = commandContext(false);
    expect(dispatchKey(event, focused, { isMac: false })).toBe(true);
    expect(action).toHaveBeenCalledWith('a');
    expect(dispatchKey(event, elsewhere, { isMac: false })).toBe(false);
    unregister();
  });

  it('lets a persisted override replace a workflow runtime default', () => {
    const action = vi.fn();
    const unregister = registerWorkflowCommands();
    setWorkflowActionKeyHandler(action);
    setKeybindingsForTest([{
      key: 'x', command: 'workflow.action.a', when: 'workflowsFocus',
      defaultId: 'workflows-pane-9', defaultKey: 'a',
    }]);
    const context = commandContext(true);
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'a' }), context, { isMac: false })).toBe(false);
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'x' }), context, { isMac: false })).toBe(true);
    expect(action).toHaveBeenCalledWith('a');
    unregister();
  });

  it('applies Escape precedence before popping the stack', () => {
    const unregister = registerWorkflowCommands();
    const context = commandContext(true);
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    setWorkflowArmedAction('discard:item');
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'Escape' }), context, { isMac: false })).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('workflow');
    openWorkflowIntake();
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'Escape' }), context, { isMac: false })).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('workflow');
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'Escape' }), context, { isMac: false })).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('overview');
    unregister();
  });

  it('pops one level with Backspace and does nothing at overview', () => {
    const unregister = registerWorkflowCommands();
    const context = commandContext(true);
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'Backspace' }), context, { isMac: false })).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('overview');
    expect(dispatchKey(new KeyboardEvent('keydown', { key: 'Backspace' }), context, { isMac: false })).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('overview');
    unregister();
  });
});
