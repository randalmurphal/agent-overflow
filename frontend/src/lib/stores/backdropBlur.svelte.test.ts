import { afterEach, describe, expect, it } from 'vitest';
import {
  acquireBackdropBlur,
  getAppShellBlur,
  registerAppShellBlurHost,
  resetBackdropBlurForTest,
} from './backdropBlur.svelte';

afterEach(() => {
  resetBackdropBlurForTest();
});

describe('backdropBlur store', () => {
  it('keeps native backdrop-filter when no app-shell host is registered', () => {
    const lease = acquireBackdropBlur('md');
    expect(lease.delegatedToShell).toBe(false);
    expect(getAppShellBlur()).toBeNull();
    lease.release();
  });

  it('delegates the bottom-most overlay to the app shell and keeps stacked modals native', () => {
    const unhost = registerAppShellBlurHost();
    const base = acquireBackdropBlur('sm');
    expect(base.delegatedToShell).toBe(true);
    expect(getAppShellBlur()).toBe('sm');

    const stacked = acquireBackdropBlur('md');
    expect(base.delegatedToShell).toBe(true);
    expect(stacked.delegatedToShell).toBe(false);
    expect(getAppShellBlur()).toBe('sm');

    // Releasing base promotes the stacked modal to own the shell blur.
    base.release();
    expect(base.delegatedToShell).toBe(false);
    expect(stacked.delegatedToShell).toBe(true);
    expect(getAppShellBlur()).toBe('md');

    stacked.release();
    expect(getAppShellBlur()).toBeNull();
    unhost();
  });
});
