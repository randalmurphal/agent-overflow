// Exercise the production bootstrap/auth/route/socket composition. Only native
// bridge I/O and browser fetch/WebSocket are replaced; route selection, health
// verification, HTTP streaming, renewal and URL validation remain real.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { NetworkPlugin } from '../native/networkPlugin';

const boundary = vi.hoisted(() => ({ plugin: null as NetworkPlugin | null }));
vi.mock('../native/platform', () => ({ isNativeShell: () => true }));
vi.mock('../native/networkPlugin', () => ({ networkPlugin: async () => boundary.plugin! }));

const ID = '11111111-2222-4333-8444-555555555555';
const LAN = 'https://192.168.1.55:60522';
const TAIL = 'https://agent-overflow.example.ts.net';
const PIN = `sha256:${'a'.repeat(64)}`;
const routes = [{ endpoint: LAN, certFingerprint: PIN }, { endpoint: TAIL }];
const requests: { origin: string; path: string; native: boolean; pin?: string; headers: Headers }[] = [];
const sockets: { url: string; pin?: string }[] = [];
let lanUp = true;
let tailUp = true;
let expired = false;
let renewals = 0;
let credential = 'original';

async function respond(url: string, method: string, headers: Headers, body: string, native: boolean, pin?: string): Promise<Response> {
  const target = new URL(url);
  requests.push({ origin: target.origin, path: target.pathname, native, pin, headers });
  if (target.origin === LAN) {
    expect(native).toBe(true); expect(pin).toBe(PIN);
    if (!lanUp) throw new TypeError('LAN unreachable');
  } else {
    expect(target.origin).toBe(TAIL);
    if (!tailUp) throw new TypeError('Tailnet unavailable');
  }
  if (target.pathname === '/healthz') {
    expect(native).toBe(true);
    expect(headers.has('X-AO-Session')).toBe(false);
    expect(headers.has('X-AO-Device-Key')).toBe(false);
    if (target.origin === TAIL) expect(pin).toBe('');
    return Response.json({ backendId: ID });
  }
  if (target.pathname === '/auth/token/recover') {
    expect(method).toBe('POST');
    expect(headers.get('X-AO-Device-Key')).toBeTruthy();
    const value = JSON.parse(body) as { refreshSecret: string; nextRefreshSecret: string };
    expect(value.refreshSecret).toBe('refresh-original');
    expect(value.nextRefreshSecret).toBeTruthy();
    renewals++; credential = 'renewed'; expired = false;
    return Response.json({ sessionId: 'same-session', credential, expiresAtMs: Date.now() + 3600000,
      refreshSecret: value.nextRefreshSecret, refreshExpiresAtMs: Date.now() + 86400000 });
  }
  expect(headers.get('X-AO-Session')).toBe(credential);
  expect(headers.get('X-AO-Device-Key')).toBeTruthy();
  if (expired) return new Response('', { status: 404 });
  if (target.pathname === '/auth/ticket') return Response.json({ ticket: 'fresh-ticket' });
  expect(target.pathname).toBe('/bootstrap.json');
  return Response.json({ backendId: ID, replicaGeneration: 'generation', backendName: 'Mac', routes,
    wsUrl: `${target.origin.replace('https:', 'wss:')}/ws`, remote: true });
}

beforeEach(() => {
  vi.resetModules(); localStorage.clear(); requests.length = sockets.length = 0;
  lanUp = true; tailUp = true; expired = false; renewals = 0; credential = 'original';
  const transfers = new Map<string, { url: string; method: string; headers: Headers; pin: string; body: string; response?: Response; read: boolean }>();
  boundary.plugin = {
    getCapabilities: async () => ({ computerRoutes: true }),
    httpStart: async (request) => { transfers.set(request.id, { ...request, headers: new Headers(request.headers), body: '', read: false }); },
    httpWrite: async ({ id, data }) => { transfers.get(id)!.body += atob(data); },
    httpHeaders: async ({ id }) => {
      const held = transfers.get(id)!;
      held.response = await respond(held.url, held.method, held.headers, held.body, true, held.pin);
      return { status: held.response.status, headers: Object.fromEntries(held.response.headers) };
    },
    httpRead: async ({ id }) => {
      const held = transfers.get(id)!;
      if (held.read) return { data: '' };
      held.read = true; return { data: btoa(await held.response!.text()) };
    },
    httpClose: async ({ id }) => { transfers.delete(id); },
    socketOpen: async ({ url, pin }) => { sockets.push({ url, pin }); },
    socketSend: async () => {}, socketAck: async () => {}, socketClose: async () => {},
    addListener: async () => ({ remove: async () => {} }),
  };
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init);
    return respond(request.url, request.method, request.headers, await request.text(), false);
  }));
  vi.stubGlobal('WebSocket', class extends EventTarget {
    constructor(url: string) { super(); sockets.push({ url }); }
    close(): void {}
  });
});
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });

for (const backend of ['', ID]) {
  it(`keeps LAN-learned tailnet routes through reload and renewal (${backend ? 'attached' : 'home'})`, async () => {
    const trust = await import('../native/networkTrust');
    const endpoint = await import('./homeEndpoint');
    const sessionKey = `agent-overflow:deviceSession${backend ? `:${backend}` : ''}`;
    trust.pairingEndpoint({ v: 1, backendId: ID, endpoint: LAN, certFingerprint: PIN, token: 'used' });
    endpoint.storeBackendEndpoint(backend, LAN);
    if (!backend) endpoint.setHomeEndpoint(LAN);
    localStorage.setItem(sessionKey, JSON.stringify({ backendId: ID, sessionId: 'same-session', credential,
      expiresAtMs: Date.now() + 3600000, refreshSecret: 'refresh-original', refreshRecovery: true, proofKind: 'bearer' }));
    async function load() {
      const boot = await import('./bootstrap');
      if (!backend) return boot.defaultBootstrap();
      const manifests = await import('./manifestBackends');
      return manifests.fetchBackendManifest({ id: backend, backendId: ID, name: 'Mac', nickname: '',
        wsUrl: `${LAN.replace('https:', 'wss:')}/ws`, bootstrapUrl: `${LAN}/bootstrap.json` });
    }
    const initial = await load();
    const initialSession = await import('./deviceSession');
    const initialTicket = await initialSession.mintDialTicket(undefined, backend);
    const initialNetwork = await import('./networkSocket');
    const initialSocket = initialNetwork.createNetworkSocket(`${initial.wsUrl}?ticket=${initialTicket}`, backend);
    await vi.waitFor(() => expect(sockets.at(-1)).toEqual({ url: `${LAN.replace('https:', 'wss:')}/ws?ticket=fresh-ticket`, pin: PIN }));
    initialSocket.close();
    expect(localStorage.getItem(`agent-overflow:computerRoutes:${encodeURIComponent(backend)}`)).toContain(TAIL);
    vi.resetModules(); lanUp = false;
    await expect(load()).rejects.toThrow('LAN unreachable');
    const away = await load();
    expect(away.wsUrl).toBe(`${TAIL.replace('https:', 'wss:')}/ws`);
    expired = true;
    const renewed = await load();
    expect(renewals).toBe(1);
    expect(JSON.parse(localStorage.getItem(sessionKey)!).sessionId).toBe('same-session');
    expect(localStorage.getItem(`agent-overflow:computerRoutes:${encodeURIComponent(backend)}`)).toContain(TAIL);
    const sessions = await import('./deviceSession');
    const ticket = await sessions.mintDialTicket(undefined, backend);
    expect(ticket).toBe('fresh-ticket');
    const { createNetworkSocket } = await import('./networkSocket');
    const socket = createNetworkSocket(`${renewed.wsUrl}?ticket=${ticket}`, backend);
    expect(sockets.at(-1)).toEqual({ url: `${TAIL.replace('https:', 'wss:')}/ws?ticket=fresh-ticket` });
    socket.close();
    const before = requests.length;
    vi.resetModules();
    await expect(load()).resolves.toMatchObject({ wsUrl: `${TAIL.replace('https:', 'wss:')}/ws` });
    expect(requests.slice(before).some((request) => request.origin === TAIL && request.path === '/healthz' && request.native)).toBe(true);
    expect(requests.filter((request) => request.origin === TAIL && request.path !== '/healthz').every((request) => !request.native && request.pin === undefined)).toBe(true);
    // Return home with the VPN unavailable: the explicit LAN port and its
    // private pin must both return, without moving the pairing or renewing it.
    lanUp = true; tailUp = false;
    await expect(load()).rejects.toThrow('Tailnet unavailable');
    const homeAgain = await load();
    expect(homeAgain.wsUrl).toBe(`${LAN.replace('https:', 'wss:')}/ws`);
    const homeSession = await import('./deviceSession');
    const homeTicket = await homeSession.mintDialTicket(undefined, backend);
    const homeNetwork = await import('./networkSocket');
    const homeSocket = homeNetwork.createNetworkSocket(`${homeAgain.wsUrl}?ticket=${homeTicket}`, backend);
    await vi.waitFor(() => expect(sockets.at(-1)).toEqual({ url: `${LAN.replace('https:', 'wss:')}/ws?ticket=fresh-ticket`, pin: PIN }));
    homeSocket.close();
    expect(renewals).toBe(1);
    expect(JSON.parse(localStorage.getItem(sessionKey)!).sessionId).toBe('same-session');
  });
}
