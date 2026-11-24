// Imported by the element leavesElement.fixture.ts leaves, as the document
// reset removes it: evaluates while that file retires.
import { fixtureState, listen } from './fixtureState';

fixtureState().teardownEvaluations += 1;
listen('teardownModule');
