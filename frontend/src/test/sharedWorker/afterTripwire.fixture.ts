import { expect, it } from 'vitest';
import { expectOwnListenerAndTimer } from './fixtureState';

it('starts without the listener that was released at once', async () => {
  expect((await expectOwnListenerAndTimer('afterTripwire')).outlived).toBe(0);
});
