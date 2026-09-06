// The seam between "this page's origin" and "the home backend's".
//
// Two properties carry the whole file. With no endpoint set, every
// function is the identity — that is what makes the desktop, `--connect`
// and a paired browser issue byte-identical requests to the ones they
// issued before this module existed. With one set, a relative path
// becomes an absolute one on the endpoint and nothing else moves.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetHomeEndpointForTest,
  forgetBackendEndpoint,
  backendTransferUrl,
  hasHomeEndpoint,
  homeCredentials,
  homeEndpoint,
  homeOriginParts,
  homeUrl,
  homeWsUrl,
  setHomeEndpoint,
  storeBackendEndpoint,
  storedBackendEndpoint,
  storedBackendEndpoints,
} from './homeEndpoint';

const ENDPOINT = 'https://desk.tail-scale.ts.net:7777';

beforeEach(() => {
  __resetHomeEndpointForTest();
  localStorage.clear();
});

afterEach(() => {
  __resetHomeEndpointForTest();
  localStorage.clear();
});

describe('with no endpoint set', () => {
  it('is the identity for every URL it is given', () => {
    expect(homeEndpoint()).toBe('');
    expect(hasHomeEndpoint()).toBe(false);
    expect(homeUrl('/bootstrap.json')).toBe('/bootstrap.json');
    expect(homeUrl('/attachments/upload?ticket=abc')).toBe('/attachments/upload?ticket=abc');
    expect(homeWsUrl('ws://127.0.0.1:5173/ws')).toBe('ws://127.0.0.1:5173/ws');
  });

  it('keeps the same-origin credentials mode every existing boot depends on', () => {
    expect(homeCredentials()).toBe('same-origin');
  });

  it('answers this document as the origin a manifest may describe', () => {
    expect(homeOriginParts()).toEqual({
      protocol: window.location.protocol,
      host: window.location.host,
    });
  });
});

describe('with an endpoint set', () => {
  beforeEach(() => {
    setHomeEndpoint(ENDPOINT);
  });

  it('carries every home route onto it and drops the cookie', () => {
    expect(hasHomeEndpoint()).toBe(true);
    expect(homeUrl('/bootstrap.json')).toBe(`${ENDPOINT}/bootstrap.json`);
    expect(homeUrl('/auth/pair')).toBe(`${ENDPOINT}/auth/pair`);
    expect(homeUrl('/attachments/upload?ticket=abc'))
      .toBe(`${ENDPOINT}/attachments/upload?ticket=abc`);
    expect(homeCredentials()).toBe('omit');
    expect(homeOriginParts()).toEqual({ protocol: 'https:', host: 'desk.tail-scale.ts.net:7777' });
  });

  it('leaves an already absolute URL alone rather than prefixing an origin onto one', () => {
    expect(homeUrl('https://elsewhere.test/thing')).toBe('https://elsewhere.test/thing');
  });

  it('keeps only the origin of what the shell declared', () => {
    setHomeEndpoint('https://desk.test:7777/some/path?q=1#frag');
    expect(homeEndpoint()).toBe('https://desk.test:7777');
  });

  it('refuses an endpoint that names nowhere to present a credential', () => {
    expect(() => setHomeEndpoint('desk.test:7777')).toThrow();
    expect(() => setHomeEndpoint('file:///tmp/app')).toThrow(/scheme not http/);
  });
});

describe('homeWsUrl', () => {
  it('rewrites a relative wsUrl onto the endpoint, scheme from the endpoint', () => {
    setHomeEndpoint(ENDPOINT);
    expect(homeWsUrl('/ws')).toBe('wss://desk.tail-scale.ts.net:7777/ws');
    setHomeEndpoint('http://192.168.1.4:7777');
    expect(homeWsUrl('/ws')).toBe('ws://192.168.1.4:7777/ws');
  });

  it('rewrites a wsUrl naming the PAGE host, which is the one host that cannot be right', () => {
    setHomeEndpoint(ENDPOINT);
    const onPage = `ws://${window.location.host}/ws?did=abc`;
    expect(homeWsUrl(onPage)).toBe('wss://desk.tail-scale.ts.net:7777/ws?did=abc');
  });

  it('leaves another machine alone: an attached backend owns its own endpoint', () => {
    setHomeEndpoint(ENDPOINT);
    expect(homeWsUrl('wss://laptop.tail-scale.ts.net:7777/ws'))
      .toBe('wss://laptop.tail-scale.ts.net:7777/ws');
  });
});

describe('the stored endpoint map', () => {
  it('holds one origin per backend, home under the empty key', () => {
    storeBackendEndpoint('', ENDPOINT);
    storeBackendEndpoint('b-1', 'https://laptop.test:7777');
    expect(storedBackendEndpoints()).toEqual({
      '': ENDPOINT,
      'b-1': 'https://laptop.test:7777',
    });
    expect(storedBackendEndpoint('b-1')).toBe('https://laptop.test:7777');
    expect(storedBackendEndpoint('never-paired')).toBe('');
  });

  it('drops an entry it cannot read rather than connecting somewhere unintended', () => {
    localStorage.setItem(
      'agent-overflow:backendEndpoints',
      JSON.stringify({ '': ENDPOINT, 'b-1': 'not a url', 'b 2': 'https://ok.test' }),
    );
    // The damaged origin and the id carrying a space both drop; the rest
    // of the map still works, which is the point of validating per entry.
    expect(storedBackendEndpoints()).toEqual({ '': ENDPOINT });
  });

  it('survives a storage that is not there at all', () => {
    localStorage.setItem('agent-overflow:backendEndpoints', '{ not json');
    expect(storedBackendEndpoints()).toEqual({});
  });

  it('forgets one machine without touching the others', () => {
    storeBackendEndpoint('', ENDPOINT);
    storeBackendEndpoint('b-1', 'https://laptop.test:7777');
    forgetBackendEndpoint('b-1');
    expect(storedBackendEndpoints()).toEqual({ '': ENDPOINT });
  });
});

describe('the shell door', () => {
  it('is read once at module load, and an unusable value is warned about, not thrown', async () => {
    // A fresh module registry, so the module-load read runs again with the
    // global this test stages. The bundle must still evaluate on a bad
    // value: a throw here would take the error surface down with it.
    vi.resetModules();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    (globalThis as { __aoHomeEndpoint?: string }).__aoHomeEndpoint = 'nonsense';
    let mod = await import('./homeEndpoint');
    expect(mod.homeEndpoint()).toBe('');
    expect(warn).toHaveBeenCalled();

    vi.resetModules();
    (globalThis as { __aoHomeEndpoint?: string }).__aoHomeEndpoint = ENDPOINT;
    mod = await import('./homeEndpoint');
    expect(mod.homeEndpoint()).toBe(ENDPOINT);

    delete (globalThis as { __aoHomeEndpoint?: string }).__aoHomeEndpoint;
    warn.mockRestore();
    vi.resetModules();
  });
});


describe('attachment transfer boundaries', () => {
  it.each([
    'https://elsewhere.test/attachments/upload?ticket=x',
    '//elsewhere.test/attachments/upload?ticket=x',
    '/attachments/../auth/refresh?ticket=x',
    '/attachments/%2e%2e/auth/refresh?ticket=x',
    '/attachments/a%2fb/c?ticket=x',
    '/attachments/upload#secret',
  ])('rejects a minted path outside the transfer routes: %s', (path) => {
    expect(() => backendTransferUrl(path, 'gpu')).toThrow();
  });
});
