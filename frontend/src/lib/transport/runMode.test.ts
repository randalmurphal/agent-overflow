import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { runMode, isClientMode, __resetRunModeForTest } from './runMode';

interface InjectedBootstrap {
  mode?: unknown;
}

function setInjected(value: InjectedBootstrap | undefined): void {
  if (value === undefined) {
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
  } else {
    (globalThis as { __AO_BOOTSTRAP__?: InjectedBootstrap }).__AO_BOOTSTRAP__ = value;
  }
  __resetRunModeForTest();
}

describe('runMode', () => {
  beforeEach(() => {
    setInjected(undefined);
  });

  afterEach(() => {
    setInjected(undefined);
  });

  it('defaults to local when window.__AO_BOOTSTRAP__ is missing', () => {
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('returns "client" when bootstrap injects mode=client', () => {
    setInjected({ mode: 'client' });
    expect(runMode()).toBe('client');
    expect(isClientMode()).toBe(true);
  });

  it('returns "headless" when bootstrap injects mode=headless', () => {
    setInjected({ mode: 'headless' });
    expect(runMode()).toBe('headless');
    expect(isClientMode()).toBe(false);
  });

  it('returns "local" for explicit mode=local', () => {
    setInjected({ mode: 'local' });
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('falls back to local for unknown mode strings', () => {
    // Forward-compat: a future backend that emits a new mode the SPA
    // doesn't recognise should not crash. The worst case is a panel
    // that should hide stays visible — strictly less harmful than a
    // black-screen.
    setInjected({ mode: 'something-else' });
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('falls back to local when mode is non-string', () => {
    setInjected({ mode: 42 });
    expect(runMode()).toBe('local');
  });

  it('falls back to local when bootstrap is present but lacks mode', () => {
    setInjected({});
    expect(runMode()).toBe('local');
  });

  it('memoises the first read until __resetRunModeForTest', () => {
    setInjected({ mode: 'client' });
    expect(runMode()).toBe('client');

    // Rewrite the global without resetting the cache. The memoised
    // value wins until the test hook clears it.
    (globalThis as { __AO_BOOTSTRAP__?: InjectedBootstrap }).__AO_BOOTSTRAP__ = { mode: 'local' };
    expect(runMode()).toBe('client');

    __resetRunModeForTest();
    expect(runMode()).toBe('local');
  });
});
