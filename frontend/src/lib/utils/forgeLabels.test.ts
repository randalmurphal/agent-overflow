import { describe, expect, it } from 'vitest';
import { forgeLabels } from './forgeLabels';

describe('forgeLabels', () => {
  it('returns github strings for github', () => {
    const labels = forgeLabels('github');
    expect(labels.noun).toBe('PR');
    expect(labels.createAction).toBe('Create PR');
    expect(labels.longSingular).toBe('Pull request');
    expect(labels.openAction).toBe('Open PR');
    expect(labels.numberSigil).toBe('#');
  });

  it('returns gitlab strings for gitlab', () => {
    const labels = forgeLabels('gitlab');
    expect(labels.noun).toBe('MR');
    expect(labels.createAction).toBe('Create MR');
    expect(labels.longSingular).toBe('Merge request');
    expect(labels.openAction).toBe('Open MR');
    expect(labels.numberSigil).toBe('!');
  });

  it('falls back to github strings for empty / null / undefined', () => {
    expect(forgeLabels('').noun).toBe('PR');
    expect(forgeLabels(null).noun).toBe('PR');
    expect(forgeLabels(undefined).noun).toBe('PR');
  });

  it('falls back to github strings for unrecognised forge ids', () => {
    expect(forgeLabels('bitbucket').noun).toBe('PR');
    expect(forgeLabels('gitea').noun).toBe('PR');
  });
});
