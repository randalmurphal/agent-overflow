import type { WorkflowDefinitionListing } from '../types/workflow';

export interface WorkflowIntakeDraft {
  projectId: string;
  goal: string;
  definition: WorkflowDefinitionListing | null;
  seeds: Record<string, unknown>;
}

export function workflowIntakeError(draft: WorkflowIntakeDraft): string | null {
  if (!draft.projectId) return 'Choose a project';
  if (!draft.goal.trim()) return 'Enter a goal';
  if (!draft.definition) return 'Choose a workflow';
  if (!draft.definition.valid) return draft.definition.firstValidationError || 'This workflow is invalid';
  for (const input of draft.definition.inputs) {
    const value = draft.seeds[input.name];
    if (input.required && (value === undefined || value === null || value === '')) return `${input.name} is required`;
    if (value === undefined || value === null || value === '') continue;
    if (input.type === 'number' && (typeof value !== 'number' || !Number.isFinite(value))) return `${input.name} must be a number`;
    if (input.type === 'boolean' && typeof value !== 'boolean') return `${input.name} must be true or false`;
    if (input.enum && !input.enum.includes(value)) return `${input.name} must use an offered value`;
  }
  return null;
}

export function compactWorkflowSeeds(seeds: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(seeds).filter(([, value]) => value !== '' && value !== undefined));
}
