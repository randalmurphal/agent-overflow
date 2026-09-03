import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearCachedDeviceIdForTest,
  getConnectionId,
  getDeviceId,
  isValidClientId,
  reresolveDeviceIdForTest,
} from './clientIdentity';

// These cases used to live in appStorage.test.ts, because the durable id was
// once that store's own. It is transport identity now — the WebSocket client
// puts it on the upgrade URL and the backend scopes the ui_state bucket by
// what it resolves there — so the cases live beside the module that owns it.

const CLIENT_ID_CACHE_KEY = 'agent-overflow:uistate:clientId';

describe('clientIdentity', () => {
  beforeEach(() => {
    localStorage.clear();
    clearCachedDeviceIdForTest();
  });

  it('mints and caches a device id when nothing provides one', () => {
    const id = getDeviceId();
    expect(id).toMatch(/^[A-Za-z0-9-]{8,64}$/);
    expect(localStorage.getItem(CLIENT_ID_CACHE_KEY)).toBe(id);
  });

  it('reuses the cached device id across a simulated same-origin reload', () => {
    const first = getDeviceId();
    reresolveDeviceIdForTest(); // module state resets, localStorage survives
    expect(getDeviceId()).toBe(first);
  });

  it('rejects a garbage cached device id and mints a fresh one', () => {
    localStorage.setItem(CLIENT_ID_CACHE_KEY, 'nope!bad id');
    reresolveDeviceIdForTest();
    expect(getDeviceId()).not.toBe('nope!bad id');
    expect(getDeviceId()).toMatch(/^[A-Za-z0-9-]{8,64}$/);
  });

  // The connection id is minted per page load and never persisted: two tabs
  // must not share one, which is what makes it the right key for "is this
  // frame the echo of my own change?".
  it('keeps the connection id stable within a page load and out of storage', () => {
    const first = getConnectionId();
    expect(first).toMatch(/^[A-Za-z0-9-]{8,64}$/);
    expect(getConnectionId()).toBe(first);
    expect(localStorage.getItem(CLIENT_ID_CACHE_KEY)).not.toBe(first);
  });

  it('bounds an id to the shape the backend accepts', () => {
    expect(isValidClientId('abcd1234')).toBe(true);
    expect(isValidClientId('a'.repeat(64))).toBe(true);
    expect(isValidClientId('short')).toBe(false);
    expect(isValidClientId('a'.repeat(65))).toBe(false);
    expect(isValidClientId('client:injected-scope')).toBe(false);
    expect(isValidClientId(undefined)).toBe(false);
  });
});
