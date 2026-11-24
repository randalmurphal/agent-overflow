// Collected without a run: adds a window listener as it loads.
import { it } from 'vitest';
import { listen } from './fixtureState';

listen('collected');

it('is collected, not run', () => {});
