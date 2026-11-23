import { afterAll, expect, it } from 'vitest';
import { expectOwnListenerAndTimer, fixturesFinished, fixtureState } from './fixtureState';

const state = fixtureState();

afterAll(() => fixturesFinished());

it("starts after the previous file's import evaluated, without its listener, with its own working", async () => {
  expect(state.lateEvaluations, 'the previous file retired before its import evaluated').toBe(1);
  expect((await expectOwnListenerAndTimer('afterLateImport')).late1).toBe(0);
});

it('evaluates its own instance of the module', async () => {
  state.openGate();
  const late = await import('./lateModule');
  expect(late.lateInstance()).toBe(2);
  expect(state.lateResets).toBe(0);
});

it("runs only its own instance's reset", () => {
  expect(state.lateResets).toBe(1);
});
