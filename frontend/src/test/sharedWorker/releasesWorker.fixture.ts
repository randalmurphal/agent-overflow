// The last fixture: releases the worker holdsWorker.fixture.ts holds.
import { it } from 'vitest';
import { fixturesFinished } from './fixtureState';

it('releases the held worker', () => fixturesFinished());
