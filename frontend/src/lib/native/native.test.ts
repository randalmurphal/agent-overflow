// The native seams, from the side every build except the APK sees.
//
// Two things are worth a test here and the rest is the platform's. The
// first is INERTNESS: this suite runs with no Capacitor bridge in the
// page, which is exactly the desktop's and the browser's condition, and
// every seam has to answer without touching a plugin — a seam that threw
// would take down `main.ts` before anything had mounted that could show
// an error. The second is `shouldLock`, which is a pure decision about
// two timestamps and is the only logic in this directory that is not a
// forwarding call.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { installNativeLifecycle } from './lifecycle';
import { DEFAULT_LOCK_WINDOW_MS, installAppLock, lockWindowMs, shouldLock } from './lock';
import { captureImage, pickFile } from './pickers';
import { isNativeShell, nativePlatform } from './platform';
import { appPlugin, biometricPlugin, bundlePlugin, scannerPlugin, unthenable } from './plugins';
import {
  confirmLaunchHealthy,
  reportBundleHealthy,
  startBundleSync,
  stopBundleSync,
} from './bundleSync';
import { scanPairingQr } from './qr';
import { adoptPairingEndpoint, onceUnlocked, prepareNativeShell } from './boot';
import { __resetHomeEndpointForTest, homeEndpoint } from '../transport/homeEndpoint';

describe('the web fallbacks', () => {
  it('answer no shell', () => {
    expect(isNativeShell()).toBe(false);
    expect(nativePlatform()).toBe('web');
  });

  it('answer no shell even when something on window merely looks like a bridge', () => {
    // The read is a feature test all the way down, so a global that
    // exists without the method is the same answer as no global at all.
    vi.stubGlobal('Capacitor', {});
    expect(isNativeShell()).toBe(false);
    vi.stubGlobal('Capacitor', {
      isNativePlatform: () => {
        throw new Error('bridge is mid-install');
      },
    });
    expect(isNativeShell()).toBe(false);
    vi.unstubAllGlobals();
  });

  it('load no plugin, because the guard runs before the import is issued', async () => {
    await expect(appPlugin()).resolves.toBeNull();
    await expect(biometricPlugin()).resolves.toBeNull();
    await expect(scannerPlugin()).resolves.toBeNull();
    // The registered one answers null through the same guard, and would
    // answer null anyway: `registerPlugin` is one of the stub's null
    // exports, and the seam type-tests it before calling.
    await expect(bundlePlugin()).resolves.toBeNull();
  });

  it('sync no bundle, and confirm nothing, without ever reaching a plugin', async () => {
    // The update channel is the one seam that runs unattended on a
    // timer's cadence, so its no-shell answer has to be a no-op that
    // subscribes to nothing rather than a subscription that never fires.
    const stop = await startBundleSync();
    expect(typeof stop).toBe('function');
    stop();
    // Neither of these may throw, and neither may hang: a browser has no
    // plugin to confirm a launch to, and `confirmLaunchHealthy` waits on
    // a hello only after it has one.
    await expect(reportBundleHealthy()).resolves.toBeUndefined();
    await expect(confirmLaunchHealthy()).resolves.toBeUndefined();
    stopBundleSync();
  });

  it('scan nothing and pick nothing', async () => {
    await expect(scanPairingQr()).resolves.toBeNull();
    await expect(pickFile()).resolves.toBeNull();
    await expect(captureImage()).resolves.toBeNull();
  });

  it('install a lifecycle that listens to nothing and a lock that is open', async () => {
    const dispose = await installNativeLifecycle();
    expect(typeof dispose).toBe('function');
    dispose();

    const changed = vi.fn();
    const lock = await installAppLock({ onChange: changed });
    // Open, silent, and disposable. A browser that showed a lock screen
    // would be showing one nothing could ever satisfy.
    expect(lock.locked()).toBe(false);
    await expect(lock.unlock()).resolves.toBe(true);
    expect(changed).not.toHaveBeenCalled();
    lock.dispose();
  });

  it('prepare a boot that is not a shell boot', () => {
    expect(prepareNativeShell()).toEqual({ shell: false, paired: false });
  });
});

describe('shouldLock', () => {
  const WINDOW = 5 * 60_000;

  it('locks a cold start, which is the case the gate exists for', () => {
    expect(shouldLock(null, 1_000_000, WINDOW)).toBe(true);
  });

  it('leaves a short trip out of the app alone', () => {
    const paused = 1_000_000;
    expect(shouldLock(paused, paused + 1, WINDOW)).toBe(false);
    expect(shouldLock(paused, paused + WINDOW - 1, WINDOW)).toBe(false);
  });

  it('locks once the window has elapsed, boundary included', () => {
    const paused = 1_000_000;
    expect(shouldLock(paused, paused + WINDOW, WINDOW)).toBe(true);
    expect(shouldLock(paused, paused + WINDOW * 10, WINDOW)).toBe(true);
  });

  it('locks every time for a window of zero, because that is what was asked for', () => {
    expect(shouldLock(1_000_000, 1_000_000, 0)).toBe(true);
    expect(shouldLock(1_000_000, 1_000_000, -1)).toBe(true);
  });

  it('locks when the clock moved backwards while the app was away', () => {
    // Ordinary on a phone that picked up network time. Erring toward one
    // prompt is the cheap direction; trusting the arithmetic is not.
    expect(shouldLock(2_000_000, 1_000_000, WINDOW)).toBe(true);
  });
});

describe('the lock window setting', () => {
  it('defaults to five minutes', () => {
    localStorage.removeItem('agent-overflow:lockWindowMs');
    expect(lockWindowMs()).toBe(DEFAULT_LOCK_WINDOW_MS);
  });

  it('reads a stored value', () => {
    localStorage.setItem('agent-overflow:lockWindowMs', '60000');
    expect(lockWindowMs()).toBe(60_000);
    localStorage.removeItem('agent-overflow:lockWindowMs');
  });

  it('never lets a damaged value turn the lock off', () => {
    for (const raw of ['', 'soon', 'NaN', '-5']) {
      localStorage.setItem('agent-overflow:lockWindowMs', raw);
      expect(lockWindowMs(), raw).toBe(DEFAULT_LOCK_WINDOW_MS);
    }
    localStorage.removeItem('agent-overflow:lockWindowMs');
  });
});

// Both doors into pairing on a shell (the scanned code and a `#pair=`
// hash) point the shell at the payload's endpoint through this one
// function, before the pairing screen's first request.
describe('adoptPairingEndpoint', () => {
  const payload = {
    v: 1 as const,
    token: 't',
    endpoint: 'https://desk.tail-scale.ts.net:7777',
    backendId: 'b-home',
  };

  afterEach(() => {
    __resetHomeEndpointForTest();
  });

  it('sets the home endpoint from the payload and answers no problem', () => {
    expect(adoptPairingEndpoint(payload)).toBe('');
    expect(homeEndpoint()).toBe('https://desk.tail-scale.ts.net:7777');
  });

  it('answers a sentence for a payload that names nowhere, and sets nothing', () => {
    expect(adoptPairingEndpoint({ ...payload, endpoint: 'nowhere' })).toMatch(/Ask for a new one/);
    expect(homeEndpoint()).toBe('');
  });
});


describe('a plugin proxy handed out by an accessor', () => {
  // Capacitor's registerPlugin proxy answers every property with a method
  // wrapper, `then` included, and that wrapper rejects. A promise resolved
  // with the bare proxy calls that `then` and never settles.
  const capacitorLike = () =>
    new Proxy(
      {},
      {
        get: (_target, prop) => (..._args: unknown[]) => {
          const rejected = Promise.reject(
            new Error(`"App.${String(prop)}()" is not implemented on android`),
          );
          // The promise machinery calls `then` on the bare proxy and drops
          // what it returns, so the rejection is otherwise unobserved.
          rejected.catch(() => {});
          return rejected;
        },
      },
    ) as { addListener(): Promise<unknown> };

  it('is not a thenable once shielded, so an async accessor can return it', async () => {
    const shielded = await (async () => unthenable(capacitorLike()))();
    expect(typeof shielded.addListener).toBe('function');
    await expect(shielded.addListener()).rejects.toThrow('addListener()');
  });

  it('would hang unshielded', async () => {
    const settled = await Promise.race([
      (async () => capacitorLike())().then(() => 'settled', () => 'settled'),
      new Promise<string>((resolve) => setTimeout(() => resolve('hung'), 50)),
    ]);
    expect(settled).toBe('hung');
  });
});

describe('onceUnlocked', () => {
  it('starts on the first open and never again, whatever the lock does after', () => {
    const start = vi.fn();
    const gate = onceUnlocked(start);
    gate(true);
    expect(start).not.toHaveBeenCalled();
    gate(false);
    expect(start).toHaveBeenCalledTimes(1);
    // A cover on a trip to another app, and the return from it.
    gate(true);
    gate(false);
    expect(start).toHaveBeenCalledTimes(1);
  });

  it('starts at once for a lock that is already open', () => {
    const start = vi.fn();
    onceUnlocked(start)(false);
    expect(start).toHaveBeenCalledTimes(1);
  });
});
