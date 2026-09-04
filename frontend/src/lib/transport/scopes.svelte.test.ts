import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { clearPairedSession, redeemPairing, type PairingPayload } from './deviceSession';
import {
  SCOPES,
  __resetScopesForTest,
  grantedScopes,
  hasScope,
  isScope,
  isViewOnly,
  isViewOnlyGrantSet,
  pageGrantsResolved,
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

  it('answers nothing before the manifest resolves, and only to a reactive reader', () => {
    // A control that appears a frame late is better than one that appears
    // and is then taken away, so the pre-bootstrap answer holds nothing.
    // But only a reader that will be re-run when the answer lands may ask
    // this early: a plain read at mount keeps the placeholder forever (the
    // idle memory trim did exactly that, 2026-09-03), so the suite refuses
    // it by name.
    expect(() => grantedScopes()).toThrow(/before the bootstrap manifest resolved/);
    expect(() => hasScope('host')).toThrow(/before the bootstrap manifest resolved/);
    expect(() => isViewOnly()).toThrow(/before the bootstrap manifest resolved/);

    let source = '';
    let held: boolean[] = [];
    const dispose = $effect.root(() => {
      $effect(() => {
        source = grantedScopes().source;
        held = SCOPES.map((scope) => hasScope(scope));
      });
    });
    flushSync();
    expect(source).toBe('unpaired');
    expect(held.some(Boolean)).toBe(false);

    // The same reader sees the manifest land without being asked again.
    setPageGrantsFromBootstrap(false);
    flushSync();
    expect(source).toBe('local-page');
    expect(held.every(Boolean)).toBe(true);
    dispose();
  });

  it('settles pageGrantsResolved when the manifest lands, and at once thereafter', async () => {
    let settled = false;
    const waiting = pageGrantsResolved().then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);

    setPageGrantsFromBootstrap(true);
    await waiting;
    expect(settled).toBe(true);
    // A later caller does not wait on a manifest that already spoke.
    let again = false;
    void pageGrantsResolved().then(() => {
      again = true;
    });
    await Promise.resolve();
    expect(again).toBe(true);
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
    // caller on this machine". The local page is that caller.
    setPageGrantsFromBootstrap(false);
    expect(hasScope('host')).toBe(true);
    expect(grantedScopes().scopes.has('host' as never)).toBe(false);

    setPageGrantsFromBootstrap(true);
    expect(hasScope('host')).toBe(false);
  });

  it('denies `host` to a paired session even on loopback', async () => {
    // A paired device that reaches its backend through a loopback forward
    // (adb reverse, an SSH tunnel) is still a device sitting at another
    // machine: the editor `host` would open is not in front of it. The
    // tier table gives owner devices everything except `host`, so the UI
    // answers the table rather than the peer address — the phone over
    // `adb reverse` showed "Open in editor" until it did (2026-09-04).
    setPageGrantsFromBootstrap(false);
    await pairWith(['threads:read']);
    expect(grantedScopes().source).toBe('paired-session');
    expect(hasScope('host')).toBe(false);
    expect(hasScope('threads:read')).toBe(true);
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

  describe('view-only mode', () => {
    it('is the observe grant set and nothing else', async () => {
      setPageGrantsFromBootstrap(true);
      await pairWith(['threads:read', 'files:read', 'settings:read']);

      expect(isViewOnly()).toBe(true);
    });

    it('is false for a device holding any execute-tier grant', async () => {
      setPageGrantsFromBootstrap(true);
      await pairWith(['threads:read', 'files:read', 'settings:read', 'git:operate']);

      expect(isViewOnly()).toBe(false);
    });

    it('is false on the owner\'s own screen', () => {
      setPageGrantsFromBootstrap(false);

      expect(isViewOnly()).toBe(false);
    });

    it('is false before the answer resolves and for an unpaired page', () => {
      // Both hold an EMPTY set, which says "nothing was granted to me" —
      // not "I was granted a read-only slice". Answering true would flash
      // the indicator on every boot and would label the pairing prompt as
      // a working read-only app. The pre-resolution read is a reactive one,
      // as the marker's is; a plain read that early is refused.
      let viewOnly: boolean | null = null;
      const dispose = $effect.root(() => {
        $effect(() => {
          viewOnly = isViewOnly();
        });
      });
      flushSync();
      expect(viewOnly).toBe(false);
      dispose();

      setPageGrantsFromBootstrap(true);
      expect(isViewOnly()).toBe(false);
    });

    it('flips the moment a narrower grant set is published', async () => {
      // The mid-flight downgrade: a full-access device re-paired onto a
      // view-only link re-reads its grants through the same
      // refreshGrantedScopes() the redial calls, with no reload.
      setPageGrantsFromBootstrap(true);
      await pairWith(['threads:read', 'git:operate']);
      expect(isViewOnly()).toBe(false);

      clearPairedSession();
      await pairWith(['threads:read', 'files:read', 'settings:read']);
      expect(isViewOnly()).toBe(true);
      expect(hasScope('git:operate')).toBe(false);
    });

    // The same question asked about a set that came from somewhere else:
    // the settings pane labels each PAIRED device from the grants its
    // session carries. One definition, so a device cannot read "View
    // only" on one screen and "Full access" on another.
    describe('isViewOnlyGrantSet', () => {
      it('answers for a grant set this page does not hold', () => {
        setPageGrantsFromBootstrap(false); // the owner's own screen
        expect(isViewOnlyGrantSet(['threads:read', 'files:read', 'settings:read'])).toBe(true);
        expect(isViewOnlyGrantSet(['threads:read', 'approvals:respond'])).toBe(false);
      });

      it('is false for an empty set, matching isViewOnly', () => {
        expect(isViewOnlyGrantSet([])).toBe(false);
      });

      it('ignores names this build does not know', () => {
        // A bundle older than the backend cannot reason about a
        // capability it has no gates for, and guessing execute-tier
        // would label a full-access device read-only.
        expect(isViewOnlyGrantSet(['threads:read', 'threads:teleport'])).toBe(true);
      });
    });
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
