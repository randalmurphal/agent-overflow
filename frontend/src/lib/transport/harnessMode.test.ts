import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetHarnessModeForTest,
  harnessPageMarker,
  setHarnessPageMarkerFromBootstrap,
  isHarnessSession,
  setHarnessSessionFromBootstrap,
  whenHarnessSession,
} from './harnessMode';

beforeEach(() => {
  __resetHarnessModeForTest();
});

describe('harness session flag', () => {
  it('defaults to false so an ordinary boot arms nothing', () => {
    const arm = vi.fn();
    whenHarnessSession(arm);
    expect(isHarnessSession()).toBe(false);
    expect(arm).not.toHaveBeenCalled();
  });

  it('arms a waiter when the manifest resolves', () => {
    const arm = vi.fn();
    whenHarnessSession(arm);
    setHarnessSessionFromBootstrap(true);
    expect(isHarnessSession()).toBe(true);
    expect(arm).toHaveBeenCalledTimes(1);
  });

	it('makes the bootstrap page marker visible before harness waiters run', () => {
		let observed = '';
		whenHarnessSession(() => { observed = harnessPageMarker(); });
		setHarnessPageMarkerFromBootstrap('backend-page-marker');
		setHarnessSessionFromBootstrap(true);
		expect(observed).toBe('backend-page-marker');
	});

  it('arms immediately when the manifest already said so', () => {
    setHarnessSessionFromBootstrap(true);
    const arm = vi.fn();
    whenHarnessSession(arm);
    expect(arm).toHaveBeenCalledTimes(1);
  });

  // A waiter fires exactly once. The manifest is refetched on every
  // reconnect revalidation, and a second arm would install a second
  // subscription and a second MutationObserver.
  it('arms each waiter once across repeated manifests', () => {
    const arm = vi.fn();
    whenHarnessSession(arm);
    setHarnessSessionFromBootstrap(true);
    setHarnessSessionFromBootstrap(true);
    expect(arm).toHaveBeenCalledTimes(1);
  });

  // The latch is what keeps a bridge from being silently disarmed by a
  // manifest that lost the field; a backend cannot stop being a harness
  // without restarting.
  it('latches true', () => {
    setHarnessSessionFromBootstrap(true);
    setHarnessSessionFromBootstrap(false);
    expect(isHarnessSession()).toBe(true);
  });

  it('lets a waiter cancel before the manifest arrives', () => {
    const arm = vi.fn();
    whenHarnessSession(arm)();
    setHarnessSessionFromBootstrap(true);
    expect(arm).not.toHaveBeenCalled();
  });
});
