import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { CommandContext } from './commandRegistry.svelte';
import { clearCommandRegistry, dismissCommandSurface, getCommand, listCommands, runCommand } from './commandRegistry.svelte';
import {
  getWorkflowsActionTargetForTest,
  registerWorkflowCommands,
  registerWorkflowsActionTarget,
} from './workflowCommands.svelte';
import {
  closeWorkflowsPane,
  getWorkflowsOverlayRunId,
  getWorkflowsOverlayTop,
  openWorkflowsPane,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
  setWorkflowArmedAction,
} from './workflowsOverlay.svelte';
// Imported for its side effect as much as its API: the settings store arms the
// tool-open exclusion at module init, which is what these tests exercise.
import {
  isSettingsOpen,
  openSettingsOverlay,
  resetSettingsOverlayForTest,
} from './settingsOverlay.svelte';
import { resetAppStorageForTest } from './appStorage';
import { createPane, focusPane, getFocusedPaneId, resetPanesForTest } from './panes.svelte';
import { getPaneLayoutItems, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import { isToolOpen, resetToolPanesForTest } from './toolPanes.svelte';
import { evaluateWhen, parseWhen } from './keybindingParser';
import { KEYBINDING_DEFAULTS } from '../generated/keybindingDefaults';

/** The palette/keybinding call shape the workflows commands are dispatched with. */
function ctx(over: Record<string, unknown> = {}): CommandContext {
  const flags = {
    workflowsPaneFocused: true,
    toolDetailOpen: true,
    workflowsRunDetail: true,
    hasActiveThread: true,
    ...(over.flags as Record<string, boolean> | undefined),
  };
  return {
    pane: { paneId: 'pane-1', threadId: 'thread-1' },
    paneId: 'pane-1',
    ...over,
    flags,
  } as unknown as CommandContext;
}

function resetPaneState(): void {
  resetToolPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  setPaneLayoutItemsForTest([]);
}

describe('workflows commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    resetSettingsOverlayForTest();
    resetPaneState();
    registerWorkflowCommands();
  });

  it('registers exactly the §8 vocabulary — the composer command is not a palette entry (D31)', () => {
    expect(listCommands().map((command) => command.id).sort()).toEqual([
      'workflows.action.enter',
      'workflows.action.primary',
      'workflows.action.reject',
      'workflows.action.retry-units',
      'workflows.action.thread',
      'workflows.back',
      'workflows.escape',
      'workflows.sweep.next',
      'workflows.sweep.prev',
      'workflows.toggle',
    ]);
  });

  it('scopes every pane command to the focused workflows pane, and none of them to a text field', () => {
    for (const command of listCommands()) {
      if (!command.id.startsWith('workflows.')) continue;
      expect(
        command.when === 'workflowsPaneFocused && toolDetailOpen'
          || command.when === 'workflowsRunDetail'
          || command.id === 'workflows.toggle',
      ).toBe(true);
      // §8: suppressed while a text field has focus — App.svelte only dispatches
      // editable-target chords for editableReachable commands.
      expect(command.editableReachable ?? false).toBe(false);
    }
    // `/workflow` is typed into the composer and expanded by the send path
    // (D31); it must not come back as a palette action that pastes text.
    expect(getCommand('workflow.composerContext')).toBeUndefined();
  });

  // The shipped Escape chord carries the same gate as the command, so a user
  // reading the keybinding list sees why Escape at home does nothing here.
  it('ships the Escape chord gated on an open detail step', () => {
    const rule = KEYBINDING_DEFAULTS.find((entry) => entry.command === 'workflows.escape');
    expect(rule?.when).toBe('workflowsPaneFocused && toolDetailOpen');
    const ast = parseWhen(rule?.when ?? '');
    expect(evaluateWhen(ast, { workflowsPaneFocused: true, toolDetailOpen: false })).toBe(false);
    expect(evaluateWhen(ast, { workflowsPaneFocused: true, toolDetailOpen: true })).toBe(true);
  });

  it('opens the pane, and closes it only when it holds focus', () => {
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
    createPane('main');
    focusPane('main');
    runCommand('workflows.toggle', ctx());
    expect(isToolOpen('workflows')).toBe(true);
    expect(getFocusedPaneId()).toBe('tool-workflows');
    runCommand('workflows.toggle', ctx());
    expect(isToolOpen('workflows')).toBe(false);
  });

  it('reveals an open but unfocused pane instead of closing it', () => {
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
    createPane('main');
    focusPane('main');
    openWorkflowsPane({ newPane: true });
    focusPane('main');
    runCommand('workflows.toggle', ctx());
    expect(isToolOpen('workflows')).toBe(true);
    expect(getFocusedPaneId()).toBe('tool-workflows');
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main', 'tool-workflows']);
  });

  // Settings is a layer over the pane strip, so a tool pane opened under it
  // would be invisible. The exclusion sits on `openTool`, so it covers the
  // chord, the tools row and the notification deep link alike. It is armed by
  // the settings store's module init — importing that module is the whole
  // wiring, and no reset disarms it.
  it('closes settings on every path that opens the pane', () => {
    openSettingsOverlay('theme');
    runCommand('workflows.toggle', ctx());
    expect(isToolOpen('workflows')).toBe(true);
    expect(isSettingsOpen()).toBe(false);

    openSettingsOverlay('theme');
    // The tools row and the OS-notification deep link both land here rather
    // than on the command; the pane is already open, so this only reveals it.
    openWorkflowsPane();
    expect(isSettingsOpen()).toBe(false);
  });

  // Settings over an open workflows pane is the reverse direction, and a pane
  // is not a layer: opening settings leaves the pane in the strip.
  it('keeps the pane when settings opens over it', () => {
    openWorkflowsPane();
    openSettingsOverlay('theme');
    expect(isSettingsOpen()).toBe(true);
    expect(isToolOpen('workflows')).toBe(true);
  });

  // Regression: the closer used to run unconditionally on every open, and it
  // blurs `document.activeElement` so a settings field commits before its
  // input unmounts. Opening the pane while settings was already closed must
  // not steal focus from whatever the user was typing in.
  it('leaves focus alone when settings is already closed', () => {
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    expect(document.activeElement).toBe(input);

    openWorkflowsPane();

    expect(isToolOpen('workflows')).toBe(true);
    expect(document.activeElement).toBe(input);
    input.remove();
  });

  it('walks the escape ladder: disarm, then back, and leaves the pane at home', () => {
    openWorkflowsPane();
    pushWorkflowRunDetail('run-1');
    setWorkflowArmedAction('cancel:run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    expect(isToolOpen('workflows')).toBe(true);
  });

  // The phone's Back runs surface-dismissal commands (utils/stepBack.ts). A
  // gate that passed at home would report the press consumed while doing
  // nothing, and Back could never leave the pane.
  it('declines a surface dismissal at home so Back can leave the pane', () => {
    openWorkflowsPane();
    pushWorkflowRunDetail('run-1');
    expect(dismissCommandSurface(ctx({ flags: { toolDetailOpen: true } }))).toBe(true);
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    expect(dismissCommandSurface(ctx({ flags: { toolDetailOpen: false } }))).toBe(false);
    expect(dismissCommandSurface(ctx({ flags: { workflowsPaneFocused: false } }))).toBe(false);
  });

  it('backs out of a run to home', () => {
    openWorkflowsPane();
    pushWorkflowRunDetail('run-1');
    runCommand('workflows.back', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    expect(isToolOpen('workflows')).toBe(true);
    closeWorkflowsPane();
    expect(isToolOpen('workflows')).toBe(false);
  });

  it('routes the resolution keys through whichever run detail is mounted', () => {
    const action = vi.fn();
    const enter = vi.fn();
    const dispose = registerWorkflowsActionTarget({ action, enter });
    runCommand('workflows.action.primary', ctx());
    runCommand('workflows.action.reject', ctx());
    runCommand('workflows.action.thread', ctx());
    runCommand('workflows.action.enter', ctx());
    expect(action.mock.calls.map(([key]) => key)).toEqual(['a', 'r', 't']);
    expect(enter).toHaveBeenCalledTimes(1);

    dispose();
    expect(getWorkflowsActionTargetForTest()).toBeNull();
    // No target mounted: the keys are inert rather than throwing.
    expect(() => runCommand('workflows.action.primary', ctx())).not.toThrow();
  });

  it('leaves a newer target in place when an older one disposes', () => {
    const first = { action: vi.fn(), enter: vi.fn() };
    const second = { action: vi.fn(), enter: vi.fn() };
    const disposeFirst = registerWorkflowsActionTarget(first);
    registerWorkflowsActionTarget(second);
    disposeFirst();
    expect(getWorkflowsActionTargetForTest()).toBe(second);
  });
});
