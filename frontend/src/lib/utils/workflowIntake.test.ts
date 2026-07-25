import { describe, expect, it } from 'vitest';
import type { WorkflowDefinitionInput, WorkflowDefinitionListing } from '../types/workflow';
import { compactWorkflowSeeds, workflowIntakeError, workflowSeedDefault } from './workflowIntake';

function definition(inputs: Partial<WorkflowDefinitionInput>[] = [], over: Partial<WorkflowDefinitionListing> = {}): WorkflowDefinitionListing {
  return { id: 'port', name: 'Port', valid: true, inputs, ...over } as WorkflowDefinitionListing;
}

const base = { projectId: 'p', goal: 'ship it', definition: definition(), seeds: {} };

describe('workflowIntakeError', () => {
  it('walks the required fields in the order the dialog presents them', () => {
    expect(workflowIntakeError({ ...base, projectId: '' })).toBe('Choose a project');
    expect(workflowIntakeError({ ...base, goal: '   ' })).toBe('Enter a goal');
    expect(workflowIntakeError({ ...base, definition: null })).toBe('Choose a workflow');
  });

  it('surfaces the definition\'s own first error rather than a generic one', () => {
    expect(workflowIntakeError({
      ...base,
      definition: definition([], { valid: false, firstValidationError: 'phase "port" has no provider' }),
    })).toBe('phase "port" has no provider');
    expect(workflowIntakeError({
      ...base,
      definition: definition([], { valid: false }),
    })).toBe('This workflow cannot run yet');
  });

  it('requires declared inputs and names the field, never the schema', () => {
    const withRequired = definition([{ name: 'ticket', type: 'string', required: true }]);
    expect(workflowIntakeError({ ...base, definition: withRequired })).toBe('ticket is required');
    expect(workflowIntakeError({ ...base, definition: withRequired, seeds: { ticket: '' } })).toBe('ticket is required');
    expect(workflowIntakeError({ ...base, definition: withRequired, seeds: { ticket: 'AO-1' } })).toBeNull();
  });

  it('type-checks a filled optional field but ignores an empty one', () => {
    const optional = definition([
      { name: 'count', type: 'number', required: false },
      { name: 'dry', type: 'boolean', required: false },
      { name: 'mode', type: 'string', required: false, enum: ['fast', 'safe'] },
    ]);
    expect(workflowIntakeError({ ...base, definition: optional, seeds: {} })).toBeNull();
    expect(workflowIntakeError({ ...base, definition: optional, seeds: { count: 'seven' } })).toBe('count must be a number');
    expect(workflowIntakeError({ ...base, definition: optional, seeds: { count: Number.NaN } })).toBe('count must be a number');
    expect(workflowIntakeError({ ...base, definition: optional, seeds: { dry: 'yes' } })).toBe('dry must be true or false');
    expect(workflowIntakeError({ ...base, definition: optional, seeds: { mode: 'reckless' } })).toBe('mode must use an offered value');
    expect(workflowIntakeError({ ...base, definition: optional, seeds: { count: 3, dry: false, mode: 'safe' } })).toBeNull();
  });
});

describe('compactWorkflowSeeds', () => {
  it('drops only the untouched fields', () => {
    expect(compactWorkflowSeeds({ a: '', b: null, c: undefined, d: 0, e: false, f: 'x' }))
      .toEqual({ d: 0, e: false, f: 'x' });
  });
});

describe('workflowSeedDefault', () => {
  it('decodes a raw JSON default and leaves plain prose alone', () => {
    expect(workflowSeedDefault({ name: 'n', type: 'number', required: false, default: '7' } as WorkflowDefinitionInput)).toBe(7);
    expect(workflowSeedDefault({ name: 'n', type: 'string', required: false, default: '"hi"' } as WorkflowDefinitionInput)).toBe('hi');
    expect(workflowSeedDefault({ name: 'n', type: 'string', required: false, default: 'not json' } as WorkflowDefinitionInput)).toBe('not json');
  });

  it('starts a boolean unchecked so the control is never indeterminate', () => {
    expect(workflowSeedDefault({ name: 'n', type: 'boolean', required: false } as WorkflowDefinitionInput)).toBe(false);
    expect(workflowSeedDefault({ name: 'n', type: 'string', required: false } as WorkflowDefinitionInput)).toBeUndefined();
  });
});
