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

  it('delegates to a lower-tier overlay even when opened after a higher-tier modal', () => {
    const unhost = registerAppShellBlurHost();
    const modal = acquireBackdropBlur('md', 60);
    expect(modal.delegatedToShell).toBe(true);
    expect(getAppShellBlur()).toBe('md');

    // OverlayShell (z-40) mounted beneath an already-open Modal (z-60) takes
    // ownership of the app-shell blur so the higher Modal restores native
    // backdrop-filter and blurs the OverlayShell beneath it.
    const overlay = acquireBackdropBlur('sm', 40);
    expect(overlay.delegatedToShell).toBe(true);
    expect(modal.delegatedToShell).toBe(false);
    expect(getAppShellBlur()).toBe('sm');

    overlay.release();
    expect(modal.delegatedToShell).toBe(true);
    expect(getAppShellBlur()).toBe('md');

    modal.release();
    expect(getAppShellBlur()).toBeNull();
    unhost();
  });
});
