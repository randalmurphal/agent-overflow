// Every test skipped or todo: Vitest runs no afterAll here.
import { it } from 'vitest';
import { listen } from './fixtureState';

listen('skipped');

it.skip('is skipped', () => {});
it.todo('is todo');
