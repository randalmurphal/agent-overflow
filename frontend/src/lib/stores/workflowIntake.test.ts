import { describe, expect, it } from 'vitest';
import type { WorkflowDefinitionListing } from '../types/workflow';
import { compactWorkflowSeeds, workflowIntakeError } from './workflowIntake';

const definition = {
  valid: true,
  inputs: [
    { name: 'title', type: 'string', required: true },
    { name: 'count', type: 'number', required: false },
  ],
} as WorkflowDefinitionListing;

describe('workflow intake validation', () => {
  it('blocks missing required fields and invalid definitions', () => {
    expect(workflowIntakeError({ projectId: '', goal: '', definition: null, seeds: {} })).toBe('Choose a project');
    expect(workflowIntakeError({ projectId: 'p', goal: 'go', definition, seeds: {} })).toBe('title is required');
    expect(workflowIntakeError({ projectId: 'p', goal: 'go', definition: { ...definition, valid: false, firstValidationError: 'broken' }, seeds: {} })).toBe('broken');
  });

  it('accepts typed fields and omits empty optional seeds', () => {
    expect(workflowIntakeError({ projectId: 'p', goal: 'go', definition, seeds: { title: 'x', count: 2 } })).toBeNull();
    expect(compactWorkflowSeeds({ title: 'x', count: '', note: null, extra: undefined, flag: false })).toEqual({ title: 'x', flag: false });
  });
});
