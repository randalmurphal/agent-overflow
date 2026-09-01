// The pieces every off-host spec needs: how a non-loopback peer is
// produced on one machine, how a device becomes one, and what a person
// at that device would have SEEN.
//
// Three spec families share them — `harness-offhost-authz.spec.ts` (what
// a paired device may reach), `harness-remote-device-lifecycle.spec.ts`
// (pair, revoke, forget, re-pair, and what the owner's screen says about
// it) and `harness-passkey-lifecycle.spec.ts` (register, sign in with no
// code, step up remotely) — so they live here rather than in whichever
// file was written first. Everything below is the REAL exchange; nothing
// here reaches past a route a phone would use.
//
// HOW A NON-LOOPBACK PEER IS PRODUCED without a second machine: bind the
// server to 0.0.0.0 and dial one of this host's own non-loopback
// interface addresses. Linux answers such a connection with a source
// address equal to the destination, so the kernel reports
// `RemoteAddr: <lan-ip>:<port>` and `loopback.PeerAddress` is false —
// the same input a real LAN browser produces. Two details make it work
// end to end:
//
//   - The bind has to go through `SetNetworkSettings`, not `--listen
//     0.0.0.0:0`. A bare LAN bind leaves the WS origin allow-list empty,
//     which is what `loopbackHostGuard` reads as "loopback mode", and it
//     404s the handshake on the non-loopback `Host` header before the
//     dispatcher is ever reached. `SetNetworkSettings` is the production
//     path: it rebinds AND installs the allow-list in one step.
//   - `SetNetworkSettings` carries `//ao:stepup`, so it rides the
//     loopback connection. That is the point rather than an
//     inconvenience: a remote peer must not be able to reconfigure the
//     bind.

import { readFileSync } from 'node:fs';
import * as os from 'node:os';
import { fileURLToPath } from 'node:url';
import { expect, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';

/** The refusal for a method a peer may not even see. */
export const UNENUMERABLE = { code: 'method_not_found', message: 'method not registered' };

interface RpcFrame {
  type: string;
  id?: string;
  result?: unknown;
  error?: { code: string; message: string; scope?: string };
}

export type RpcOutcome =
  | { ok: true; result: unknown }
  | { ok: false; error: { code: string; message: string; scope?: string } };

/**
 * Assert an RPC was ANSWERED, and hand back the payload already narrowed.
 *
 * `expect(outcome.ok).toBe(true)` reads well and narrows nothing, so a spec
 * that then wants the payload is reaching into a union half the compiler
 * still believes could be the refusal — which is a typecheck failure at
 * the one moment a wire-level assertion becomes interesting. Going through
 * here keeps the message and returns the branch, and a refusal reports its
 * own reason instead of `false !== true`.
 */
export function answered(outcome: RpcOutcome, why: string): unknown {
  expect(outcome.ok ? '' : `${outcome.error.code}: ${outcome.error.message}`, why).toBe('');
  if (!outcome.ok) throw new Error(outcome.error.message);
  return outcome.result;
}

/**
 * One WebSocket speaking the transport wire, reporting the raw outcome so a
 * refusal can be asserted as a frame rather than as a thrown message.
 *
 * Deliberately hand-rolled rather than a second `HarnessApp`: these specs
 * assert on the RPC error envelope, and the TS client's `rpc()` flattens
 * it into an `Error` message.
 */
export class WireClient {
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

export interface TokenGrant {
  sessionId: string;
  credential: string;
  pairingId?: string;
  scopes?: string[];
}

/** What `MintDevicePairing` answers. */
export interface PairingInvite {
  linkId: string;
  url: string;
  expiresAtMs: number;
}

/**
 * What a minted pairing link carries. The FRAGMENT holds the payload,
 * which is never sent to a server; the QUERY holds a one-time page ticket,
 * because the device the link is for holds no credential and could not
 * load the page otherwise.
 */
export interface PairingLink {
  pageTicket: string;
  token: string;
}

export function mintLink(invite: { url: string }): PairingLink {
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
 *
 * The `keyThumbprint` is the device's IDENTITY: redeeming a second link
 * with the same one adopts the same device row, and a fresh one enrolls a
 * new device. That is the same fact a browser lives by, where the string
 * is 32 CSPRNG bytes minted once per origin (`deviceSession.ts`).
 */
export async function pairDeviceOverWire(
  harness: HarnessApp,
  host: string,
  port: number,
  link: PairingLink,
  deviceKey: string,
  label = 'e2e off-host peer',
): Promise<TokenGrant> {
  const redeemed = await fetch(`http://${host}:${port}/auth/pair`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      token: link.token,
      keyThumbprint: deviceKey,
      label,
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
export async function wsTicket(
  host: string,
  port: number,
  grant: TokenGrant,
  deviceKey: string,
): Promise<string> {
  const minted = await fetch(`http://${host}:${port}/auth/ticket`, {
    method: 'POST',
    headers: { 'X-AO-Session': grant.credential, 'X-AO-Device-Key': deviceKey },
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
export function nonLoopbackIPv4(): string | null {
  for (const addrs of Object.values(os.networkInterfaces())) {
    for (const addr of addrs ?? []) {
      if (addr.family === 'IPv4' && !addr.internal) return addr.address;
    }
  }
  return null;
}

// ---------------------------------------------------------------------
// Naming what went over the wire.
// ---------------------------------------------------------------------

/**
 * Method id → method name, read out of the generated bindings.
 *
 * A generated binding calls `Call.ByID(<hash>, …)`, so the request frame
 * carries `methodId` and NOT `method` (`internal/transport/frame.go`):
 * only `callByName` sends a name. A spec asserting on wire traffic
 * therefore has a number where the useful failure message needs a name,
 * and this is the one place that translation lives.
 *
 * Read from the generated file rather than from a hand-kept table: the
 * hash is Wails' reflection hash, it moves when a signature does, and a
 * table would go stale silently — which for a test that asserts an
 * ABSENCE means it stops naming the thing it caught.
 */
let methodNames: Map<number, string> | null = null;

export function methodNameById(id: number): string {
  if (methodNames === null) {
    const source = readFileSync(
      fileURLToPath(new URL('../../frontend/bindings/agent-overflow/app.ts', import.meta.url)),
      'utf8',
    );
    methodNames = new Map();
    const pattern = /export function (\w+)\([^)]*\)[^{]*\{\s*return \$Call\.ByID\((\d+)/g;
    for (const match of source.matchAll(pattern)) {
      methodNames.set(Number(match[2]), match[1]);
    }
    expect(
      methodNames.size,
      'the generated bindings must yield a method table, or every wire assertion reports "unknown"',
    ).toBeGreaterThan(50);
  }
  return methodNames.get(id) ?? `methodId:${id}`;
}

// ---------------------------------------------------------------------
// Page instrumentation: what a person would have SEEN.
// ---------------------------------------------------------------------

export interface Surfaced {
  /** Every error toast that was ever rendered, in order, whether or not it is still up. */
  errorToasts: string[];
  /** Every console error and uncaught exception. */
  consoleErrors: string[];
  /** Every `data-status` the transport banner ever published. */
  transportStatuses: string[];
  /** Every RPC refusal frame the page received, as `<method> <code>:<scope>`. */
  refusals: string[];
  /**
   * The method name of every RPC REPLY frame, refused or not. The
   * precondition for asserting `refusals` is empty: a capture that saw no
   * traffic at all would report a clean wire for a page that never
   * connected. It is also how a spec asserts that a ceremony DID run.
   */
  rpcReplies: string[];
}

/**
 * Record what the page surfaced, in NODE rather than on `window`, so a
 * navigation cannot lose what an earlier document showed — the same
 * reason `fixtures.ts` collects CSP violations this way.
 *
 * Toasts and banner statuses are captured through a MutationObserver
 * rather than by polling a locator, because both are transient: a toast
 * auto-dismisses and a status moves on, so a locator assertion run after
 * the fact would report an absence that was never true.
 *
 * Call it before the first navigation: the init script has to be in place
 * for the document that will do the thing being observed.
 */
export async function instrument(page: Page): Promise<Surfaced> {
  const surfaced: Surfaced = {
    errorToasts: [],
    consoleErrors: [],
    transportStatuses: [],
    refusals: [],
    rpcReplies: [],
  };

  await page.exposeFunction('__aoRecordErrorToast', (text: string) => {
    surfaced.errorToasts.push(text);
  });
  await page.exposeFunction('__aoRecordTransportStatus', (status: string) => {
    if (surfaced.transportStatuses.at(-1) !== status) surfaced.transportStatuses.push(status);
  });
  await page.addInitScript(() => {
    const win = window as unknown as {
      __aoRecordErrorToast?: (text: string) => void;
      __aoRecordTransportStatus?: (status: string) => void;
    };
    const noteToast = (el: Element) => win.__aoRecordErrorToast?.(el.textContent?.trim() ?? '');
    const noteBanner = (el: Element) =>
      win.__aoRecordTransportStatus?.(el.getAttribute('data-status') ?? '');
    const scan = (node: Node) => {
      if (!(node instanceof Element)) return;
      if (node.matches('[data-toast-type="error"]')) noteToast(node);
      for (const el of node.querySelectorAll('[data-toast-type="error"]')) noteToast(el);
      if (node.matches('[data-testid="transport-status-banner"]')) noteBanner(node);
      for (const el of node.querySelectorAll('[data-testid="transport-status-banner"]'))
        noteBanner(el);
    };
    // `document` is a valid observe target and exists this early, which
    // `document.body` does not.
    new MutationObserver((records) => {
      for (const record of records) {
        if (record.type === 'attributes' && record.target instanceof Element) {
          noteBanner(record.target);
          continue;
        }
        for (const added of record.addedNodes) scan(added);
      }
    }).observe(document, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['data-status'],
    });
  });

  page.on('console', (message) => {
    if (message.type() === 'error') surfaced.consoleErrors.push(message.text());
  });
  page.on('pageerror', (error) => surfaced.consoleErrors.push(`uncaught: ${error.message}`));

  // The wire itself. A refusal is a field on the RPC response frame
  // (`internal/transport/frame.go`), and the prose is redacted for a
  // non-loopback caller, so the CODE plus the named scope is the whole
  // answer that survives. The METHOD is not on the reply at all — it is on
  // the request that shares its id — so the sent side is correlated here
  // too: "something was refused" is a failure nobody can act on, and the
  // name of the call is the whole lead.
  page.on('websocket', (ws) => {
    const methodById = new Map<string, string>();
    ws.on('framesent', (frame) => {
      let parsed: { type?: string; id?: string; method?: string; methodId?: number };
      try {
        parsed = JSON.parse(String(frame.payload)) as typeof parsed;
      } catch {
        return;
      }
      if (parsed.type !== 'rpc' || !parsed.id) return;
      // A generated binding sends `methodId` and nothing else; only
      // `callByName` sends a name.
      const name =
        parsed.method ?? (parsed.methodId ? methodNameById(parsed.methodId) : undefined);
      if (name) methodById.set(parsed.id, name);
    });
    ws.on('framereceived', (frame) => {
      let parsed: { type?: string; id?: string; error?: { code?: string; scope?: string } };
      try {
        parsed = JSON.parse(String(frame.payload)) as typeof parsed;
      } catch {
        return;
      }
      if (parsed.type !== 'rpc') return;
      const method = methodById.get(parsed.id ?? '') ?? 'unknown';
      methodById.delete(parsed.id ?? '');
      surfaced.rpcReplies.push(method);
      if (!parsed.error?.code) return;
      surfaced.refusals.push(`${method} ${parsed.error.code}:${parsed.error.scope ?? ''}`);
    });
  });

  return surfaced;
}
