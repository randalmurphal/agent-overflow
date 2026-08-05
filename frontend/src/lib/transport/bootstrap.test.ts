// Locality of the document origin decides whether a refused bootstrap
// credential is terminal (see wsClient's credentialDead latch): a page
// served over the network can only be re-credentialled by re-opening a
// share link, while a loopback page is handed a live token by the shell
// that launched it. Getting this predicate wrong in either direction is
// a user-visible bug — a false "remote" tells a desktop user to reopen a
// share link that does not exist, and a false "loopback" leaves a phone
// retrying a dead token forever.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  BootstrapRejectedError,
  defaultBootstrap,
  isLoopbackHostname,
  pageServedOverLoopback,
} from './bootstrap';

describe('isLoopbackHostname', () => {
  it('accepts every host that names this machine', () => {
    // The embedded webview loads 127.0.0.1 (cmd/agent-overflow-windows
    // builds exactly that URL); dev servers and the --connect stub use
    // localhost; ::1 arrives without brackets from location.hostname.
    for (const host of [
      '127.0.0.1',
      '127.0.0.53',
      '127.1.2.3',
      'localhost',
      'LOCALHOST',
      'app.localhost',
      '::1',
      '[::1]',
      '  localhost  ',
    ]) {
      expect(isLoopbackHostname(host), host).toBe(true);
    }
  });

  it('rejects hosts that came off the network', () => {
    for (const host of [
      '192.168.1.24', // the LAN share URL (internal/network)
      '10.0.0.5',
      '172.17.0.2',
      'desktop.tailnet.ts.net',
      'example.com',
      // Near-misses that must not read as loopback: a name that merely
      // contains the string, and an address outside 127.0.0.0/8.
      'notlocalhost',
      'localhost.evil.com',
      '128.0.0.1',
      '1.127.0.0',
      '',
    ]) {
      expect(isLoopbackHostname(host), host).toBe(false);
    }
  });
});

describe('pageServedOverLoopback', () => {
  // happy-dom serves tests from http://localhost/.
  it('reads the current document origin', () => {
    expect(pageServedOverLoopback()).toBe(true);
  });
});

describe('defaultBootstrap', () => {
  afterEach(() => {
    window.sessionStorage.clear();
    delete (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__;
    vi.unstubAllGlobals();
  });

  const INJECTED = {
    wsUrl: 'ws://desktop.tailnet.ts.net:8790/ws',
    token: 'tok-injected',
    mode: 'client',
    remote: true,
  };

  it('returns the injected manifest without fetching on first load', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const b = await defaultBootstrap();

    expect(b.token).toBe('tok-injected');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  // The `--connect` stub serves /bootstrap.json on the page's own
  // origin by probing the upstream from Go (CORS-free), so a
  // revalidation must NOT short-circuit on the injected global — the
  // fetch is the whole point.
  it('revalidates an injected manifest through the stub origin', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(INJECTED), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);

    const b = await defaultBootstrap({ revalidate: true });

    expect(fetchMock).toHaveBeenCalledWith(
      '/bootstrap.json?t=tok-injected',
      expect.objectContaining({ credentials: 'same-origin' }),
    );
    expect(b.wsUrl).toBe(INJECTED.wsUrl);
    expect(b.remote).toBe(true);
  });

  it('surfaces the stub-relayed refusal as BootstrapRejectedError', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    vi.stubGlobal('fetch', vi.fn(async () => new Response('not found', { status: 404 })));

    await expect(defaultBootstrap({ revalidate: true })).rejects.toBeInstanceOf(
      BootstrapRejectedError,
    );
  });

  it('keeps a stub-relayed 503 transient', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    vi.stubGlobal('fetch', vi.fn(async () => new Response('unreachable', { status: 503 })));

    const err = await defaultBootstrap({ revalidate: true }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(Error);
    expect(err).not.toBeInstanceOf(BootstrapRejectedError);
  });

  // Clearing on refusal would buy nothing (a re-presented stale token
  // 404s identically) while destroying the one copy that lets a page
  // reload recover from a refusal that wasn't real.
  it('keeps the stashed token when the server refuses it', async () => {
    window.sessionStorage.setItem('ao:bootstrap-token', 'stashed-token');
    const fetchMock = vi.fn(async () => new Response('not found', { status: 404 }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(defaultBootstrap()).rejects.toBeInstanceOf(BootstrapRejectedError);

    expect(fetchMock).toHaveBeenCalledWith(
      '/bootstrap.json?t=stashed-token',
      expect.objectContaining({ credentials: 'same-origin' }),
    );
    expect(window.sessionStorage.getItem('ao:bootstrap-token')).toBe('stashed-token');
  });
});
