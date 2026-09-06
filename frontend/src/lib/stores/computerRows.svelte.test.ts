import { afterEach, expect, it } from 'vitest';
import { tick } from 'svelte';
import { computerCatalog } from './computerRows';
import type { ProjectWithCounts } from '../types/models';

let dispose: (() => void) | undefined;
afterEach(() => { dispose?.(); dispose = undefined; });

it('does not subscribe a catalog loader to the fallback it replaces', async () => {
  let rows = $state.raw<ProjectWithCounts[]>([]);
  let loads = 0;
  dispose = $effect.root(() => {
    $effect(() => {
      computerCatalog('projects', () => rows, () => '');
      loads++;
    });
  });
  await tick();
  rows = [];
  await tick();
  expect(loads).toBe(1);
});
