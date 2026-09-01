// The remote-device lifecycle, end to end, in a real browser that is not
// on this machine as far as the backend can tell: pair through the
// pairing screen, be revoked, be restored, be re-paired narrower, be
// forgotten, enroll again — and what the owner's own screen says about
// each of those states.
//
// WHY THIS FILE EXISTS. Every bug waves 7b/7c1/7c2 fixed was found by a
// person with a second machine on a couch, and every one of them was
// machine-INDEPENDENT: the post-pairing error burst, a re-revoke that
// silently swept nothing, a view-only session spending one refusal per
// pane, a device list that could not tell "connected" from "signed out".
// The mechanics that make an off-host peer reproducible on one host were
// already in `harness-offhost-authz.spec.ts`; what was missing was
// driving the LIFECYCLE through them, so the next regression of that
// class is caught by `make e2e` rather than by a human.
//
// WHAT IS REAL HERE. The pairing screen is the shipped
// `PairingScreen.svelte` on a `#pair=` fragment, the redemption is the
// shipped `deviceSession.redeemPairing`, the credential is the one
// `/auth/pair` issued, and the app on the other side is the shipped
// `App.svelte` mounted by `main.ts` after `redialAfterPairing`. The only
// step spoken over the wire instead of clicked is the owner's
// CONFIRMATION, which is a host-side RPC (`ConfirmDevicePairing`) the
// modal issues verbatim — and the spec still compares the number the
// phone displays against the number the host holds first, because that
// comparison IS the gate.
//
// The phone is a plain-HTTP LAN origin, so it is not a secure context,
// `crypto.subtle` does not exist, and the device enrolls `bearer` with
// the 32-CSPRNG-byte identifier `deviceSession.deviceKeyThumbprint()`
// mints once per origin (spec §15 constraint 6). That is the real shape
// of a LAN browser, and it is what makes "the SAME context re-pairs into
// the SAME device row" and "a forgotten key enrolls again as a NEW row"
// observable at all: the identity lives in that context's localStorage
// and outlives every session on top of it.
//
// WHY IT OWNS ITS BACKEND, and shares one across the file: the LAN-bind
// preference PERSISTS to the settings file and REBINDS the listener, and
// `harness.reset()` undoes neither — so borrowing the worker fixture's
// instance would hand the next spec a LAN-bound backend. The five cases
// are one choreography on one device row, so they run `.serial` and
// carry state forward deliberately rather than re-staging a pairing per
// case.

import { expect, test, type BrowserContext, type Page } from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  instrument,
  mintLink,
  nonLoopbackIPv4,
  pairDeviceOverWire,
  type PairingInvite,
  type Surfaced,
} from './offhost-helpers.js';

// ---------------------------------------------------------------------
// Wire shapes (internal/app/app_access_types.go).
// ---------------------------------------------------------------------

interface AccessSession {
  id: string;
  binding: string;
  awaitingConfirmation?: boolean;
  lastUsedAtMs?: number;
  connections?: number;
  scopes?: string[];
  survivedRevocation?: boolean;
}

interface AccessDevice {
  id: string;
  label: string;
  class: string;
  platform?: string;
  channel?: string;
  lastSeenAtMs?: number;
  revokedAtMs?: number;
  sessions?: AccessSession[];
}

interface PendingPairing {
  linkId: string;
  redeemed?: boolean;
  deviceLabel?: string;
  verificationNumber?: string;
}

interface AccessOverview {
  devices: AccessDevice[];
  pendingPairings?: PendingPairing[];
}

interface DeviceRevocationResult {
  deviceMoved: boolean;
  sessionsEnded: number;
  connectionsClosed: number;
}

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

// ---------------------------------------------------------------------
// Budgets. Every one of these is wall-clock against a mechanism whose
// own constant is named, never a count of loop turns
// (frontend/AGENTS.md § Testing).
// ---------------------------------------------------------------------

// The pairing screen probes for the owner's confirmation every 3s
// (PairingScreen.PROBE_INTERVAL_MS), holds the confirmed frame for 700ms,
// and then AWAITS `redialAfterPairing`, which is itself bounded at
// REDIAL_SETTLE_BUDGET_MS (5s). ~9s is the designed worst case, so this
// is roughly twice it.
const PAIRED_APP_MOUNT_MS = 20_000;

// A stopped reconnect ladder publishes no further status. RECONNECT_INITIAL_MS
// is 250ms and the ladder grows from there, so this window is ~8 attempts
// at its FASTEST step: a ladder that were still running could not stay
// silent across it.
const LADDER_SILENCE_MS = 2_000;

/** The two states the client latches on and stops the ladder for. */
const TERMINAL_STATUSES = ['unauthorized', 'pairing-required'];

// ---------------------------------------------------------------------
// The flow, driven the way the person is.
// ---------------------------------------------------------------------

const lanIP = nonLoopbackIPv4();

// The label the phone enrolls under, carried across cases: cluster 5
// reads the device list back and has to find this row by name.
const PHONE_LABEL = 'Couch browser';

async function mintInvite(
  harness: HarnessApp,
  access: 'full' | 'view-only',
): Promise<PairingInvite> {
  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'browser', access);
  // The precondition the whole file rests on: a link that points at
  // loopback would pair a device that is not off-host, and every
  // assertion below would be about the wrong peer.
  expect(
    new URL(invite.url.split('#')[0]).hostname,
    'the pairing link must point at this host by a LAN address, not by loopback',
  ).toBe(lanIP);
  return invite;
}

/**
 * Walk the pairing screen to the point where it is showing a number and
 * waiting: name the device, spend the link, display the verification
 * number. Answers the number the phone is showing.
 */
async function redeemOnScreen(page: Page, invite: PairingInvite, label: string): Promise<string> {
  await page.goto(invite.url);
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
  await page.getByLabel('Device name').fill(label);
  await page.getByRole('button', { name: 'Pair' }).click();
  const shown = page.getByLabel('Verification number');
  await expect(shown).toBeVisible();
  return ((await shown.textContent()) ?? '').trim();
}

/**
 * The owner's half: find the redeemed link, compare the number, confirm.
 * The comparison is not decoration — the number is an HMAC over the key
 * the device actually presented (internal/identity/pairing.go), so a
 * device that redeemed with a different key could not display one this
 * side matches.
 */
async function confirmOnHost(harness: HarnessApp, shownOnDevice: string): Promise<void> {
  let redeemed: PendingPairing | undefined;
  await expect
    .poll(
      async () => {
        const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
        redeemed = (overview.pendingPairings ?? []).find((p) => p.redeemed);
        return redeemed?.verificationNumber ?? '';
      },
      { message: 'the redemption must reach the host as a pairing awaiting confirmation' },
    )
    // The comparison IS the gate, so it is the poll's own predicate: a
    // number that never matches fails here rather than confirming
    // whatever happened to be pending.
    .toBe(shownOnDevice);
  await harness.rpc('ConfirmDevicePairing', redeemed!.linkId);
}

/** Every device row that is not this backend's own page channel. */
async function pairedDevices(harness: HarnessApp): Promise<AccessDevice[]> {
  const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
  return overview.devices.filter((d) => d.channel !== 'local');
}

/** The one paired device row, asserted to be the only one. */
async function solePairedDevice(harness: HarnessApp): Promise<AccessDevice> {
  const devices = await pairedDevices(harness);
  expect(devices, 'exactly one paired device is expected at this point').toHaveLength(1);
  return devices[0];
}

test.describe.serial('remote device lifecycle', () => {
  // Not green-washed: a host with no non-loopback interface (a locked-down
  // CI container) genuinely cannot produce the peer this spec is about, and
  // saying so is the honest outcome. A skip is visible in the report; a
  // vacuous pass is not.
  test.skip(
    lanIP === null,
    'no non-loopback IPv4 interface on this host, so no off-host peer can be produced',
  );

  let harness: HarnessApp;
  let phoneContext: BrowserContext;
  let phone: Page;
  let surfaced: Surfaced;
  let threadIds: string[] = [];

  test.beforeAll(async ({ browser }) => {
    harness = await launchHarness();

    // The production LAN-bind path: rebinds to 0.0.0.0 on the same port
    // and installs the WS origin allow-list in one step.
    const network = await harness.rpc<{ bindAll: boolean }>('SetNetworkSettings', {
      bindAll: true,
    });
    expect(network.bindAll).toBe(true);

    // Two threads with real turns: a draft row is hidden from the sidebar,
    // and the view-only case needs a SWITCH between two visible threads.
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        {
          name: 'remote-lifecycle',
          repo: {},
          threads: [
            {
              title: 'Kitchen sink',
              turns: [{ userText: 'first', items: [{ kind: 'assistant_text', summary: 'one' }] }],
            },
            {
              title: 'Second thread',
              turns: [{ userText: 'second', items: [{ kind: 'assistant_text', summary: 'two' }] }],
            },
          ],
        },
      ],
    });
    threadIds = seed.projects[0].threadIds;
    expect(threadIds, 'the fixture must seed two visible threads').toHaveLength(2);

    phoneContext = await browser.newContext();
    phone = await phoneContext.newPage();
    surfaced = await instrument(phone);
  });

  test.afterAll(async () => {
    await phoneContext?.close();
    // Leave the instance as we found it even though nothing else shares
    // it — the settings file outlives the listener, and a future reader
    // of this data dir should not find a LAN bind nobody asked for.
    await harness?.rpc('SetNetworkSettings', { bindAll: false }).catch(() => undefined);
    await harness?.close();
  });

  // -------------------------------------------------------------------
  // 1. Pairing completes cleanly.
  // -------------------------------------------------------------------
  test('a full-access device pairs through the screen and the app mounts with nothing surfaced', async () => {
    const invite = await mintInvite(harness, 'full');
    const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
    expect(shown, 'the verification number is six digits, leading zeros kept').toMatch(/^\d{6}$/);

    // The credential exists and admits nothing until this call. Before it,
    // the session is present and awaiting confirmation — which is the
    // pending state costing no second credential (identity/AGENTS.md).
    const beforeConfirm = await solePairedDevice(harness);
    expect(
      (beforeConfirm.sessions ?? []).every((s) => s.awaitingConfirmation),
      'a redeemed but unconfirmed session must admit nothing',
    ).toBe(true);

    await confirmOnHost(harness, shown);

    // The regression this case exists for: the app mounts on the far side
    // of an AWAITED `redialAfterPairing`, so the boot fan-out is issued
    // against a settled transport. Mounting earlier surfaced ~20 failures
    // for a pairing that had worked (fixed in daff9f20).
    await expect(phone.getByTestId('thread-row')).toHaveCount(2, {
      timeout: PAIRED_APP_MOUNT_MS,
    });
    // Sorted, because sidebar order is recency and this case is about the
    // rows ARRIVING — asserting the order here would fail the day a seed
    // writes its turns a millisecond differently.
    expect((await phone.getByTestId('thread-row-title').allTextContents()).sort()).toEqual([
      'Kitchen sink',
      'Second thread',
    ]);

    // A full-access device is not view-only and is not inert. The composer
    // mounts with a thread, so opening one is part of the assertion rather
    // than setup for it — a paired browser that lists rows it cannot open
    // is the same defect one screen further in.
    await expect(phone.getByTestId('view-only-indicator')).toHaveCount(0);
    await phone.getByTestId('thread-row').first().click();
    await expect(phone.getByLabel('Message Input')).toBeEnabled();
    await expect(phone.getByTestId('project-item-new-thread').first()).toBeEnabled();

    // Nothing was surfaced for a pairing that worked, on either channel.
    expect(surfaced.errorToasts, 'a successful pairing must surface no error toast').toEqual([]);
    expect(surfaced.consoleErrors, 'a successful pairing must log no console error').toEqual([]);
    // The banner is an overlay that renders nothing on the happy path, so
    // the honest assertion is that it never published a terminal state —
    // a transient 'reconnecting' during the redial is not a failure.
    expect(
      surfaced.transportStatuses.filter((s) => TERMINAL_STATUSES.includes(s)),
      'a paired page must not latch a terminal transport state',
    ).toEqual([]);
  });

  // -------------------------------------------------------------------
  // 1b. Settings → Network from the phone. Second in EXECUTION order
  //     because it needs the device the case above paired, still full
  //     access and still attached — the case below revokes it.
  // -------------------------------------------------------------------
  test('a full-access device manages the backend’s exposure, without being handed its credentials', async () => {
    await phone.getByTestId('sidebar-settings-button').click();
    await expect(phone.getByRole('tablist', { name: 'Settings Sections' })).toBeVisible();
    await phone.getByRole('tab', { name: 'Network' }).click();

    // It LOADS, which is the whole widening: read as `host`, this section
    // drew its unavailable arm for every paired device — the owner's own
    // phone included — and Settings → Network was reachable from nowhere
    // but the machine. `access:admin` is what it answers now.
    await expect(phone.getByRole('switch', { name: 'Toggle remote access' })).toBeVisible();
    await expect(phone.getByTestId('network-section-local-only')).toHaveCount(0);

    // And the credential half is not in what it loaded: no share URL field
    // and no Copy beside it, one sentence saying where they are. The wire
    // form of the same claim — that the bytes never left the machine — is
    // harness-offhost-authz.spec.ts; this is what the person sees.
    await expect(phone.getByTestId('share-url-host-only')).toBeVisible();
    await expect(phone.getByLabel('Application URL')).toHaveCount(0);
    await expect(phone.getByTestId('insecure-url-warning')).toHaveCount(0);
  });

  // -------------------------------------------------------------------
  // 2. Revocation is absolute, end to end.
  // -------------------------------------------------------------------
  test('revoking ends the live socket for good, and a second revoke says nothing was live', async () => {
    const device = await solePairedDevice(harness);
    const live = (device.sessions ?? []).filter((s) => !s.awaitingConfirmation);
    expect(live, 'the paired device must hold one live session').toHaveLength(1);
    expect(
      live[0].connections ?? 0,
      'the paired page must be attached before the revocation means anything',
    ).toBeGreaterThan(0);

    const first = await harness.rpc<DeviceRevocationResult>('RevokeAccessDevice', device.id);
    expect(first).toMatchObject({ deviceMoved: true, sessionsEnded: 1 });
    expect(first.connectionsClosed, 'the live socket is closed synchronously').toBeGreaterThan(0);

    // The page lands in a terminal state rather than a reconnect loop. The
    // ladder stops because the manifest refetch presents a credential this
    // backend now refuses and the renewal behind it is refused too.
    const banner = phone.getByTestId('transport-status-banner');
    await expect(banner).toBeVisible();
    await expect
      .poll(() => surfaced.transportStatuses.at(-1), {
        message: 'the paired page must latch a terminal transport state after its device is revoked',
      })
      .toMatch(/^(unauthorized|pairing-required)$/);
    const latched = surfaced.transportStatuses.at(-1)!;
    const publishedByThen = surfaced.transportStatuses.length;

    // Stopped, not slow. A ladder still running republishes within
    // RECONNECT_INITIAL_MS; nothing is published across this window.
    await phone.waitForTimeout(LADDER_SILENCE_MS);
    expect(
      surfaced.transportStatuses.slice(publishedByThen),
      'a latched client must not re-enter the reconnect ladder',
    ).toEqual([]);
    await expect(banner).toHaveAttribute('data-status', latched);

    // Re-revoking is legitimate and answers honestly. The device row does
    // not move a second time and there was nothing left to sweep — the
    // answer that used to be indistinguishable from the first one, which
    // is how a device that kept access went unnoticed (spec §2).
    const second = await harness.rpc<DeviceRevocationResult>('RevokeAccessDevice', device.id);
    expect(second).toEqual({ deviceMoved: false, sessionsEnded: 0, connectionsClosed: 0 });

    // Restore says "that is still my device": it re-admits the KEY to
    // pairing and moves no credential, so the page stays dead until a
    // fresh link is redeemed on it.
    await harness.rpc('RestoreAccessDevice', device.id);
    const restored = await solePairedDevice(harness);
    expect(restored.revokedAtMs ?? 0, 'a restored device row is no longer revoked').toBe(0);
    expect(restored.sessions ?? [], 'restoring hands back no credential').toEqual([]);

    // Re-pairing the SAME context: same stored key thumbprint, so the
    // existing row is adopted rather than a second one accumulating —
    // and narrower this time, which is the fixture the next case needs.
    const invite = await mintInvite(harness, 'view-only');
    const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
    await confirmOnHost(harness, shown);
    await expect(phone.getByTestId('thread-row')).toHaveCount(2, {
      timeout: PAIRED_APP_MOUNT_MS,
    });

    const adopted = await solePairedDevice(harness);
    expect(adopted.id, 'a known key must ADOPT its device row rather than mint a second').toBe(
      device.id,
    );
  });

  // -------------------------------------------------------------------
  // 4. View-only degradation, proven at the wire. Third in EXECUTION
  //    order on purpose: the device is view-only only because the case
  //    above re-paired it that way, and the one below re-pairs it full
  //    again — the numbering names the cluster, the position is the
  //    choreography.
  // -------------------------------------------------------------------
  test('a view-only device is inert, says so, and spends no refusal on a passive load', async () => {
    // A fresh document so the capture covers the whole boot fan-out, not
    // only what happened after the redial in the previous case.
    surfaced.errorToasts.length = 0;
    surfaced.consoleErrors.length = 0;
    surfaced.refusals.length = 0;
    surfaced.rpcReplies.length = 0;
    await phone.reload();
    await expect(phone.getByTestId('thread-row')).toHaveCount(2, {
      timeout: PAIRED_APP_MOUNT_MS,
    });

    // The ambient marker, and the two controls a person would reach for.
    // A gated control stays MOUNTED and goes inert — never hidden, never a
    // click that swallows itself (transport/AGENTS.md).
    await expect(phone.getByTestId('view-only-indicator')).toBeVisible();
    await expect(phone.getByLabel('Message Input')).toBeDisabled();
    await expect(phone.getByTestId('project-item-new-thread').first()).toBeDisabled();

    // A representative navigation: open a thread, switch to the other,
    // open settings. Each of those mounts panes whose stores fire passive
    // loads, which is exactly the burst this asserts the absence of.
    const rows = phone.getByTestId('thread-row');
    await rows.first().click();
    await expect(phone.getByLabel('Message Input')).toBeVisible();
    await rows.last().click();
    await expect(phone.getByLabel('Message Input')).toBeVisible();
    await phone.getByTestId('sidebar-settings-button').click();
    await expect(phone.getByRole('tablist', { name: 'Settings Sections' })).toBeVisible();
    await phone.getByRole('tab', { name: 'Network' }).click();
    await expect(phone.getByRole('tab', { name: 'Network' })).toHaveAttribute(
      'aria-selected',
      'true',
    );

    // Both of that tab's sections settled, and settled is a RENDERED state
    // rather than a moment in time — without waiting on it, the assertion
    // below races the very loads it is about and passes whenever it wins
    // (which it did, two runs in three).
    //
    // Neither section is a control this device could use: both are
    // `access:admin`, which view-only is not — the same grant that let the
    // full-access phone read the network settings two cases up is the one
    // this device does not hold. Both must say so from what they were
    // granted, not discover it from a refusal.
    await expect(phone.getByTestId('network-section-local-only')).toBeVisible();
    await expect(phone.getByTestId('devices-section-unavailable')).toBeVisible();

    // The strongest form of the claim, read off the wire rather than off a
    // surface: a load that ran because a pane mounted has nobody to report
    // a refusal to, so it must not issue one at all
    // (stores/viewOnlyPassiveLoads.test.ts is the unit sweep; this is the
    // same contract proven end to end).
    //
    // Its precondition first: the capture has to have SEEN the wire. A
    // boot fan-out is ~20 calls, so a handful is a floor no working page
    // misses and no broken capture reaches.
    expect(
      surfaced.rpcReplies.length,
      'the wire capture must have observed this session, or the emptiness below means nothing',
    ).toBeGreaterThan(5);
    expect(
      surfaced.refusals,
      'a passive load must check its grant before it fires, not discover the refusal',
    ).toEqual([]);
    expect(surfaced.errorToasts, 'a view-only session must surface no error toast').toEqual([]);
    expect(surfaced.consoleErrors, 'a view-only session must log no console error').toEqual([]);
  });

  // -------------------------------------------------------------------
  // 3. Forget-device.
  // -------------------------------------------------------------------
  test('forgetting needs a revocation first, and frees the key to enroll again', async () => {
    const device = await solePairedDevice(harness);

    // Revoke, then forget. Deleting the row first would remove the only
    // handle on a device that still holds credentials.
    await expect(
      harness.rpc('ForgetAccessDevice', device.id),
      'forgetting an un-revoked device must refuse and say why',
    ).rejects.toThrow(/still has access/);

    await harness.rpc<DeviceRevocationResult>('RevokeAccessDevice', device.id);
    await harness.rpc('ForgetAccessDevice', device.id);
    expect(
      await pairedDevices(harness),
      'a forgotten device is gone from the overview, revoked list included',
    ).toEqual([]);

    // The key becomes free: the unique index is over the SURVIVING rows,
    // so the same browser — same stored thumbprint — enrolls again. It
    // still costs an owner-minted link and a number the owner compared,
    // so nothing returns unwatched.
    const invite = await mintInvite(harness, 'full');
    const shown = await redeemOnScreen(phone, invite, PHONE_LABEL);
    await confirmOnHost(harness, shown);
    await expect(phone.getByTestId('thread-row')).toHaveCount(2, {
      timeout: PAIRED_APP_MOUNT_MS,
    });

    const reborn = await solePairedDevice(harness);
    expect(reborn.id, 'a forgotten row does not come back; the key enrolls a NEW one').not.toBe(
      device.id,
    );
    await expect(phone.getByTestId('view-only-indicator')).toHaveCount(0);
    await expect(phone.getByLabel('Message Input')).toBeEnabled();
  });

  // -------------------------------------------------------------------
  // 5. Devices list truth, on the owner's own screen.
  // -------------------------------------------------------------------
  test('the device list names what each device can do, whether it is here, and what a revoke ended', async ({
    browser,
  }) => {
    const port = harness.bootstrap.port;

    // Two more rows so the list has something to distinguish. Paired over
    // the wire rather than through a browser: the surface under test is
    // the owner's device LIST, and backend setup goes through RPCs.
    const tablet = await pairDeviceOverWire(
      harness,
      lanIP!,
      port,
      mintLink(await mintInvite(harness, 'view-only')),
      'e2e-sofa-tablet-key',
      'Sofa tablet',
    );
    expect(tablet.scopes ?? [], 'a view-only link grants the observe tier only').not.toEqual([]);
    const laptop = await pairDeviceOverWire(
      harness,
      lanIP!,
      port,
      mintLink(await mintInvite(harness, 'full')),
      'e2e-old-laptop-key',
      'Old laptop',
    );
    // A device that is paired and holds nothing: the state that used to
    // read exactly like a device that was connected.
    await harness.rpc('RevokeAccessSession', laptop.sessionId);

    const host = await browser.newPage();
    try {
      await harness.open(host);
      await host.getByTestId('sidebar-settings-button').click();
      await host.getByRole('tab', { name: 'Network' }).click();

      const deviceRows = host.getByTestId('access-device');
      const localRow = deviceRows.filter({ hasText: 'This computer' });
      const phoneRow = deviceRows.filter({ hasText: PHONE_LABEL });
      const tabletRow = deviceRows.filter({ hasText: 'Sofa tablet' });
      const laptopRow = deviceRows.filter({ hasText: 'Old laptop' });
      await expect(deviceRows).toHaveCount(4);

      // The backend's own page channel is not a device somebody paired,
      // and revoking it would sign this window out — so no affordance is
      // drawn that can only fail.
      await expect(localRow.getByRole('button', { name: /Revoke/ })).toHaveCount(0);

      // What each device CAN DO, read off the grant set rather than off a
      // device class: both of these are class `browser`.
      await expect(tabletRow).toContainText('View only');
      await expect(phoneRow).not.toContainText('View only');

      // Whether it is here. The phone is attached; the laptop holds no
      // credential at all.
      await expect(phoneRow).toContainText('connected now');
      await expect(laptopRow).toContainText('signed out');
      await expect(tabletRow).not.toContainText('connected now');

      // And what a revoke ENDED, in the words the backend answered with.
      // Two-step: the first click arms, the second commits.
      const revoke = tabletRow.getByRole('button', { name: /Revoke/ });
      await revoke.click();
      await expect(tabletRow.getByRole('button', { name: 'Confirm revoke' })).toBeVisible();
      await tabletRow.getByRole('button', { name: 'Confirm revoke' }).click();
      await expect(
        host.getByTestId('toast').filter({ hasText: 'Sofa tablet' }),
      ).toContainText('Revoked Sofa tablet. 1 session ended.');
      await expect(host.getByTestId('revoked-device').filter({ hasText: 'Sofa tablet' })).toHaveCount(
        1,
      );
    } finally {
      await host.close();
    }
  });
});
