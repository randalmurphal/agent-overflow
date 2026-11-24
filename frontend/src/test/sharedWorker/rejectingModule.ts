// Fails to evaluate once the gate opens.
import { fixtureState } from './fixtureState';

await fixtureState().rejectGate.opened;
throw new Error('the rejecting module failed after the last test');
