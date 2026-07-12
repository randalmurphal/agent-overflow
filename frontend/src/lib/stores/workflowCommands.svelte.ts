import { registerCommand, unregisterCommand } from './commandRegistry.svelte';
import { registerRuntimeKeybindingDefaults, type KeybindingRule } from './keybindings.svelte';
import { getFocusedPaneId } from './panes.svelte';
import {
  WORKFLOWS_PANE_ID,
  consumeWorkflowEscape,
  getWorkflowCurrentLevel,
  stepWorkflowSweep,
} from './workflowsPane.svelte';

export type WorkflowActionKey = 'a' | 'r' | 't' | 'enter' | `digit-${number}`;
let actionKeyHandler: ((key: WorkflowActionKey) => void) | null = null;

export function setWorkflowActionKeyHandler(handler: ((key: WorkflowActionKey) => void) | null): void {
  actionKeyHandler = handler;
}

const commandIds = [
  'workflow.back', 'workflow.next', 'workflow.previous', 'workflow.action.a',
  'workflow.action.r', 'workflow.action.t', 'workflow.action.enter',
  ...Array.from({ length: 9 }, (_, index) => `workflow.answer.${index + 1}`),
];

export const workflowDefaultKeybindings: readonly KeybindingRule[] = ([
  { key: 'escape', command: 'workflow.back', when: 'workflowsFocus' },
  { key: 'backspace', command: 'workflow.back', when: 'workflowsFocus' },
  { key: 'j', command: 'workflow.next', when: 'workflowsFocus' },
  { key: 'k', command: 'workflow.previous', when: 'workflowsFocus' },
  { key: 'arrowright', command: 'workflow.next', when: 'workflowsFocus' },
  { key: 'arrowdown', command: 'workflow.next', when: 'workflowsFocus' },
  { key: 'arrowleft', command: 'workflow.previous', when: 'workflowsFocus' },
  { key: 'arrowup', command: 'workflow.previous', when: 'workflowsFocus' },
  { key: 'a', command: 'workflow.action.a', when: 'workflowsFocus' },
  { key: 'r', command: 'workflow.action.r', when: 'workflowsFocus' },
  { key: 't', command: 'workflow.action.t', when: 'workflowsFocus' },
  { key: 'enter', command: 'workflow.action.enter', when: 'workflowsFocus' },
  ...Array.from({ length: 9 }, (_, index) => ({
    key: String(index + 1), command: `workflow.answer.${index + 1}`, when: 'workflowsFocus',
  })),
] satisfies KeybindingRule[]).map((rule, index) => ({
  ...rule,
  defaultId: `workflows-pane-${index + 1}`,
  defaultKey: rule.key,
}));

function focused(): boolean { return getFocusedPaneId() === WORKFLOWS_PANE_ID; }

export function registerWorkflowCommands(): () => void {
  const unregisterDefaults = registerRuntimeKeybindingDefaults('workflows-pane', workflowDefaultKeybindings);
  registerCommand({ id: 'workflow.back', label: 'Workflows: Back', when: 'workflowsFocus', run: () => { consumeWorkflowEscape(); } });
  registerCommand({ id: 'workflow.next', label: 'Workflows: Next attention run', when: 'workflowsFocus', run: () => { if (getWorkflowCurrentLevel().kind === 'run') stepWorkflowSweep(1); } });
  registerCommand({ id: 'workflow.previous', label: 'Workflows: Previous attention run', when: 'workflowsFocus', run: () => { if (getWorkflowCurrentLevel().kind === 'run') stepWorkflowSweep(-1); } });
  for (const key of ['a', 'r', 't', 'enter'] as const) {
    registerCommand({
      id: `workflow.action.${key}`,
      label: `Workflows: ${key === 'a' ? 'Primary action' : key === 'r' ? 'Secondary action' : key === 't' ? 'Open thread' : 'Toggle evidence'}`,
      when: 'workflowsFocus',
      run: () => { if (focused()) actionKeyHandler?.(key); },
    });
  }
  for (let digit = 1; digit <= 9; digit += 1) {
    registerCommand({
      id: `workflow.answer.${digit}`,
      label: `Workflows: Answer ${digit}`,
      when: 'workflowsFocus',
      run: () => { if (focused()) actionKeyHandler?.(`digit-${digit}`); },
    });
  }
  return () => {
    unregisterDefaults();
    for (const id of commandIds) unregisterCommand(id);
    actionKeyHandler = null;
  };
}
