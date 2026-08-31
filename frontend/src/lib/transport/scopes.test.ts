import { beforeEach, describe, expect, it, vi } from 'vitest';
import { clearPairedSession, redeemPairing, type PairingPayload } from './deviceSession';
import {
  SCOPES,
  __resetScopesForTest,
  grantedScopes,
  hasScope,
  isScope,
  refreshGrantedScopes,
  setPageGrantsFromBootstrap,
} from './scopes';

const PAYLOAD: PairingPayload = {
  v: 1,
  backendId: 'backend-1',
  endpoint: 'http://192.168.1.20:8123',
  token: 'link-token',
};

// A /auth/pair response, optionally publishing a grant set. `scopes`
// absent is what a backend older than the field answers.
function grantResponse(scopes?: unknown): Response {
  const body: Record<string, unknown> = {
    sessionId: 'sess-1',
    credential: 'cred-1',
    expiresAtMs: Date.now() + 15 * 60_000,
    refreshSecret: 'refresh-1',
  };
  if (scopes !== undefined) body.scopes = scopes;
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function pairWith(scopes?: unknown): Promise<void> {
  const fetcher = vi.fn(async () => grantResponse(scopes)) as unknown as typeof fetch;
  await redeemPairing(PAYLOAD, 'a phone', fetcher);
  refreshGrantedScopes();
}

describe('scopes', () => {
  beforeEach(() => {
    clearPairedSession();
    __resetScopesForTest();
  });

  it('answers nothing before the manifest resolves', () => {
    // A control that appears a frame late is better than one that appears
    // and is then taken away, so the pre-bootstrap answer holds nothing.
    expect(grantedScopes().source).toBe('unpaired');
    for (const scope of SCOPES) expect(hasScope(scope)).toBe(false);
  });

  it('gives the local page every grantable scope', () => {
    setPageGrantsFromBootstrap(false);

    const snapshot = grantedScopes();
    expect(snapshot.source).toBe('local-page');
    expect(snapshot.everyScope).toBe(true);
    expect(snapshot.onHost).toBe(true);
    // Explicitly all, not "no check ran": every name answers true,
    // including `host`, which presence rather than a grant authorizes.
    for (const scope of SCOPES) expect(hasScope(scope)).toBe(true);
  });

  it('gives an unpaired networked page nothing of its own', () => {
    setPageGrantsFromBootstrap(true);

    const snapshot = grantedScopes();
    expect(snapshot.source).toBe('unpaired');
    expect(snapshot.everyScope).toBe(false);
    expect(snapshot.onHost).toBe(false);
    for (const scope of SCOPES) expect(hasScope(scope)).toBe(false);
  });

  it('takes a paired session over the page, on loopback and off it', async () => {
    // Precedence is not an ordering preference: a browser holding a paired
    // session presents THAT session on the upgrade even from loopback, so
    // its grants are what the backend's gate will compare. Judging the page
    // instead would offer every surface to a session granted two.
    setPageGrantsFromBootstrap(false);
    await pairWith(['threads:read', 'files:read']);

    expect(grantedScopes().source).toBe('paired-session');
    expect(grantedScopes().everyScope).toBe(false);
    expect(hasScope('files:read')).toBe(true);
    expect(hasScope('threads:operate')).toBe(false);
    expect(hasScope('access:admin')).toBe(false);
  });

  it('answers `host` from presence, never from the grant set', async () => {
    // `host` is a method property rather than a grant — no session holds
    // it, and internal/transport/authorize.go authorizes it from "is the
    // caller on this machine". A paired session on the owner's own machine
    // therefore still opens an editor.
    setPageGrantsFromBootstrap(false);
    await pairWith(['threads:read']);
    expect(hasScope('host')).toBe(true);
    expect(grantedScopes().scopes.has('host' as never)).toBe(false);

    setPageGrantsFromBootstrap(true);
    expect(hasScope('host')).toBe(false);
  });

  it('reads an empty published grant set as "granted nothing"', async () => {
    setPageGrantsFromBootstrap(true);
    await pairWith([]);

    expect(grantedScopes().source).toBe('paired-session');
    for (const scope of SCOPES) expect(hasScope(scope)).toBe(false);
  });

  it('falls back to judging the page when the backend published no grants', async () => {
    // The distinction the wire's never-null rule exists for: a backend too
    // old to publish grants says nothing, which must not read as "granted
    // nothing" and blank the owner's own screen.
    setPageGrantsFromBootstrap(false);
    await pairWith(undefined);

    expect(grantedScopes().source).toBe('local-page');
    expect(hasScope('threads:operate')).toBe(true);
  });

  it('drops capability names this build has no gate for', async () => {
    setPageGrantsFromBootstrap(true);
    await pairWith(['threads:read', 'quantum:entangle']);

    expect([...grantedScopes().scopes]).toEqual(['threads:read']);
    expect(isScope('quantum:entangle')).toBe(false);
  });

  it('re-resolves when pairing completes, without being asked twice', async () => {
    setPageGrantsFromBootstrap(true);
    expect(hasScope('threads:autonomy')).toBe(false);

    // redeemPairing stores the credential; the re-resolve is what
    // wsClient.redialAfterPairing() drives when the flow finishes.
    const fetcher = vi.fn(async () =>
      grantResponse(['threads:read', 'threads:autonomy'])) as unknown as typeof fetch;
    await redeemPairing(PAYLOAD, 'a phone', fetcher);
    expect(hasScope('threads:autonomy')).toBe(false);

    refreshGrantedScopes();
    expect(hasScope('threads:autonomy')).toBe(true);
  });

  it('survives a reconnect the way the hello snapshot does', async () => {
    // Nothing clears the answer on a disconnect, and the manifest refetch
    // every reconnect performs re-publishes the same locality. A
    // capability that flapped to "nothing" for the length of an outage
    // would blank half the UI mid-reconnect.
    setPageGrantsFromBootstrap(false);
    await pairWith(['git:operate']);
    const before = grantedScopes();

    setPageGrantsFromBootstrap(false);
    const after = grantedScopes();

    expect(after.source).toBe('paired-session');
    expect(hasScope('git:operate')).toBe(true);
    // Identity, not just equality: an unchanged answer must not mint a new
    // snapshot, or every gated surface in the app re-evaluates per
    // reconnect for a value that did not move.
    expect(after).toBe(before);
  });

  it('drops back to the page answer when the session is cleared', async () => {
    setPageGrantsFromBootstrap(false);
    await pairWith(['threads:read']);
    expect(grantedScopes().source).toBe('paired-session');

    // A refused renewal clears the store; the page then dials unpaired.
    clearPairedSession();
    refreshGrantedScopes();

    expect(grantedScopes().source).toBe('local-page');
    expect(hasScope('threads:operate')).toBe(true);
  });

  it('refuses a stored grant list that is not a list of names', async () => {
    setPageGrantsFromBootstrap(true);
    await pairWith('threads:read');

    // Not an array: the response published nothing readable, so the page
    // answer stands rather than a string being walked character by
    // character into a grant set.
    expect(grantedScopes().source).toBe('unpaired');
  });
});
