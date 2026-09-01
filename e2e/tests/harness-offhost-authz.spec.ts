// Off-host authorization: what a paired device reaches over a LAN bind,
// what stays refused whatever it holds, and what a call it IS allowed to
// make leaves out of its answer.
//
// WHY THIS FILE EXISTS AT ALL. Every other harness peer in this suite —
// the TS client, the SPA under Playwright, `bin/ao-harness` — reaches the
// backend on 127.0.0.1, so nothing else in the gate ever produces a
// non-loopback peer. The whole remote-access posture rests on what happens
// when one appears: the `/ws` admission rule, the binding class compared
// at presentation, the per-call scope gate, and the one receiver still
// judged by locality. An untested refusal is a claim, not a control.
//
// The LIFECYCLE around that posture — pairing through the real screen,
// revoking, forgetting, re-pairing, and what the owner's device list says
// about each — is `harness-remote-device-lifecycle.spec.ts`. This file
// owns the refusal matrix only, and the two share
// `offhost-helpers.ts`.
//
// WHY IT OWNS ITS BACKEND. It flips the LAN-bind preference, which
// PERSISTS to the settings file and REBINDS the transport listener. The
// worker fixture's instance is shared by every other spec in the worker
// and `harness.reset()` does not undo either, so borrowing it would leave
// a LAN-bound backend behind for whatever ran next.
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

import { expect, test } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  UNENUMERABLE,
  WireClient,
  answered,
  mintLink,
  nonLoopbackIPv4,
  pairDeviceOverWire,
  wsTicket,
} from './offhost-helpers.js';

/** The device key this spec's pretend phone holds. */
const DEVICE_KEY = 'e2e-offhost-device';

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

  test('a paired device reads; host tooling and host-scoped calls stay refused; credentials stay home', async () => {
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
    const link = mintLink(await harness.rpc<{ url: string }>('MintDevicePairing', 'phone', 'full'));
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

    const grant = await pairDeviceOverWire(harness, lanIP!, port, link, DEVICE_KEY);
    const remote = await WireClient.connect(
      lanIP!,
      port,
      await wsTicket(lanIP!, port, grant, DEVICE_KEY),
    );
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

      // (d) Answered, and REDACTED. Managing how a backend is exposed is
      // what an `access:admin` device is for, so Settings → Network reads
      // for this peer — but the credential half of that record does not
      // travel: this launch's token would let the holder attach as the
      // backend's own local channel, and both share URLs carry one-time
      // page tickets out of a bounded book. Read off the wire rather than
      // off the screen, because the screen is not what would leak.
      const record = answered(
        await remote.call('GetNetworkSettings'),
        'an access:admin device must read the network settings',
      ) as {
        bindAll: boolean;
        token: string;
        url: string;
        tailnet: { url: string };
        insecure: boolean;
      };
      expect(record.token, "this launch's token must not reach an off-host device").toBe('');
      expect(record.url, 'the ticketed share URL must not reach an off-host device').toBe('');
      expect(record.tailnet.url, 'the ticketed tailnet URL must not reach an off-host device').toBe(
        '',
      );
      expect(record.insecure, 'a record with no URL describes no URL').toBe(false);
      // The settings the device came for are all there — the point of
      // widening the read is that the screen WORKS from a phone.
      expect(record.bindAll, 'the LAN bind this test turned on must be reported').toBe(true);

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
