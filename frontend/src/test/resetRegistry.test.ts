import { describe, expect, it } from 'vitest';
import { addToast, getToasts } from '../lib/stores/toast.svelte';
import { resetRegistry } from './resetRegistry';

// The tests in this file run in order: the second sees what the first left.
describe('between tests', () => {
  it('a test leaves a toast in the toast store', () => {
    addToast('error', 'left behind');
    expect(getToasts()).toHaveLength(1);
  });

  it('the next test starts with the store reset by its reset export', () => {
    expect(getToasts()).toEqual([]);
  });
});

describe('the modules a file loaded', () => {
  it('registers the stores this file imports and none it does not', () => {
    const keys = resetRegistry.keys();
    expect(keys).toContain('src/lib/stores/toast.svelte.ts');
    expect(keys).not.toContain('src/lib/stores/settings.svelte.ts');
    expect(keys).not.toContain('src/lib/stores/threads.svelte.ts');
  });
});
