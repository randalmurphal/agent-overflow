// Collected after collectsListener.fixture.ts in its worker: fails to load
// if that file's listener is still active, or if it was not collected first.
import { it } from 'vitest';
import { fire } from './fixtureState';

const heard = fire().collected;
if (heard === undefined) throw new Error('collected before collectsListener.fixture.ts');
if (heard !== 0) throw new Error(`the previous collected file's listener heard ${heard} events, want 0`);

it('is collected, not run', () => {});
