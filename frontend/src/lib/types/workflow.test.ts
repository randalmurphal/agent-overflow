import { describe, expect, it } from 'vitest';
import { parseWorkflowDigest, parseWorkflowDisposition } from './workflow';

describe('workflow JSON projections', () => {
  it('accepts embedded RawMessage objects and encoded strings', () => {
    expect(parseWorkflowDigest({ whatHappened: 'Built', whatItNeeds: 'Review' })).toEqual({
      whatHappened: 'Built', whatItNeeds: 'Review',
    });
    expect(parseWorkflowDigest('{"whatHappened":"Built","whatItNeeds":"Nothing"}')).toEqual({
      whatHappened: 'Built', whatItNeeds: 'Nothing',
    });
    expect(parseWorkflowDisposition({ action: 'merged', mode: 'ff', sha: 'abc', policy: 'manual', at: 1 })).toMatchObject({ action: 'merged', sha: 'abc' });
  });

  it('rejects malformed human-facing payloads', () => {
    expect(parseWorkflowDigest({ whatHappened: 2, whatItNeeds: 'x' })).toBeNull();
    expect(parseWorkflowDisposition({ action: 'merged' })).toBeNull();
  });
});
