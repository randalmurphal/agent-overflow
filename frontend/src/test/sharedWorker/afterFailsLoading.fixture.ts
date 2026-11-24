import { expect, it } from 'vitest';
import { expectOwnListenerAndTimer } from './fixtureState';

it('starts without the listener of the file that failed while loading, with its own working', async () => {
  expect((await expectOwnListenerAndTimer('afterFailsLoading')).failsLoading).toBe(0);
});
