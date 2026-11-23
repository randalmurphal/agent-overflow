// Holds its worker until afterLateImport.fixture.ts, the last fixture, is
// done, so every other fixture runs in the other worker, one file per run
// request, in order.
import { it } from 'vitest';
import { fixturesDone } from './fixtureState';

it('holds its worker until the last fixture is done', () => fixturesDone());
