// Workflow commands: the overlay's keyboard vocabulary (UI-SPEC §8).
// Registered from registerBuiltinCommands so they live in the one command
// registry the palette and the keybinding dispatcher both read; the shipped
// chords are in
// `internal/keybindings` Defaults, gated on `workflowsOverlayOpen` /
// `workflowsRunDetail`, so a bare `a` or `j` is inert everywhere else and
// suppressed entirely while a text field has focus.
//
// Navigation and sweep commands run straight off the stores. The four
// resolution keys (a / r / t / Enter) need the run detail's own context — the
// note field, the selected unit, the first diff file — so WorkflowRunDetail
// registers a target here and the commands dispatch through it.

import { registerCommand } from './commandRegistry.svelte';
import type { WorkflowActionKey } from '../utils/workflowActionRows';
import {
  consumeWorkflowsOverlayEscape,
  closeWorkflowsOverlay,
  getWorkflowsOverlayRunId,
  popWorkflowsOverlay,
  toggleWorkflowsOverlay,
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
    description: 'Open the workflows overlay over the pane tree. Pressing the same chord while open closes it.',
    icon: '⧉',
    run: () => toggleWorkflowsOverlay(),
  });

  registerCommand({
    id: 'workflows.escape',
    dismissesSurface: true,
    label: 'Workflows: Escape',
    description: 'Disarm a confirmation, close a dialog, go back, then close the overlay — in that order.',
    when: 'workflowsOverlayOpen',
    run: () => {
      consumeWorkflowsOverlayEscape();
    },
  });

  registerCommand({
    id: 'workflows.back',
    label: 'Workflows: Back',
    icon: '‹',
    when: 'workflowsOverlayOpen',
    run: () => {
      if (!popWorkflowsOverlay()) closeWorkflowsOverlay();
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
