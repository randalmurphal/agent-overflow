// Workflow commands: the workflows pane's keyboard vocabulary (UI-SPEC §8).
// Registered from registerBuiltinCommands so they live in the one command
// registry the palette and the keybinding dispatcher both read; the shipped
// chords are in `internal/keybindings` Defaults, gated on
// `workflowsPaneFocused` / `workflowsRunDetail`, so a bare `a` or `j` is inert
// everywhere else and suppressed entirely while a text field has focus.
//
// Navigation and sweep commands run straight off the stores. The four
// resolution keys (a / r / t / Enter) need the run detail's own context — the
// note field, the selected unit, the first diff file — so WorkflowRunDetail
// registers a target here and the commands dispatch through it.

import { registerCommand } from './commandRegistry.svelte';
import type { WorkflowActionKey } from '../utils/workflowActionRows';
import {
  consumeWorkflowsOverlayEscape,
  getWorkflowsOverlayRunId,
  popWorkflowsOverlay,
  toggleWorkflowsPane,
} from './workflowsOverlay.svelte';
import { advanceWorkflowSweep } from './workflowSweep';

/**
 * The run detail's key surface. Every member is optional-free on purpose: a
 * state with no primary action still answers the call (with a no-op), so a
 * keypress can never fall through to a half-registered target.
 */
export interface WorkflowsActionTarget {
  /** `a` / `r` / `t` — the row's action for that key, if it has one. */
  action(key: WorkflowActionKey): void;
  /** Enter — toggle the first diff file, or send the answer. */
  enter(): void;
}

let actionTarget: WorkflowsActionTarget | null = null;

export function registerWorkflowsActionTarget(target: WorkflowsActionTarget): () => void {
  actionTarget = target;
  return () => {
    if (actionTarget === target) actionTarget = null;
  };
}

export function getWorkflowsActionTargetForTest(): WorkflowsActionTarget | null {
  return actionTarget;
}

export function registerWorkflowCommands(): void {
  registerCommand({
    id: 'workflows.toggle',
    label: 'Workflows: Open',
    description: 'Open or reveal the workflows pane. Pressing the same chord while it is focused closes it.',
    icon: '⧉',
    run: () => toggleWorkflowsPane(),
  });

  // The pane's list → detail step. Gated on `toolDetailOpen` as well as focus
  // because it is a surface dismissal: the phone's Back runs it, and a gate
  // that passed at home would consume Back without doing anything, so Back
  // could never leave the pane.
  registerCommand({
    id: 'workflows.escape',
    dismissesSurface: true,
    label: 'Workflows: Escape',
    description: 'Disarm a confirmation, close a dialog, then go back — in that order.',
    when: 'workflowsPaneFocused && toolDetailOpen',
    run: () => {
      consumeWorkflowsOverlayEscape();
    },
  });

  registerCommand({
    id: 'workflows.back',
    label: 'Workflows: Back',
    icon: '‹',
    when: 'workflowsRunDetail',
    run: () => {
      popWorkflowsOverlay();
    },
  });

  registerCommand({
    id: 'workflows.sweep.next',
    label: 'Workflows: Next Needs-Attention Run',
    icon: '↓',
    when: 'workflowsRunDetail',
    run: () => advanceWorkflowSweep(1, getWorkflowsOverlayRunId()),
  });

  registerCommand({
    id: 'workflows.sweep.prev',
    label: 'Workflows: Previous Needs-Attention Run',
    icon: '↑',
    when: 'workflowsRunDetail',
    run: () => advanceWorkflowSweep(-1, getWorkflowsOverlayRunId()),
  });

  const actionCommand = (id: string, label: string, key: WorkflowActionKey) => {
    registerCommand({
      id,
      label,
      when: 'workflowsRunDetail',
      run: () => actionTarget?.action(key),
    });
  };
  actionCommand('workflows.action.primary', 'Workflows: Primary Action', 'a');
  actionCommand('workflows.action.reject', 'Workflows: Request Changes / Discard', 'r');
  actionCommand('workflows.action.thread', 'Workflows: Thread Action', 't');
  actionCommand('workflows.action.retry-units', 'Workflows: Retry All Failed Units', 'u');

  registerCommand({
    id: 'workflows.action.enter',
    label: 'Workflows: Confirm',
    when: 'workflowsRunDetail',
    run: () => actionTarget?.enter(),
  });
}
