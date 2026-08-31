// Locality of the document origin decides whether a refused bootstrap
// credential is terminal (see wsClient's credentialDead latch): a page
// served over the network can only be re-credentialled by re-opening a
// share link, while a loopback page is handed a fresh page URL by the
// shell that launched it. Getting this predicate wrong in either
// direction is a user-visible bug — a false "remote" tells a desktop
// user to reopen a share link that does not exist, and a false
// "loopback" leaves a phone retrying a dead session forever.

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
  it('rejects a non-ws scheme', () => {
    expect(() => validateWsUrl(`http://${window.location.host}/ws`)).toThrow(/scheme not ws\/wss/);
    expect(() => validateWsUrl('javascript:alert(1)')).toThrow();
    expect(() => validateWsUrl('nonsense')).toThrow(/invalid/);
  });

  it('accepts a same-origin wsUrl', () => {
    expect(() => validateWsUrl(SAME_ORIGIN_WS)).not.toThrow();
  });

  // There is no exemption left to get wrong. Every manifest the SPA can
  // receive is served by the page's own origin — including the
  // `--connect` stub's, which carries the socket to the remote backend
  // itself rather than pointing the page at it.
  it('refuses a cross-origin wsUrl on every path', () => {
    expect(() => validateWsUrl('ws://desktop.tailnet.ts.net:8790/ws')).toThrow(/not same-origin/);
  });
});

describe('defaultBootstrap', () => {
  // The page ticket lives in the URL, so each case stamps the URL it is
  // describing and the reset puts the document back on a bare path.
  function setPageSearch(search: string): void {
    window.history.replaceState(null, '', window.location.pathname + search);
  }

  afterEach(() => {
    setPageSearch('');
    vi.unstubAllGlobals();
  });

  function manifestResponse(body: unknown): Response {
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  }

  it('spends the URL ticket on the manifest fetch and scrubs it', async () => {
    setPageSearch('?t=ticket-1&cid=desktop-1');
    const fetchMock = vi.fn(async () => manifestResponse({ wsUrl: SAME_ORIGIN_WS }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(defaultBootstrap()).resolves.toMatchObject({ wsUrl: SAME_ORIGIN_WS });

    expect(fetchMock).toHaveBeenCalledWith(
      '/bootstrap.json?t=ticket-1',
      expect.objectContaining({ credentials: 'same-origin' }),
    );
    // Only the ticket is removed; the parameters the shell stamped for
    // the SPA to read (client identity, run mode) survive.
    expect(window.location.search).toBe('?cid=desktop-1');
  });

  // The refetch after a run of connect failures carries nothing: the
  // ticket is spent, and the session cookie the browser attaches is the
  // whole credential. A fetch that still demanded a ticket would make
  // every reconnect refetch a refusal.
  it('fetches without a ticket once the URL has none', async () => {
    const fetchMock = vi.fn(async () => manifestResponse({ wsUrl: SAME_ORIGIN_WS }));
    vi.stubGlobal('fetch', fetchMock);

    await expect(defaultBootstrap()).resolves.toMatchObject({ wsUrl: SAME_ORIGIN_WS });

    expect(fetchMock).toHaveBeenCalledWith(
      '/bootstrap.json',
      expect.objectContaining({ credentials: 'same-origin' }),
    );
  });

  // The hazard the origin check exists for: the manifest's wsUrl must
  // name the origin that served the page. A manifest naming somewhere
  // else was tampered with in flight, and honouring it would point the
  // page's socket at another authority.
  it('refuses a cross-origin wsUrl from the fetched manifest', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => manifestResponse({ wsUrl: 'ws://evil.example.com/ws' })));

    await expect(defaultBootstrap()).rejects.toThrow(/not same-origin/);
  });

  it('accepts a same-origin wsUrl from the fetched manifest', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => manifestResponse({ wsUrl: SAME_ORIGIN_WS })));

    await expect(defaultBootstrap()).resolves.toMatchObject({ wsUrl: SAME_ORIGIN_WS });
  });

  it('surfaces a refusal as BootstrapRejectedError', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('not found', { status: 404 })));

    await expect(defaultBootstrap()).rejects.toBeInstanceOf(BootstrapRejectedError);
  });

  // 503 is the readiness gate, not a verdict on the credential: the
  // server has already issued the cookie by the time it answers one.
  it('keeps a 503 transient', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('not ready', { status: 503 })));

    const err = await defaultBootstrap().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(Error);
    expect(err).not.toBeInstanceOf(BootstrapRejectedError);
  });

  it('rejects a manifest without a wsUrl', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => manifestResponse({ launchId: 'launch-1' })));

    await expect(defaultBootstrap()).rejects.toThrow(/missing wsUrl/);
  });
});
