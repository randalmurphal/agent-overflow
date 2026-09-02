import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { CommandContext } from './commandRegistry.svelte';
import { clearCommandRegistry, getCommand, listCommands, runCommand } from './commandRegistry.svelte';
import {
  getWorkflowsActionTargetForTest,
  registerWorkflowCommands,
  registerWorkflowsActionTarget,
} from './workflowCommands.svelte';
import {
  closeWorkflowsOverlay,
  getWorkflowsOverlayRunId,
  getWorkflowsOverlayTop,
  isWorkflowsOverlayOpen,
  openWorkflowsOverlay,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
  setWorkflowArmedAction,
} from './workflowsOverlay.svelte';
// Imported for its side effect as much as its API: the settings store arms the
// mutual exclusion at module init, which is what these tests exercise.
import {
  isSettingsOpen,
  openSettingsOverlay,
  resetSettingsOverlayForTest,
} from './settingsOverlay.svelte';
import { resetAppStorageForTest } from './appStorage';

/** The palette/keybinding call shape the overlay commands are dispatched with. */
function ctx(over: Record<string, unknown> = {}): CommandContext {
  const flags = {
    workflowsOverlayOpen: true,
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

describe('overlay commands', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    resetSettingsOverlayForTest();
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

  it('scopes every overlay command to the overlay, and none of them to a text field', () => {
    for (const command of listCommands()) {
      if (!command.id.startsWith('workflows.')) continue;
      expect(command.when === 'workflowsOverlayOpen' || command.when === 'workflowsRunDetail' || command.id === 'workflows.toggle').toBe(true);
      // §8: suppressed while a text field has focus — App.svelte only dispatches
      // editable-target chords for editableReachable commands.
      expect(command.editableReachable ?? false).toBe(false);
    }
    // `/workflow` is typed into the composer and expanded by the send path
    // (D31); it must not come back as a palette action that pastes text.
    expect(getCommand('workflow.composerContext')).toBeUndefined();
  });

  it('toggles the overlay from the one unscoped command', () => {
    runCommand('workflows.toggle', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(true);
    runCommand('workflows.toggle', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  // Settings and this overlay are both full-height layers over the pane strip;
  // stacking them means two focus traps and an ambiguous Esc. The exclusion
  // sits on `openWorkflowsOverlay`, the one writer of `open = true`, so it
  // covers the chord, the sidebar chip and the notification deep link alike.
  // It is armed by the settings store's module init — importing that module is
  // the whole wiring, and no reset disarms it.
  it('closes settings on every path that opens the overlay', () => {
    openSettingsOverlay('theme');
    runCommand('workflows.toggle', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(true);
    expect(isSettingsOpen()).toBe(false);

    closeWorkflowsOverlay();
    openSettingsOverlay('theme');
    // The sidebar chip and the OS-notification deep link both land here
    // rather than on the command.
    openWorkflowsOverlay();
    expect(isSettingsOpen()).toBe(false);
  });

  // Regression: the closer used to run unconditionally on every open, and it
  // blurs `document.activeElement` so a settings field commits before its
  // input unmounts. Opening the overlay while settings was already closed
  // therefore stole focus from whatever the user was typing in.
  it('leaves focus alone when settings is already closed', () => {
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    expect(document.activeElement).toBe(input);

    openWorkflowsOverlay();

    expect(isWorkflowsOverlayOpen()).toBe(true);
    expect(document.activeElement).toBe(input);
    input.remove();
  });

  it('walks the escape ladder: disarm, then back, then close', () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1');
    setWorkflowArmedAction('cancel:run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
    runCommand('workflows.escape', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    runCommand('workflows.escape', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  it('backs out of a run and then closes the overlay', () => {
    openWorkflowsOverlay();
    pushWorkflowRunDetail('run-1');
    runCommand('workflows.back', ctx());
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    runCommand('workflows.back', ctx());
    expect(isWorkflowsOverlayOpen()).toBe(false);
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
