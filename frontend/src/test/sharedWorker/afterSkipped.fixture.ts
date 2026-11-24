import { expect, it } from 'vitest';
import { expectOwnListenerAndTimer } from './fixtureState';

it("starts without the skipped file's listener, with its own working", async () => {
  expect((await expectOwnListenerAndTimer('afterSkipped')).skipped).toBe(0);
});
