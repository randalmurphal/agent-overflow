import { expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { itemTranscriptScope } from './itemTranscriptScope';

it('routes a nested completion through its attached launch when the launch is not loaded', () => {
  const launch = makeItem({ id: 'nested', parentId: 'outer' });
  const completion = makeItem({ id: 'done', completionOf: 'nested', completionLaunch: launch });
  expect(itemTranscriptScope(completion, () => undefined)).toBe('outer');
  expect(itemTranscriptScope({ ...completion, completionLaunch: { ...launch, id: 'unrelated' } }, () => undefined)).toBe('');
  expect(itemTranscriptScope(completion, () => ({ ...launch, parentId: 'current' }))).toBe('current');
});
