import { setFlagsFromString } from 'node:v8';
import { runInNewContext } from 'node:vm';
import { expect, it, vi } from 'vitest';
import * as held from '../lib/stores/toast.svelte';

// Every module instance a file loaded is reset after every test while
// anything references it, the instances a test loads after
// vi.resetModules() included; one nothing references is dropped once it is
// collected.
type Toasts = typeof import('../lib/stores/toast.svelte');
let older: Toasts | undefined;
let newer: Toasts;

setFlagsFromString('--expose-gc');
const collect = runInNewContext('gc') as () => void;

// The tests run in order: each reads what the one before it left.
it('a test loads two more instances of a module the file imports', async () => {
  vi.resetModules();
  older = await import('../lib/stores/toast.svelte');
  vi.resetModules();
  newer = await import('../lib/stores/toast.svelte');
  expect(new Set([held, older, newer]).size).toBe(3);
  for (const instance of [held, older, newer]) instance.addToast('error', 'first');
});

it('resets every instance after the test that loaded it', () => {
  for (const instance of [held, older!, newer]) {
    expect(instance.getToasts()).toEqual([]);
    instance.addToast('error', 'second');
  }
});

it('resets every instance a test still holds after every later test', () => {
  for (const instance of [held, older!, newer]) expect(instance.getToasts()).toEqual([]);
});

it('does not keep an instance nothing references', async () => {
  const probe = new WeakRef(older!.getToasts);
  older = undefined;
  for (let round = 0; round < 20 && probe.deref() !== undefined; round += 1) {
    // A WeakRef target lives at least until the job that read it ends.
    await new Promise((done) => setTimeout(done, 0));
    collect();
  }
  expect(probe.deref()).toBeUndefined();
});
