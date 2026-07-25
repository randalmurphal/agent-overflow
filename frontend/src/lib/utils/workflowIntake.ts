// New-run validation (UI-SPEC §5.1). Pure: the dialog renders fields and this
// module decides whether the draft can start.
//
// R2 — the word "variables" never appears. A workflow's typed inputs are plain
// form fields, and every message here names the field, not the schema that
// declared it.

import type { WorkflowDefinitionInput, WorkflowDefinitionListing } from '../types/workflow';

export interface WorkflowIntakeDraft {
  projectId: string;
  goal: string;
  definition: WorkflowDefinitionListing | null;
  seeds: Record<string, unknown>;
}

function isBlank(value: unknown): boolean {
  return value === undefined || value === null || value === '';
}

export function workflowIntakeError(draft: WorkflowIntakeDraft): string | null {
  if (!draft.projectId) return 'Choose a project';
  if (!draft.goal.trim()) return 'Enter a goal';
  if (!draft.definition) return 'Choose a workflow';
  if (!draft.definition.valid) return draft.definition.firstValidationError || 'This workflow cannot run yet';
  for (const input of draft.definition.inputs ?? []) {
    const value = draft.seeds[input.name];
    if (input.required && isBlank(value)) return `${input.name} is required`;
    if (isBlank(value)) continue;
    if (input.type === 'number' && (typeof value !== 'number' || !Number.isFinite(value))) {
      return `${input.name} must be a number`;
    }
    if (input.type === 'boolean' && typeof value !== 'boolean') return `${input.name} must be true or false`;
    if (input.enum && !input.enum.includes(value)) return `${input.name} must use an offered value`;
  }
  return null;
}

/** Drop empty fields so an optional input the human left alone stays unset. */
export function compactWorkflowSeeds(seeds: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(seeds).filter(([, value]) => !isBlank(value)));
}

/**
 * The value a field starts on. Declared defaults arrive as raw JSON, so a
 * string default that happens to be valid JSON is decoded rather than shown
 * quoted; a boolean with no default starts unchecked so the control is never
 * indeterminate.
 */
export function workflowSeedDefault(input: WorkflowDefinitionInput): unknown {
  if (input.default !== undefined && input.default !== null) {
    if (typeof input.default !== 'string') return input.default;
    try {
      return JSON.parse(input.default) as unknown;
    } catch {
      return input.default;
    }
  }
  return input.type === 'boolean' ? false : undefined;
}
