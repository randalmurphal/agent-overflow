import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import {
  runMode,
  isClientMode,
  __resetRunModeForTest,
} from './runMode';

// The mode rides the page URL — the `--connect` stub stamps `mode=client`
// on the URL it hands its webview — so switching modes means rewriting
// the document URL and dropping the memoised read.
function setPageMode(mode: string | undefined): void {
  const search = mode === undefined ? '' : `?mode=${mode}`;
  window.history.replaceState(null, '', window.location.pathname + search);
  __resetRunModeForTest();
}

describe('runMode', () => {
  beforeEach(() => {
    setPageMode(undefined);
  });

  afterEach(() => {
    setPageMode(undefined);
  });

  it('defaults to local when the URL carries no mode', () => {
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('returns "client" for mode=client', () => {
    setPageMode('client');
    expect(runMode()).toBe('client');
    expect(isClientMode()).toBe(true);
  });

  it('returns "headless" for mode=headless', () => {
    setPageMode('headless');
    expect(runMode()).toBe('headless');
    expect(isClientMode()).toBe(false);
  });

  it('returns "local" for explicit mode=local', () => {
    setPageMode('local');
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('falls back to local for unknown mode strings', () => {
    // Forward-compat: a future shell that stamps a mode the SPA doesn't
    // recognise should not crash. The worst case is a panel that should
    // hide stays visible — strictly less harmful than a black screen.
    setPageMode('something-else');
    expect(runMode()).toBe('local');
    expect(isClientMode()).toBe(false);
  });

  it('falls back to local for an empty mode parameter', () => {
    setPageMode('');
    expect(runMode()).toBe('local');
  });

  it('memoises the first read until __resetRunModeForTest', () => {
    setPageMode('client');
    expect(runMode()).toBe('client');

    // Rewrite the URL without resetting the cache. The memoised value
    // wins until the test hook clears it.
    window.history.replaceState(null, '', window.location.pathname + '?mode=local');
    expect(runMode()).toBe('client');

    __resetRunModeForTest();
    expect(runMode()).toBe('local');
  });
});

// Run mode carries NOTHING about authorization. What a session may do
// arrives on the manifest and lives in ./scopes.ts, which owns its own
// suite; keeping the two apart is what lets a `--connect` client attached
// to a LOCAL backend do everything while its local-only settings panels
// still hide.
