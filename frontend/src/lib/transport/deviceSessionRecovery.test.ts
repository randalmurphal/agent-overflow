import 'fake-indexeddb/auto';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { clearDeviceKey, enrollDeviceKey } from './deviceKey';
import { hasPairedSession, renewPairedSession } from './deviceSession';

const key = 'agent-overflow:deviceSession';
const header = 'X-AO-Refresh-Recovery';
const initial = { sessionId: 'session', credential: 'old-credential', refreshSecret: 'old-secret', expiresAtMs: 1,
  refreshRecovery: true, proofKind: 'key', backendId: 'backend-a', futureField: { value: 42 } };
const read = () => JSON.parse(localStorage.getItem(key)!);
const save = (value: object) => localStorage.setItem(key, JSON.stringify(value));
const response = (next: string) => Response.json({ sessionId: 'session', credential: `access-${next}`,
  refreshSecret: next, expiresAtMs: Date.now() + 3600000 }, { headers: { [header]: '1' } });

beforeEach(async () => {
  localStorage.clear();
  await clearDeviceKey();
  await enrollDeviceKey();
  save(initial);
});
afterEach(() => vi.restoreAllMocks());

it('recovers a lost reply using the saved successor and a fresh signed proof', async () => {
  const proofs: string[] = [];
  let accepted = '';
  let first = true;
  const fetcher = vi.fn<typeof fetch>(async (_url, init) => {
    const body = JSON.parse(String(init?.body));
    proofs.push(new Headers(init?.headers).get('X-AO-Device-Key')!);
    expect(body.refreshSecret).toBe('old-secret');
    expect(body.nextRefreshSecret).toMatch(/^[\w-]{43}$/);
    expect(read().pendingNextSecret).toBe(body.nextRefreshSecret);
    if (first) { first = false; accepted = body.nextRefreshSecret; throw new TypeError('lost response'); }
    expect(body.nextRefreshSecret).toBe(accepted);
    return response(accepted);
  });
  expect(await renewPairedSession(fetcher)).toBe(false);
  expect(read().refreshSecret).toBe('old-secret');
  expect(await renewPairedSession(fetcher)).toBe(true);
  expect(read().refreshSecret).toBe(accepted);
  expect(read().pendingNextSecret).toBeUndefined();
  expect(read().futureField).toEqual({ value: 42 });
  expect(new Set(proofs).size).toBe(2);
});

it.each(['grant', 'refusal', 'network'])('ignores a late %s after another context advances storage', async (kind) => {
  let release!: () => void;
  let arrived!: () => void;
  const started = new Promise<void>((resolve) => { arrived = resolve; });
  const wait = new Promise<void>((resolve) => { release = resolve; });
  const fetcher = vi.fn<typeof fetch>(async (_url, init) => {
    const body = JSON.parse(String(init?.body));
    arrived();
    await wait;
    if (kind === 'network') throw new TypeError('lost response');
    if (kind === 'refusal') return Response.json({ reason: 'refresh_reused' }, { status: 401 });
    return response(body.nextRefreshSecret);
  });
  const renewal = renewPairedSession(fetcher);
  await started;
  const newer = { ...initial, refreshSecret: 'newest-secret', credential: 'newest-access', expiresAtMs: Date.now() + 3600000 };
  save(newer);
  release();
  expect(await renewal).toBe(true);
  expect(read()).toEqual(newer);
});

it('does not send a renewal when its recovery state cannot be saved', async () => {
  const original = localStorage.setItem.bind(localStorage);
  vi.spyOn(localStorage, 'setItem').mockImplementation(function (this: Storage, name, value) {
    if (name === key) throw new DOMException('full', 'QuotaExceededError');
    original(name, value);
  });
  const fetcher = vi.fn<typeof fetch>();
  expect(await renewPairedSession(fetcher)).toBe(false);
  expect(fetcher).not.toHaveBeenCalled();
  expect(read()).toEqual(initial);
});

it('retains recovery state after an invalid successor or a superseded response', async () => {
  expect(await renewPairedSession(async () => response('different-successor'))).toBe(false);
  const pending = read();
  expect(pending.pendingNextSecret).toBeTruthy();
  expect(await renewPairedSession(async () => Response.json({ reason: 'refresh_superseded' }, { status: 401 }))).toBe(false);
  expect(read()).toEqual(pending);
  expect(hasPairedSession()).toBe(true);
});

it.each([true, false])('negotiates an existing profile with recovery support %s', async (support) => {
  save({ ...initial, refreshRecovery: undefined });
  const fetcher = vi.fn<typeof fetch>(async (url, init) => {
    if (!init?.method) {
      expect(String(url).endsWith('/auth/token')).toBe(true);
      expect(new Headers(init?.headers).has('X-AO-Session')).toBe(false);
      return new Response(null, { status: 405, headers: support ? { [header]: '1' } : {} });
    }
    const body = JSON.parse(String(init?.body));
    expect(!!body.nextRefreshSecret).toBe(support);
    return response(body.nextRefreshSecret ?? 'legacy-successor');
  });
  expect(await renewPairedSession(fetcher)).toBe(true);
  expect(fetcher).toHaveBeenCalledTimes(2);
});

it('keeps an unrelated computer paired when this one is revoked', async () => {
  const otherKey = `${key}:other-computer`;
  localStorage.setItem(otherKey, JSON.stringify(initial));
  expect(await renewPairedSession(async () => Response.json({ reason: 'revoked_session' }, { status: 401 }))).toBe(false);
  expect(hasPairedSession()).toBe(false);
  expect(JSON.parse(localStorage.getItem(otherKey)!)).toEqual(initial);
});

it.each(['future_host_reason', 'proof_replayed', 'outside_time_window'])('keeps pairing after a nonterminal or unknown renewal verdict: %s', async (reason) => {
  expect(await renewPairedSession(async () => Response.json({ reason }, { status: 401 }))).toBe(false);
  expect(hasPairedSession()).toBe(true);
  expect(read().pendingNextSecret).toBeTruthy();
});
