// The socket's half of the phone tray: which sends become a notification,
// and under what tag.
//
// Two rules carry the whole design and both are asymmetric, which is
// exactly the kind of thing that gets "simplified" back out. A
// PRESENTATION is background-only, because a tray notification over an
// app somebody is looking at is the double notification the design
// exists to avoid. A RETRACTION is never gated: what it withdraws was
// posted while the app was away, and coming forward does not take it off
// the tray.
//
// The tag is the third: `<backendId>|<id>` for every backend, home
// included, because notification ids are not unique across machines and
// because the pushed path composes the same tag from its own `backend`
// key — so the two paths collide on purpose for one moment and never
// across two machines.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const seam = vi.hoisted(() => ({
  native: true,
  bridge: null as unknown,
  lease: 'background' as 'active' | 'background' | 'suspended',
  subscriptions: [] as {
    name: string;
    handler: (data: unknown, origin: { backendId: string }) => void;
  }[],
  cancels: 0,
}));

vi.mock('../native/platform', () => ({
  isNativeShell: () => seam.native,
  nativePlatform: () => (seam.native ? 'android' : 'web'),
}));

vi.mock('../native/plugins', () => ({
  pushPlugin: async () => seam.bridge,
}));

vi.mock('../transport/lease', () => ({
  clientLease: () => seam.lease,
}));

vi.mock('./wailsEvents', () => ({
  wailsEventOn: (
    name: string,
    handler: (data: unknown, origin: { backendId: string }) => void,
  ) => {
    seam.subscriptions.push({ name, handler });
    return () => {
      seam.cancels += 1;
    };
  },
}));

import { pushTag, startPushPresenter, stopPushPresenter } from './pushPresenter.svelte';

interface FakeTray {
  presented: { id: string; kind: string; title: string; body: string; target: string }[];
  retracted: string[];
  present(options: { id: string; kind: string; title: string; body: string; target: string }): Promise<void>;
  retract(options: { id: string }): Promise<void>;
}

function fakeTray(): FakeTray {
  const tray: FakeTray = {
    presented: [],
    retracted: [],
    async present(options) {
      tray.presented.push(options);
    },
    async retract(options) {
      tray.retracted.push(options.id);
    },
  };
  return tray;
}

let tray: FakeTray;

/** Deliver one send the way the transport would, from one backend. */
async function deliver(send: unknown, backendId = 'home-9'): Promise<void> {
  for (const sub of seam.subscriptions) sub.handler(send, { backendId });
  // The subscription's handler starts the presentation and does not await
  // it, the same way the transport does not await a listener.
  await Promise.resolve();
  await Promise.resolve();
}

beforeEach(() => {
  tray = fakeTray();
  seam.native = true;
  seam.bridge = tray;
  seam.lease = 'background';
  seam.subscriptions = [];
  seam.cancels = 0;
  vi.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => {
  stopPushPresenter();
  vi.restoreAllMocks();
});

describe('startPushPresenter', () => {
  it('presents a backgrounded send on the tray', async () => {
    await startPushPresenter();
    await deliver({
      id: 'thread:t-1',
      kind: 'turn-complete',
      title: 'Fix the parser',
      body: 'Turn complete',
      target: { kind: 'thread', threadId: 't-1' },
    });
    expect(tray.presented).toEqual([
      {
        id: 'home-9|thread:t-1',
        kind: 'turn-complete',
        title: 'Fix the parser',
        body: 'Turn complete',
        target: '{"kind":"thread","threadId":"t-1"}',
      },
    ]);
  });

  it('presents nothing while the person is looking at the app', async () => {
    await startPushPresenter();
    seam.lease = 'active';
    await deliver({ id: 'thread:t-1', kind: 'turn-complete' });
    expect(tray.presented).toEqual([]);
  });

  it('retracts even in the foreground, because the tray still holds it', async () => {
    await startPushPresenter();
    seam.lease = 'active';
    await deliver({ id: 'thread:t-1', retract: true });
    expect(tray.retracted).toEqual(['home-9|thread:t-1']);
  });

  it('keeps two machines apart on one tray, and an unknown origin bare', async () => {
    await startPushPresenter();
    await deliver({ id: 'provider-auth:claude', kind: 'provider-signed-out' }, 'home-9');
    await deliver({ id: 'provider-auth:claude', kind: 'provider-signed-out' }, 'laptop');
    await deliver({ id: 'provider-auth:claude', kind: 'provider-signed-out' }, '');
    expect(tray.presented.map((row) => row.id)).toEqual([
      'home-9|provider-auth:claude',
      'laptop|provider-auth:claude',
      'provider-auth:claude',
    ]);
  });

  it('ignores a send with no id, which nothing could ever retract', async () => {
    await startPushPresenter();
    await deliver({ kind: 'turn-complete', title: 'nameless' });
    await deliver({ id: 7, kind: 'turn-complete' });
    expect(tray.presented).toEqual([]);
  });

  it('sends an absent target as the empty string rather than the word undefined', async () => {
    await startPushPresenter();
    await deliver({ id: 'app-update', kind: 'app-update', title: 'Update', body: 'Ready' });
    expect(tray.presented[0]?.target).toBe('');
  });

  it('subscribes nothing off a shell', async () => {
    seam.native = false;
    await startPushPresenter();
    expect(seam.subscriptions).toHaveLength(0);
  });

  it('subscribes nothing on an APK built before the plugin existed', async () => {
    seam.bridge = null;
    await startPushPresenter();
    expect(seam.subscriptions).toHaveLength(0);
  });

  it('subscribes once however many times it is started', async () => {
    await startPushPresenter();
    await startPushPresenter();
    expect(seam.subscriptions).toHaveLength(1);
  });

  it('drops the subscription when stopped, and presents nothing after', async () => {
    await startPushPresenter();
    stopPushPresenter();
    expect(seam.cancels).toBe(1);
    await deliver({ id: 'thread:t-1', kind: 'turn-complete' });
    expect(tray.presented).toEqual([]);
  });
});

describe('pushTag', () => {
  it('is the backend and the id, the spelling the pushed message and the renderer share', () => {
    expect(pushTag('thread:t-1', 'home-9')).toBe('home-9|thread:t-1');
    expect(pushTag('provider-auth:claude', 'laptop')).toBe('laptop|provider-auth:claude');
  });

  it('keeps the bare id only when the origin is unknown', () => {
    expect(pushTag('thread:t-1', '')).toBe('thread:t-1');
  });
});
