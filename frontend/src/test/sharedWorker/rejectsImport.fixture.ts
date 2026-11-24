// Starts an import it does not wait for, which rejects after the file's
// last hook, on a timer its afterAll sets.
import { afterAll, it } from 'vitest';
import { fixtureState } from './fixtureState';

it('starts an import it does not wait for', () => {
  void import('./rejectingModule');
});

afterAll(() => {
  setTimeout(() => fixtureState().rejectGate.open(), 0);
});
