// Starts an import it does not wait for. The module it imports evaluates
// only after the gate opens, on a timer this file's afterAll sets: after
// the file's last hook, and before its timers are released only if its
// retirement waits for the import.
import { afterAll, it } from 'vitest';
import { fixtureState } from './fixtureState';

it('starts an import it does not wait for', () => {
  void import('./lateModule');
});

afterAll(() => {
  setTimeout(() => fixtureState().openGate(), 0);
});
