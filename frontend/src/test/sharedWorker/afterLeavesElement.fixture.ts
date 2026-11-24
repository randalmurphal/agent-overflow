import { expect, it } from 'vitest';
import { expectOwnListenerAndTimer, fixtureState } from './fixtureState';

it("starts after the previous file's teardown import evaluated, without the teardown's listeners, with its own working", async () => {
  expect(fixtureState().teardownEvaluations, 'the teardown import settled before the previous file retired').toBe(1);
  const hits = await expectOwnListenerAndTimer('afterLeavesElement');
  expect({ teardown: hits.teardown, teardownModule: hits.teardownModule }).toEqual({ teardown: 0, teardownModule: 0 });
});
