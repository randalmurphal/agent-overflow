// LAN-bound authorization: `LocalOnlyMethods` refused for a non-loopback
// peer, and everything else still answered.
//
// WHY THIS FILE EXISTS AT ALL. Every other harness peer in this suite —
// the TS client, the SPA under Playwright, `bin/ao-harness` — reaches the
// backend on 127.0.0.1, so `Dispatcher.ResolveForOrigin`'s non-loopback
// branch never executes anywhere in the gate. The whole remote-access
// posture (docs/architecture/*, `internal/transport/internalmethods.go`)
// rests on that branch: terminal spawn, editor open, git mutation, session
// control, settings writes and credential reads are all "safe on a LAN
// bind" only because it fires. An untested refusal is a claim, not a
// control.
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
// and `remoteAddrIsLoopback` is false — the same input a real LAN browser
// produces. Two details make it work end to end:
//
//   - The bind has to go through `SetNetworkSettings`, not `--listen
//     0.0.0.0:0`. A bare LAN bind leaves the WS origin allow-list empty,
//     which is what `loopbackHostGuard` reads as "loopback mode", and it
//     404s the handshake on the non-loopback `Host` header before the
//     dispatcher is ever reached. `SetNetworkSettings` is the production
//     path: it rebinds AND installs the allow-list in one step.
//   - `SetNetworkSettings` is itself a LocalOnly method, so the toggle
//     rides the loopback connection. That is the point rather than an
//     inconvenience: a LAN peer must not be able to reconfigure the bind.
//
// HOW THE LAN PEER GETS ONTO THE SOCKET AT ALL. The `/ws` upgrade now
// refuses a non-loopback peer that names no durable session
// (`internal/transport/AGENTS.md`), so the launch token alone no longer
// opens one — this spec asserts that too. To reach the dispatcher it does
// what a paired browser does: exchange the launch token for the backend's
// session credential on `/bootstrap.json`, mint a single-use ticket at
// `/auth/ticket`, and dial `?ticket=`. `fetch` can set a header and the
// WHATWG `WebSocket` cannot, which is exactly why that ticket exists.
//
// The client here is deliberately hand-rolled rather than a second
// `HarnessApp`: this spec asserts on the RPC error envelope, and the TS
// client's `rpc()` flattens it into an `Error` message.

import * as os from 'node:os';
import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';

/** The wire's refusal for a method a peer may not call — see below. */
const REFUSAL = { code: 'method_not_found', message: 'method not registered' };

interface RpcFrame {
  type: string;
  id?: string;
  result?: unknown;
  error?: { code: string; message: string };
}

type RpcOutcome = { ok: true; result: unknown } | { ok: false; error: { code: string; message: string } };

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
    const id = `localonly-${this.nextId++}`;
    const outcome = new Promise<RpcOutcome>((resolve) => this.pending.set(id, resolve));
    this.ws.send(JSON.stringify({ type: 'rpc', id, method, params }));
    return outcome;
  }

  close(): void {
    this.ws.close();
  }
}

/**
 * The `?ticket=` query a LAN peer needs to open a socket: the backend's
 * own session credential, exchanged for a single-use WebSocket ticket.
 *
 * Both requests present the launch token, which is what a client that is
 * not a browser has always presented. What they buy is the session id the
 * upgrade now requires of an off-host peer — the credential is the local
 * page channel's, the same one the `--connect` stub forwards on its
 * carried hop (`internal/relaysession`).
 */
async function lanTicketQuery(host: string, port: number, token: string): Promise<string> {
  const base = `http://${host}:${port}`;
  const manifest = await fetch(`${base}/bootstrap.json?token=${encodeURIComponent(token)}`);
  expect(manifest.ok, 'the LAN bootstrap exchange must answer').toBe(true);
  // The cookie is port-qualified because cookies do not scope by port
  // (internal/transport/credential.go cookieNameForHost).
  const prefix = `ao_session_${port}=`;
  const planted = manifest.headers.getSetCookie().find((c) => c.startsWith(prefix));
  expect(planted, 'the bootstrap exchange must plant a session cookie').toBeTruthy();
  const credential = planted!.slice(prefix.length).split(';')[0];

  const minted = await fetch(`${base}/auth/ticket`, {
    method: 'POST',
    headers: { 'X-AO-Session': credential },
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

test.describe('LAN-bound authorization', () => {
  // Not green-washed: a host with no non-loopback interface (a locked-down
  // CI container) genuinely cannot produce the peer this spec is about, and
  // saying so is the honest outcome. A skip is visible in the report; a
  // vacuous pass is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no LAN peer can be produced',
  );

  let harness: HarnessApp;

  test.beforeAll(async () => {
    harness = await launchHarness();
  });

  test.afterAll(async () => {
    await harness?.close();
  });

  test('refuses LocalOnly methods for a non-loopback peer and answers the rest', async () => {
    // The production LAN-bind path: rebinds to 0.0.0.0 on the same port and
    // installs the WS origin allow-list. Runs on the loopback connection
    // because it is itself LocalOnly.
    const settings = await harness.rpc<{ bindAll: boolean; url: string }>('SetNetworkSettings', {
      bindAll: true,
    });
    expect(settings.bindAll).toBe(true);

    // The launch token alone no longer opens a socket off-host: it names
    // the backend launch and not the client, so nothing could revoke the
    // connection it would open.
    let sessionlessRefused = false;
    try {
      const doomed = await WireClient.connect(
        lanIP!,
        harness.bootstrap.port,
        `token=${encodeURIComponent(harness.bootstrap.token)}`,
      );
      doomed.close();
    } catch {
      sessionlessRefused = true;
    }
    expect(
      sessionlessRefused,
      'a LAN peer naming no session must be refused the upgrade',
    ).toBe(true);

    const ticketed = await lanTicketQuery(lanIP!, harness.bootstrap.port, harness.bootstrap.token);
    const remote = await WireClient.connect(lanIP!, harness.bootstrap.port, ticketed);
    try {
      // (a) Refused. Two entries from two different mechanisms, because
      // they can regress independently: the whole `Harness` receiver is
      // registered `LocalOnly: true`, while `GetGitStatus` is refused by
      // name out of `LocalOnlyMethods`.
      for (const [method, ...params] of [
        ['HarnessInfo'],
        ['GetGitStatus', 'unused-thread-id'],
        ['OpenInEditor', '/etc/passwd', 0, 0, '/tmp', ''],
      ] as Array<[string, ...unknown[]]>) {
        const outcome = await remote.call(method, ...params);
        expect(outcome, `${method} must be refused for a LAN peer`).toMatchObject({
          ok: false,
          error: REFUSAL,
        });
      }

      // The refusal is deliberately INDISTINGUISHABLE from a method that
      // was never registered, so a LAN scanner cannot enumerate which
      // methods are privileged versus simply absent. If this ever diverges
      // — a distinct code, a different message — the privileged surface has
      // become fingerprintable.
      const absent = await remote.call('NoSuchMethodExistsAnywhere');
      expect(absent).toMatchObject({ ok: false, error: REFUSAL });

      // (b) Not LocalOnly: still answered, so the refusals above are about
      // the classification and not about a LAN peer being broken.
      const version = await remote.call('Version');
      expect(version.ok, 'Version must answer a LAN peer').toBe(true);
      expect(typeof (version as { result: unknown }).result).toBe('string');
      const projects = await remote.call('ListProjects');
      expect(projects.ok, 'ListProjects must answer a LAN peer').toBe(true);

      // The same methods on the same server, from loopback: proof that the
      // refusals are the peer's locality rather than a method that stopped
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
