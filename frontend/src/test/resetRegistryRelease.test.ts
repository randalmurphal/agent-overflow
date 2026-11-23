import { expect, it, vi } from 'vitest';
import * as held from '../lib/stores/toast.svelte';

// Which module instances the registry resets after vi.resetModules(): the
// one the file's imports hold, after every test; each one a test loads,
// after that test; and of those, only the newest of the module afterwards.
type Toasts = typeof import('../lib/stores/toast.svelte');
let older: Toasts;
let newer: Toasts;

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
  for (const instance of [held, older, newer]) {
    expect(instance.getToasts()).toEqual([]);
    instance.addToast('error', 'second');
  }
});

it('then resets the held instance and the newest, and releases the older', () => {
  expect(held.getToasts()).toEqual([]);
  expect(newer.getToasts()).toEqual([]);
  expect(older.getToasts().map((toast) => toast.message)).toEqual(['second']);
});
