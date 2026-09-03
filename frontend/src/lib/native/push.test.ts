// Which backends this phone has told where to reach it, and when.
//
// The registration table is the whole of what this seam decides, and
// every row of it is a bug somebody would only find on a device: a phone
// that registered once and never again after a machine was attached, a
// permission prompt that came back every time a backend changed, a
// rotated token only half the backends heard about, a detach that let go
// of the socket before it withdrew the registration behind it.
//
// The plugin is faked rather than reached. It exists only inside an APK,
// and what is under test is this module's decisions about it.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const seam = vi.hoisted(() => {
  return {
    /** Whether these tests are pretending to be inside the APK. */
    native: true,
    /** Null stands for an APK built before the plugin existed. */
    bridge: null as unknown,
    /** The backend `withBackendTarget` currently has pinned. */
    pinned: null as string | null,
    backends: [] as { id: string; home: boolean; name: string }[],
    backendListeners: new Set<() => void>(),
    registered: [] as { backend: string | null; platform: string; token: string }[],
    unregistered: [] as (string | null)[],
    /** Backend ids whose RPCs reject, standing for one unreachable machine. */
    unreachable: new Set<string>(),
  };
});

vi.mock('./platform', () => ({
  isNativeShell: () => seam.native,
  nativePlatform: () => (seam.native ? 'android' : 'web'),
}));

vi.mock('./plugins', () => ({
  pushPlugin: async () => seam.bridge,
}));

vi.mock('../stores/bindings', () => ({
  RegisterPushToken: async (platform: string, token: string) => {
    const backend = seam.pinned;
    if (backend !== null && seam.unreachable.has(backend)) throw new Error('unreachable');
    seam.registered.push({ backend, platform, token });
  },
  UnregisterPushToken: async () => {
    const backend = seam.pinned;
    if (backend !== null && seam.unreachable.has(backend)) throw new Error('unreachable');
    seam.unregistered.push(backend);
  },
}));

vi.mock('../transport/backends', () => ({
  attachedBackends: () => seam.backends,
  onBackendsChanged: (listener: () => void) => {
    seam.backendListeners.add(listener);
    return () => seam.backendListeners.delete(listener);
  },
  // The real one arms a pin that the next dispatch drains. The fake reads
  // it at the same moment the RPC would: synchronously, inside `issue`.
  withBackendTarget: <T>(backendId: string, issue: () => T): T => {
    seam.pinned = backendId;
    try {
      return issue();
    } finally {
      seam.pinned = null;
    }
  },
}));

import {
  __pushTokenForTest,
  startPushRegistration,
  stopPushRegistration,
  unregisterPushFrom,
  watchPushTaps,
} from './push';

const TOKEN = 'token-one';
const ROTATED = 'token-two';

interface FakeBridge {
  permission: { granted: boolean } | Error;
  token: { configured: boolean; token: string } | Error;
  pendingTap: { id?: string; target?: string } | Error;
  prompts: number;
  presented: unknown[];
  retracted: string[];
  listeners: { tap: ((tap: unknown) => void)[]; tokenRefresh: ((e: unknown) => void)[] };
  removals: number;
  requestPermission(): Promise<{ granted: boolean }>;
  getToken(): Promise<{ configured: boolean; token: string }>;
  present(options: unknown): Promise<void>;
  retract(options: { id: string }): Promise<void>;
  takePendingTap(): Promise<{ id?: string; target?: string }>;
  addListener(event: string, handler: (value: never) => void): Promise<{ remove: () => Promise<void> }>;
}

function fakeBridge(): FakeBridge {
  const bridge: FakeBridge = {
    permission: { granted: true },
    token: { configured: true, token: TOKEN },
    pendingTap: {},
    prompts: 0,
    presented: [],
    retracted: [],
    listeners: { tap: [], tokenRefresh: [] },
    removals: 0,
    async requestPermission() {
      bridge.prompts += 1;
      if (bridge.permission instanceof Error) throw bridge.permission;
      return bridge.permission;
    },
    async getToken() {
      if (bridge.token instanceof Error) throw bridge.token;
      return bridge.token;
    },
    async present(options: unknown) {
      bridge.presented.push(options);
    },
    async retract(options: { id: string }) {
      bridge.retracted.push(options.id);
    },
    async takePendingTap() {
      if (bridge.pendingTap instanceof Error) throw bridge.pendingTap;
      return bridge.pendingTap;
    },
    async addListener(event: string, handler: (value: never) => void) {
      const list = event === 'tap' ? bridge.listeners.tap : bridge.listeners.tokenRefresh;
      list.push(handler as (value: unknown) => void);
      return {
        remove: async () => {
          bridge.removals += 1;
          const at = list.indexOf(handler as (value: unknown) => void);
          if (at !== -1) list.splice(at, 1);
        },
      };
    },
  };
  return bridge;
}

let bridge: FakeBridge;

beforeEach(() => {
  bridge = fakeBridge();
  seam.native = true;
  seam.bridge = bridge;
  seam.pinned = null;
  seam.backends = [
    { id: '', home: true, name: 'desk' },
    { id: 'laptop', home: false, name: 'laptop' },
  ];
  seam.backendListeners.clear();
  seam.registered = [];
  seam.unregistered = [];
  seam.unreachable.clear();
  vi.spyOn(console, 'warn').mockImplementation(() => {});
  vi.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => {
  stopPushRegistration();
  vi.restoreAllMocks();
});

describe('startPushRegistration', () => {
  it('tells every attached backend, each pinned to itself', async () => {
    await startPushRegistration();
    expect(seam.registered).toEqual([
      { backend: '', platform: 'android', token: TOKEN },
      { backend: 'laptop', platform: 'android', token: TOKEN },
    ]);
    expect(__pushTokenForTest()).toBe(TOKEN);
  });

  it('does nothing at all off a shell, without reaching a plugin', async () => {
    seam.native = false;
    // A poisoned bridge, so a seam that consulted one would fail loudly
    // rather than pass by coincidence.
    seam.bridge = null;
    await startPushRegistration();
    expect(seam.registered).toEqual([]);
    expect(bridge.prompts).toBe(0);
  });

  it('does nothing on an APK built before the plugin existed', async () => {
    seam.bridge = null;
    await startPushRegistration();
    expect(seam.registered).toEqual([]);
  });

  it('asks for the permission once per boot after a refusal', async () => {
    bridge.permission = { granted: false };
    await startPushRegistration();
    await startPushRegistration();
    expect(bridge.prompts).toBe(1);
    expect(seam.registered).toEqual([]);
  });

  it('treats a build with no push configuration as a normal outcome', async () => {
    bridge.token = { configured: false, token: '' };
    await startPushRegistration();
    expect(seam.registered).toEqual([]);
    // Said once, at info. A person with no Firebase project has nothing
    // to fix and must not be shown a failure.
    expect(console.info).toHaveBeenCalledTimes(1);
    expect(console.warn).not.toHaveBeenCalled();
  });

  it('registers with a backend attached after boot', async () => {
    await startPushRegistration();
    seam.registered = [];
    seam.backends = [...seam.backends, { id: 'server', home: false, name: 'server' }];
    for (const listener of seam.backendListeners) listener();
    await vi.waitFor(() => expect(seam.registered).toHaveLength(3));
    expect(seam.registered.map((row) => row.backend)).toEqual(['', 'laptop', 'server']);
  });

  it('re-registers everywhere when the token rotates, and ignores an unchanged one', async () => {
    await startPushRegistration();
    seam.registered = [];
    for (const handler of bridge.listeners.tokenRefresh) handler({ token: TOKEN });
    for (const handler of bridge.listeners.tokenRefresh) handler({ token: '' });
    expect(seam.registered).toEqual([]);

    for (const handler of bridge.listeners.tokenRefresh) handler({ token: ROTATED });
    await vi.waitFor(() => expect(seam.registered).toHaveLength(2));
    expect(seam.registered.every((row) => row.token === ROTATED)).toBe(true);
  });

  it('keeps telling the other backends when one is unreachable', async () => {
    seam.unreachable.add('');
    await startPushRegistration();
    expect(seam.registered).toEqual([{ backend: 'laptop', platform: 'android', token: TOKEN }]);
  });

  it('drops its subscriptions and its token when stopped', async () => {
    await startPushRegistration();
    stopPushRegistration();
    expect(__pushTokenForTest()).toBe('');
    expect(bridge.removals).toBe(1);
    expect(seam.backendListeners.size).toBe(0);
  });
});

describe('unregisterPushFrom', () => {
  it('withdraws this phone from one backend, pinned to it', async () => {
    await startPushRegistration();
    await unregisterPushFrom('laptop');
    expect(seam.unregistered).toEqual(['laptop']);
  });

  it('never throws when the backend being detached cannot be reached', async () => {
    seam.unreachable.add('laptop');
    await expect(unregisterPushFrom('laptop')).resolves.toBeUndefined();
    expect(seam.unregistered).toEqual([]);
  });

  it('is inert off a shell', async () => {
    seam.native = false;
    await unregisterPushFrom('laptop');
    expect(seam.unregistered).toEqual([]);
  });
});

describe('watchPushTaps', () => {
  it('delivers the tap this launch started with, and every later one', async () => {
    const seen: unknown[] = [];
    bridge.pendingTap = { id: 'thread:1', target: '{"kind":"thread","threadId":"t-1"}' };
    const stop = await watchPushTaps((target) => seen.push(target));
    await vi.waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0]).toEqual({ kind: 'thread', threadId: 't-1' });

    for (const handler of bridge.listeners.tap) {
      handler({ id: 'thread:2', target: '{"kind":"thread","threadId":"t-2"}' });
    }
    expect(seen[1]).toEqual({ kind: 'thread', threadId: 't-2' });
    stop();
    expect(bridge.listeners.tap).toHaveLength(0);
  });

  it('says nothing when this launch did not start from a tap', async () => {
    const seen: unknown[] = [];
    await watchPushTaps((target) => seen.push(target));
    expect(seen).toEqual([]);
  });

  it('survives a route it cannot read rather than taking the launch down', async () => {
    const seen: unknown[] = [];
    bridge.pendingTap = { id: 'thread:1', target: 'not json' };
    await watchPushTaps((target) => seen.push(target));
    expect(seen).toEqual([]);
    expect(console.warn).toHaveBeenCalled();
  });

  it('is inert off a shell', async () => {
    seam.native = false;
    const seen: unknown[] = [];
    bridge.pendingTap = { id: 'thread:1', target: '{}' };
    await watchPushTaps((target) => seen.push(target));
    expect(seen).toEqual([]);
  });
});
