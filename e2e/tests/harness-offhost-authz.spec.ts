// Off-host authorization: what a paired device reaches over a LAN bind,
// and what stays refused whatever it holds.
//
// WHY THIS FILE EXISTS AT ALL. Every other harness peer in this suite —
// the TS client, the SPA under Playwright, `bin/ao-harness` — reaches the
// backend on 127.0.0.1, so nothing else in the gate ever produces a
// non-loopback peer. The whole remote-access posture rests on what happens
// when one appears: the `/ws` admission rule, the binding class compared
// at presentation, the per-call scope gate, and the one receiver still
// judged by locality. An untested refusal is a claim, not a control.
//
// WHY IT OWNS ITS BACKEND. It flips the LAN-bind preference, which
// PERSISTS to the settings file and REBINDS the transport listener. The
// worker fixture's instance is shared by every other spec in the worker
// and `harness.reset()` does not undo either, so borrowing it would leave
// a LAN-bound backend behind for whatever ran next.
//
// HOW A NON-LOOPBACK PEER IS PRODUCED without a second machine: bind the
// server to 0.0.0.0 and dial one of this host's own non-loopback interface
// addresses. Linux answers such a connection with a source address equal
// to the destination, so the kernel reports `RemoteAddr: <lan-ip>:<port>`
// and `loopback.PeerAddress` is false — the same input a real LAN browser
// produces. Two details make it work end to end:
//
//   - The bind has to go through `SetNetworkSettings`, not `--listen
//     0.0.0.0:0`. A bare LAN bind leaves the WS origin allow-list empty,
//     which is what `loopbackHostGuard` reads as "loopback mode", and it
//     404s the handshake on the non-loopback `Host` header before the
//     dispatcher is ever reached. `SetNetworkSettings` is the production
//     path: it rebinds AND installs the allow-list in one step.
//   - `SetNetworkSettings` carries `//ao:stepup`, so it rides the loopback
//     connection. That is the point rather than an inconvenience: a remote
//     peer must not be able to reconfigure the bind.
//
// HOW THE OFF-HOST PEER GETS ONTO THE SOCKET AT ALL. It pairs, exactly as
// a phone would. The launch token no longer opens an off-host upgrade
// (wave 6d1), and the backend's own local-channel credential is now
// `loopback-only` at PRESENTATION too (wave 6d2) — the off-host bootstrap
// exchange never plants it, and presenting it from off-host would resolve
// no session anyway. Both of those are asserted here. What is left is the
// real flow: mint a link on the host, load the page with the one-time
// ticket the link carries, redeem it with a device key, confirm it on the
// host, then mint a single-use `/ws` ticket. `fetch` can set a header and
// the WHATWG `WebSocket` cannot, which is exactly why that ticket exists.
//
// The client here is deliberately hand-rolled rather than a second
// `HarnessApp`: this spec asserts on the RPC error envelope, and the TS
// client's `rpc()` flattens it into an `Error` message.

import * as os from 'node:os';
import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';

/** The refusal for a method a peer may not even see. */
const UNENUMERABLE = { code: 'method_not_found', message: 'method not registered' };

/** The device key this spec's pretend phone holds. */
const DEVICE_KEY = 'e2e-offhost-device';

interface RpcFrame {
  type: string;
  id?: string;
  result?: unknown;
  error?: { code: string; message: string; scope?: string };
}

type RpcOutcome =
  | { ok: true; result: unknown }
  | { ok: false; error: { code: string; message: string; scope?: string } };

/**
 * One WebSocket speaking the transport wire, reporting the raw outcome so a
 * refusal can be asserted as a frame rather than as a thrown message.
 */
class WireClient {
  private ws: WebSocket;
  private pending = new Map<string, (outcome: RpcOutcome) => void>();
  private nextId = 1;

  private constructor(ws: WebSocket) {
    this.ws = ws;
    ws.addEventListener('message', (msg) => {
      const frame = JSON.parse(String(msg.data)) as RpcFrame;
      if (frame.type !== 'rpc') return;
      const settle = this.pending.get(frame.id ?? '');
      if (!settle) return;
      this.pending.delete(frame.id ?? '');
      settle(frame.error ? { ok: false, error: frame.error } : { ok: true, result: frame.result });
    });
  }

  static async connect(host: string, port: number, query: string): Promise<WireClient> {
    const ws = new WebSocket(`ws://${host}:${port}/ws?${query}`);
    await new Promise<void>((resolve, reject) => {
      ws.addEventListener('open', () => resolve(), { once: true });
      // The upgrade answers 404 rather than 403 for every refusal it has
      // (bad token, guarded Host, no session named by an off-host peer),
      // so a failure here is opaque by design.
      ws.addEventListener(
        'error',
        () => reject(new Error(`WS handshake to ${host}:${port} was refused`)),
        { once: true },
      );
    });
    return new WireClient(ws);
  }

  call(method: string, ...params: unknown[]): Promise<RpcOutcome> {
    const id = `offhost-${this.nextId++}`;
    const outcome = new Promise<RpcOutcome>((resolve) => this.pending.set(id, resolve));
    this.ws.send(JSON.stringify({ type: 'rpc', id, method, params }));
    return outcome;
  }

  close(): void {
    this.ws.close();
  }
}

interface TokenGrant {
  sessionId: string;
  credential: string;
  pairingId?: string;
  scopes?: string[];
}

/**
 * What a minted pairing link carries. The FRAGMENT holds the payload,
 * which is never sent to a server; the QUERY holds a one-time page ticket,
 * because the device the link is for holds no credential and could not
 * load the page otherwise.
 */
interface PairingLink {
  pageTicket: string;
  token: string;
}

function mintLink(invite: { url: string }): PairingLink {
  const [before, fragment] = invite.url.split('#pair=');
  expect(fragment, 'the pairing URL must carry its payload in the fragment').toBeTruthy();
  const pageTicket = new URL(before).searchParams.get('t');
  expect(pageTicket, 'the pairing URL must carry a one-time page ticket').toBeTruthy();
  const payload = JSON.parse(Buffer.from(fragment!, 'base64url').toString('utf8')) as {
    token: string;
  };
  return { pageTicket: pageTicket!, token: payload.token };
}

/**
 * The credential a paired device holds, produced by walking the rest of
 * the real exchange: the device redeems the link with a key it generated
 * first, and the host confirms the verification number. It is enrolled
 * `device-bound` — the binding class that reaches a non-loopback listener.
 */
async function pairDevice(
  harness: HarnessApp,
  host: string,
  port: number,
  link: PairingLink,
): Promise<TokenGrant> {
  const redeemed = await fetch(`http://${host}:${port}/auth/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      token: link.token,
      keyThumbprint: DEVICE_KEY,
      label: 'e2e off-host peer',
      platform: 'playwright',
    }),
  });
  expect(redeemed.ok, '/auth/pair must answer a redemption naming a live link').toBe(true);
  const grant = (await redeemed.json()) as TokenGrant;

  // The credential exists and admits nothing until the owner, at the
  // machine, matches the number the device is showing.
  await harness.rpc('ConfirmDevicePairing', grant.pairingId);
  return grant;
}

/** A single-use `/ws` ticket for a session the caller already holds. */
async function wsTicket(host: string, port: number, grant: TokenGrant): Promise<string> {
  const minted = await fetch(`http://${host}:${port}/auth/ticket`, {
    method: 'POST',
    headers: { 'X-AO-Session': grant.credential, 'X-AO-Device-Key': DEVICE_KEY },
  });
  expect(minted.ok, '/auth/ticket must answer a request naming a live session').toBe(true);
  const { ticket } = (await minted.json()) as { ticket: string };
  return `ticket=${encodeURIComponent(ticket)}`;
}

/**
 * A non-loopback IPv4 address on this host, or null. `internal` covers the
 * loopback interface as a whole, which matters on WSL: `lo` carries
 * 10.255.255.254 alongside 127.0.0.1, and that address is neither in
 * 127/8 nor reachable as a LAN peer.
 */
function nonLoopbackIPv4(): string | null {
  for (const addrs of Object.values(os.networkInterfaces())) {
    for (const addr of addrs ?? []) {
      if (addr.family === 'IPv4' && !addr.internal) return addr.address;
    }
  }
  return null;
}

const lanIP = nonLoopbackIPv4();

test.describe('off-host authorization', () => {
  // Not green-washed: a host with no non-loopback interface (a locked-down
  // CI container) genuinely cannot produce the peer this spec is about, and
  // saying so is the honest outcome. A skip is visible in the report; a
  // vacuous pass is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;

  test.beforeAll(async () => {
    harness = await launchHarness();
  });

  test.afterAll(async () => {
    await harness?.close();
  });

  test('a paired device reads; host tooling and host-scoped calls stay refused', async () => {
    // The production LAN-bind path: rebinds to 0.0.0.0 on the same port and
    // installs the WS origin allow-list.
    const settings = await harness.rpc<{ bindAll: boolean; url: string }>('SetNetworkSettings', {
      bindAll: true,
    });
    expect(settings.bindAll).toBe(true);

    const port = harness.bootstrap.port;

    // The launch token alone opens no socket off-host: it names the backend
    // launch and not the client, so nothing could revoke the connection it
    // would open.
    let sessionlessRefused = false;
    try {
      const doomed = await WireClient.connect(
        lanIP!,
        port,
        `token=${encodeURIComponent(harness.bootstrap.token)}`,
      );
      doomed.close();
    } catch {
      sessionlessRefused = true;
    }
    expect(sessionlessRefused, 'an off-host peer naming no session must be refused the upgrade').toBe(
      true,
    );

    // The pairing page loading on the phone: it spends the link's one-time
    // page ticket, which is the exchange that plants a page cookie. It must
    // plant that cookie — the page has to load to show the pairing state —
    // and must NOT plant the backend's own `loopback-only` session, whose
    // class does not reach this peer.
    const link = mintLink(await harness.rpc<{ url: string }>('MintDevicePairing', 'phone'));
    const manifest = await fetch(
      `http://${lanIP}:${port}/bootstrap.json?t=${encodeURIComponent(link.pageTicket)}`,
    );
    expect(manifest.ok, 'the off-host bootstrap exchange must still answer').toBe(true);
    const planted = manifest.headers.getSetCookie();
    expect(
      planted.some((c) => c.startsWith(`ao_page_${port}=`)),
      'the page cookie must still be planted: the page has to load to show the pairing state',
    ).toBe(true);
    expect(
      planted.some((c) => c.startsWith(`ao_session_${port}=`)),
      'the loopback-only local channel must not be planted for an off-host peer',
    ).toBe(false);

    const grant = await pairDevice(harness, lanIP!, port, link);
    const remote = await WireClient.connect(lanIP!, port, await wsTicket(lanIP!, port, grant));
    try {
      // (a) Unenumerable. The `Harness` receiver is registered
      // `RegisterOptions{LocalOnly: true}` — host tooling, no `//ao:scope`
      // annotations at all — so no grant can reach it and the refusal is
      // indistinguishable from a method that was never registered.
      const tooling = await remote.call('HarnessInfo');
      expect(tooling, 'host tooling must be unenumerable off-host').toMatchObject({
        ok: false,
        error: UNENUMERABLE,
      });
      const absent = await remote.call('NoSuchMethodExistsAnywhere');
      expect(absent).toMatchObject({ ok: false, error: UNENUMERABLE });

      // (b) Named refusal. `host` marks a call with no remote form, and no
      // session may be granted it — this device holds every OTHER scope and
      // is still refused, with the missing scope named so a client can say
      // WHY a control is disabled.
      const hostScoped = await remote.call('OpenInEditor', '/etc/passwd', 0, 0, '/tmp', '');
      expect(hostScoped, 'a host-scoped call must be refused off-host').toMatchObject({
        ok: false,
        error: { code: 'scope_required', scope: 'host' },
      });

      // (c) Answered. Reads the device's grants cover, including the
      // workspace-content surface this wave unlocked:
      // HighlightPatchWithContext was refused off-host by NAME until the
      // origin partition was deleted, and `files:read` is what decides it
      // now. Its content priming is best-effort, so a caller with no thread
      // gets the unprimed spans rather than an error — which is exactly the
      // answer the grant entitles it to.
      for (const [method, ...params] of [
        ['Version'],
        ['ListProjects'],
        [
          'HighlightPatchWithContext',
          '',
          { scope: 'workspace', path: 'main.go', patch: '@@ -1 +1 @@\n+package main\n' },
        ],
      ] as Array<[string, ...unknown[]]>) {
        const outcome = await remote.call(method, ...params);
        expect(outcome.ok, `${method} must answer a paired device`).toBe(true);
      }

      // The same host tooling on the same server, from loopback: proof that
      // (a) is the peer's locality rather than a method that stopped
      // working when the listener moved.
      const info = await harness.rpc<{ pid: number }>('HarnessInfo');
      expect(info.pid).toBe(harness.bootstrap.pid);
    } finally {
      remote.close();
      // Leave the instance as we found it even though nothing else shares
      // it — the settings file outlives the listener, and a future reader
      // of this data dir should not find a LAN bind nobody asked for.
      await harness.rpc('SetNetworkSettings', { bindAll: false });
    }
  });
});
