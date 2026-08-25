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
  validateWsUrl,
  wsUrlMatchesPageOrigin,
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

// The transport builds its manifest's wsUrl from the inbound request's
// Host header (internal/transport/server.go deriveWSURL), so on every
// same-origin boot path — embedded webview, WSL launcher window, LAN
// browser, dev (the Go server proxies Vite, so the page origin is still
// the Go server's) — the answer names the exact authority the page was
// loaded from. Anything else in that manifest is a manifest that was
// tampered with in flight.
describe('wsUrlMatchesPageOrigin', () => {
  const httpPage = { protocol: 'http:', host: '127.0.0.1:34567' };
  const httpsPage = { protocol: 'https:', host: 'desktop.tailnet.ts.net' };

  it('accepts the URL the transport derives from the page Host header', () => {
    expect(wsUrlMatchesPageOrigin('ws://127.0.0.1:34567/ws', httpPage)).toBe(true);
    expect(wsUrlMatchesPageOrigin('wss://desktop.tailnet.ts.net/ws', httpsPage)).toBe(true);
  });

  it('rejects a different host', () => {
    expect(wsUrlMatchesPageOrigin('ws://evil.example.com/ws', httpPage)).toBe(false);
    expect(wsUrlMatchesPageOrigin('ws://127.0.0.2:34567/ws', httpPage)).toBe(false);
  });

  // Host alone is not the origin: a second server on the same machine is
  // a different security principal, and on a LAN bind it may not even be
  // ours.
  it('rejects a different port on the same host', () => {
    expect(wsUrlMatchesPageOrigin('ws://127.0.0.1:34568/ws', httpPage)).toBe(false);
    expect(wsUrlMatchesPageOrigin('ws://127.0.0.1/ws', httpPage)).toBe(false);
  });

  // A TLS-fronted page must not be downgraded to a cleartext socket, and
  // a plain-http page has no wss listener to reach.
  it('pairs the ws scheme with the page scheme', () => {
    expect(wsUrlMatchesPageOrigin('wss://127.0.0.1:34567/ws', httpPage)).toBe(false);
    expect(wsUrlMatchesPageOrigin('ws://desktop.tailnet.ts.net/ws', httpsPage)).toBe(false);
  });

  // ws:/http: and wss:/https: share default ports, so the URL parser
  // normalises an explicit default away on both sides and the two
  // spellings must still match.
  it('treats an explicit default port as the default port', () => {
    expect(wsUrlMatchesPageOrigin('ws://localhost:80/ws', { protocol: 'http:', host: 'localhost' })).toBe(true);
    expect(
      wsUrlMatchesPageOrigin('wss://example.com:443/ws', { protocol: 'https:', host: 'example.com' }),
    ).toBe(true);
  });

  it('rejects an unparseable url', () => {
    expect(wsUrlMatchesPageOrigin('not a url', httpPage)).toBe(false);
    expect(wsUrlMatchesPageOrigin('/ws', httpPage)).toBe(false);
  });
});

// SAME_ORIGIN_WS is what the transport's own manifest would carry for
// this document — derived rather than spelled out, since the environment
// picks the port the suite runs on.
const SAME_ORIGIN_WS = `ws://${window.location.host}/ws`;

describe('validateWsUrl', () => {
  it('rejects a non-ws scheme whatever the origin posture', () => {
    for (const requireSameOrigin of [true, false]) {
      expect(() => validateWsUrl(`http://${window.location.host}/ws`, requireSameOrigin)).toThrow(
        /scheme not ws\/wss/,
      );
      expect(() => validateWsUrl('javascript:alert(1)', requireSameOrigin)).toThrow();
      expect(() => validateWsUrl('nonsense', requireSameOrigin)).toThrow(/invalid/);
    }
  });

  it('accepts a same-origin wsUrl when the call site requires same-origin', () => {
    expect(() => validateWsUrl(SAME_ORIGIN_WS, true)).not.toThrow();
  });

  it('refuses a cross-origin wsUrl when the call site requires same-origin', () => {
    expect(() => validateWsUrl('ws://desktop.tailnet.ts.net:8790/ws', true)).toThrow(
      /not same-origin/,
    );
  });

  // The `--connect` remote client is legitimately cross-origin: the page
  // is served by the local stub and the socket goes to another machine.
  it('accepts a cross-origin wsUrl when the call site does not require same-origin', () => {
    expect(() => validateWsUrl('ws://desktop.tailnet.ts.net:8790/ws', false)).not.toThrow();
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

  // The trust anchor is the local stub, so a revalidation may confirm
  // the session but never retarget it: a manifest naming a different
  // wsUrl than the one injected at page load is refused (2026-08-25
  // security review, finding 7).
  it('rejects a revalidated manifest that names a different wsUrl', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    const retargeted = { ...INJECTED, wsUrl: 'ws://attacker.example:9999/ws' };
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify(retargeted), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(defaultBootstrap({ revalidate: true })).rejects.toThrow(
      /different wsUrl/,
    );
  });

  // REGRESSION GUARD for the same-origin check's exemption. The
  // `--connect` stub (internal/clientmode) injects this manifest into
  // the page it serves from loopback, and its wsUrl names the REMOTE
  // backend by construction. Requiring same-origin here — or inferring
  // the exemption from the manifest's own `mode` field, which a spoofed
  // manifest sets for free — kills remote-client boot outright.
  it('accepts the injected cross-origin manifest on first load (--connect)', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('fetch should not be called');
      }),
    );

    const b = await defaultBootstrap();

    expect(b.wsUrl).toBe(INJECTED.wsUrl);
    expect(b.mode).toBe('client');
  });

  // Same exemption on the other half of the --connect flow: the stub's
  // /bootstrap.json answers with its OWN manifest (clientmode
  // manifestJSON), so the reconnect revalidation sees the same remote
  // wsUrl through a fetch rather than the injected global.
  it('accepts the stub-relayed cross-origin manifest on revalidation', async () => {
    (globalThis as { __AO_BOOTSTRAP__?: unknown }).__AO_BOOTSTRAP__ = { ...INJECTED };
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify(INJECTED), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(defaultBootstrap({ revalidate: true })).resolves.toMatchObject({
      wsUrl: INJECTED.wsUrl,
    });
  });

  // The hazard the check exists for: nothing injected the manifest, so
  // the page is served by the transport itself and its wsUrl must name
  // that same origin. A manifest naming somewhere else was tampered with
  // in flight, and honouring it would hand the token to the attacker's
  // socket.
  it('refuses a cross-origin wsUrl from the fetched manifest', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ wsUrl: 'ws://evil.example.com/ws', token: 'tok' }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(defaultBootstrap()).rejects.toThrow(/not same-origin/);
  });

  it('accepts a same-origin wsUrl from the fetched manifest', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify({ wsUrl: SAME_ORIGIN_WS, token: 'tok' }), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      ),
    );

    await expect(defaultBootstrap()).resolves.toMatchObject({ wsUrl: SAME_ORIGIN_WS });
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
