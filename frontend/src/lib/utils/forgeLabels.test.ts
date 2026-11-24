import { describe, expect, it } from 'vitest';
import { forgeLabels } from './forgeLabels';

describe('forgeLabels', () => {
  const strings = {
    github: { noun: 'PR', createAction: 'Create PR', longSingular: 'Pull request', openAction: 'Open PR', numberSigil: '#' },
    gitlab: { noun: 'MR', createAction: 'Create MR', longSingular: 'Merge request', openAction: 'Open MR', numberSigil: '!' },
  };

  // Empty, null, undefined and unrecognised forge ids fall back to github.
  it.each([
    ['github', 'github'],
    ['gitlab', 'gitlab'],
    ['', 'github'],
    [null, 'github'],
    [undefined, 'github'],
    ['bitbucket', 'github'],
    ['gitea', 'github'],
  ] as const)('returns for %j the %s strings', (forge, want) => {
    expect(forgeLabels(forge)).toMatchObject(strings[want]);
  });
});
