// Evaluates only once the gate opens, then adds a listener and has a reset
// that runs between tests.
import { fixtureState, listen } from './fixtureState';

const state = fixtureState();
await state.lateGate.opened;
state.lateEvaluations += 1;
const instance = state.lateEvaluations;
listen(`late${instance}`);

export function lateInstance(): number {
  return instance;
}

export function resetLateModuleForTest(): void {
  state.lateResets += 1;
}
