// The shell, inside a real Android WebView: it boots under its fixed
// origin, redeems a pairing link, passes the platform's own lock, opens a
// thread, and answers the hardware back button.
//
// WRITTEN AGAINST THE PLAYWRIGHT ANDROID API, NOT YET RUN. There is no
// emulator on the box this was authored on, so every line below is
// written from `_android` / `AndroidDevice` / `AndroidWebView` as the
// current docs describe them and from the app's own contracts. The first
// real execution is the Mac pass. Do not read a green `make e2e-android`
// on a laptop with no device as evidence for any of it: the script exits
// 0 when nothing is attached, on purpose.
//
// WHAT ONLY THIS CAN ANSWER. `compact-shell-origin.spec.ts` proves the
// cross-origin transport in a real browser against the real Go server,
// which is the half of the shell a laptop can test. It cannot say whether
// the bundle BOOTS under `https://shell.agent-overflow.invalid`, whether
// the Capacitor plugins the APK was built with register, whether the app
// lock actually gates the app, or whether a hardware back press reaches
// `showCompactList`. Those are facts about a device.
//
// ---------------------------------------------------------------------
// WHY `adb reverse` AND `127.0.0.1`, NOT `10.0.2.2`
// ---------------------------------------------------------------------
// The emulator's documented alias for the host loopback is `10.0.2.2`,
// and it is the obvious address for the harness backend. It cannot be
// used here, for two independent reasons, and both of them are checks
// this repo's own code performs:
//
//  1. **Mixed content.** The page's origin is `https://`. Capacitor
//     leaves the WebView at the platform default
//     (`CapConfig.allowMixedContent` is false and `Bridge` only calls
//     `setMixedContentMode(ALWAYS_ALLOW)` when it is true), so
//     `MIXED_CONTENT_NEVER_ALLOW` stands and a fetch or a WebSocket to
//     `http://10.0.2.2:<port>` is blocked by Blink before it reaches the
//     network. `http://127.0.0.1:<port>` is not: Chromium treats
//     loopback as a potentially trustworthy origin, so it is not mixed
//     content at all. Turning mixed content on in `capacitor.config.ts`
//     would apply to the RELEASE build too, which is exactly the door
//     the phone's tailnet-TLS ruling closes.
//
//  2. **The loopback Host guard.** `transport.Server.loopbackHostGuard`
//     answers 404 to any non-loopback `Host` header while the listener is
//     bound to loopback, which is the DNS-rebinding defence. A request
//     addressed to `10.0.2.2:<port>` carries exactly such a Host, so the
//     manifest fetch would 404 unless the spec first flipped the whole
//     backend to a LAN bind — persisted settings, a rebind, and a wider
//     listener, for a test that needs none of it.
//
// `adb reverse tcp:<port> tcp:<port>` makes the DEVICE's own loopback
// forward to the host's, so the phone reaches the harness at
// `http://127.0.0.1:<port>` and both problems disappear. The happy
// consequence is that the pairing payload needs no surgery: the harness
// binds loopback, so `MintDevicePairing` already names
// `http://127.0.0.1:<port>` as its endpoint and this spec redeems the
// link exactly as it was minted.
//
// The one thing the emulator still needs is permission to speak cleartext
// to that address, which `mobile/android/app/src/debug/` grants for
// `127.0.0.1` and only there (see mobile/AGENTS.md).
//
// ---------------------------------------------------------------------
// WHY IT OWNS ITS BACKEND
// ---------------------------------------------------------------------
// There is no worker fixture here — this config has no `tests/fixtures.ts`
// — and there could not be one: the port has to exist before `adb reverse`
// can name it, and the reverse forward has to exist before the WebView
// can load anything. So the backend, the forward and the device are one
// fixture chain, torn down in the reverse order they were built.

import { execFile } from 'node:child_process';
import * as os from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';

import {
  _android,
  expect,
  test as base,
  type AndroidDevice,
  type Page,
} from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';

const run = promisify(execFile);

/** The shell's application id (`mobile/capacitor.config.ts`, `appId`). */
const SHELL_PACKAGE = 'dev.agentoverflow.app';

/**
 * The origin `mobile/capacitor.config.ts` fixes and
 * `internal/transport/shellorigin.go` admits as `ShellOrigin`. Asserting
 * it here is the third leg of that agreement: the two constants name each
 * other in comments, and this is the run that catches them disagreeing.
 */
const SHELL_ORIGIN = 'https://shell.agent-overflow.invalid';

/**
 * The PIN `scripts/android-smoke.sh` sets before the run and clears after.
 * The emulator has no biometric, and `native/lock.ts` passes
 * `allowDeviceCredential: true` precisely so a device without one falls
 * back to the credential it is unlocked with — so the device PIN IS the
 * app lock here, and typing it is answering the real prompt.
 */
const LOCK_PIN = '1234';

/** Long enough for a cold WebView on an emulator, short enough to fail. */
const WEBVIEW_MS = 120_000;
/** The pairing screen probes every 3s and awaits a bounded redial after. */
const PAIRED_MOUNT_MS = 60_000;

// ---------------------------------------------------------------------
// Wire shapes, mirroring internal/app/app_access_types.go.
// ---------------------------------------------------------------------

interface PairingInvite {
  linkId: string;
  url: string;
}

interface PendingPairing {
  linkId: string;
  redeemed?: boolean;
  verificationNumber?: string;
}

interface AccessOverview {
  pendingPairings?: PendingPairing[];
}

interface SeedResult {
  projects: Array<{ projectId: string; path: string; threadIds: string[] }>;
}

/** The `adb` the smoke script found, by the same rule the script uses. */
function adbPath(): string {
  const home = process.env.ANDROID_HOME ?? path.join(os.homedir(), 'Android', 'Sdk');
  return path.join(home, 'platform-tools', 'adb');
}

/** The `#pair=` fragment off a minted link, which is all a device needs. */
function fragmentOf(invite: PairingInvite): string {
  const at = invite.url.indexOf('#pair=');
  expect(at, 'a pairing link must carry its payload in the fragment').toBeGreaterThan(-1);
  return invite.url.slice(at);
}

/**
 * The window Android currently gives focus to, as `dumpsys` names it.
 *
 * Used to know when the platform's credential prompt is actually up. The
 * prompt is drawn OVER the WebView by the OS, so nothing in the page can
 * see it — but the app's own window losing focus is what a system modal
 * over it means, and that is true of every credential UI regardless of
 * what a given system image calls its activity. Matching on the app's
 * package rather than on the prompt's name is what keeps this from being
 * a guess about one Android build.
 */
async function focusedWindow(device: AndroidDevice): Promise<string> {
  const dump = (await device.shell('dumpsys window')).toString();
  return /mCurrentFocus=(.*)/.exec(dump)?.[1]?.trim() ?? '';
}

/** The owner's half: find the redeemed link, compare the number, confirm. */
async function confirmOnHost(harness: HarnessApp, shownOnDevice: string): Promise<void> {
  let redeemed: PendingPairing | undefined;
  await expect
    .poll(async () => {
      const overview = await harness.rpc<AccessOverview>('GetAccessOverview');
      redeemed = (overview.pendingPairings ?? []).find((p) => p.redeemed);
      return redeemed?.verificationNumber ?? '';
    }, { message: 'the redemption must reach the backend as a pairing awaiting confirmation' })
    // The comparison IS the gate: the number is an HMAC over the key the
    // device actually presented, so a redemption that never crossed the
    // reverse forward could not produce one this side matches.
    .toBe(shownOnDevice);
  await harness.rpc('ConfirmDevicePairing', redeemed!.linkId);
}

interface ShellFixtures {
  device: AndroidDevice;
  harness: HarnessApp;
  page: Page;
}

const test = base.extend<ShellFixtures>({
  // The emulator or phone the smoke script chose. It passes the serial
  // because `adb devices` may list several and `_android.devices()` has no
  // filter of its own; with none set, the sole device is taken and more
  // than one is an error rather than a coin flip.
  device: async ({}, use) => {
    const wanted = process.env.AO_ANDROID_SERIAL ?? '';
    const devices = await _android.devices();
    expect(devices, 'make e2e-android runs only with a device attached').not.toHaveLength(0);
    const device =
      wanted === ''
        ? devices[0]
        : devices.find((candidate) => candidate.serial() === wanted);
    expect(device, `no attached device answers to serial ${wanted}`).toBeDefined();
    expect(
      wanted !== '' || devices.length === 1,
      'several devices are attached and none was named: set AO_ANDROID_SERIAL',
    ).toBe(true);
    await use(device!);
    await device!.close();
  },

  // The backend, plus the reverse forward that is the only reason the
  // phone can reach it. Torn down in the order it was built up, so a
  // failed run does not leave a forward pointing at a dead port.
  harness: async ({ device }, use) => {
    const harness = await launchHarness();
    const port = String(harness.bootstrap.port);
    const serial = device.serial();
    await run(adbPath(), ['-s', serial, 'reverse', `tcp:${port}`, `tcp:${port}`]);
    try {
      await use(harness);
    } finally {
      await run(adbPath(), ['-s', serial, 'reverse', '--remove', `tcp:${port}`]).catch(
        () => undefined,
      );
      await harness.close();
    }
  },

  // The shell's own WebView, as an ordinary Playwright Page. Everything
  // after this line is the same API every other spec in this repo uses,
  // which is the point: the app under test is the app that ships, in the
  // container that ships it.
  page: async ({ device }, use) => {
    const webView = await device.webView({ pkg: SHELL_PACKAGE }, { timeout: WEBVIEW_MS });
    await use(await webView.page());
  },
});

test('the shell boots at its own origin, pairs, unlocks, and navigates', async ({
  device,
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'shell-boot',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            // A thread with a real turn: a draft row is hidden from the
            // sidebar, so a seed without one would assert on nothing.
            title: 'Shell boot thread',
            turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }],
          },
        ],
      },
    ],
  });
  expect(seed.projects[0].threadIds, 'the fixture must seed one visible thread').toHaveLength(1);

  // --- The door: a `#pair=` hash on the shell's own origin -------------
  // `main.ts` checks the hash BEFORE the first-run screen for every
  // client, and on a shell it adopts the payload's endpoint through
  // `native/boot.adoptPairingEndpoint` before the pairing screen issues
  // its first request. That contract is what makes this navigation the
  // whole setup: no QR camera, no typed link, and no test-only door.
  //
  // The goto is a same-document hash change (the activity is already at
  // this origin), so the reload is what actually runs `main.ts` again
  // with the hash present. Both lines are load-bearing.
  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full');
  await page.goto(SHELL_ORIGIN + '/' + fragmentOf(invite));
  await page.reload();

  // --- 1. The bundle booted under the fixed origin ---------------------
  // The agreement `mobile/capacitor.config.ts` and
  // `internal/transport/shellorigin.go` make with each other, checked
  // from inside the running WebView rather than from either constant.
  expect(await page.evaluate(() => window.location.origin)).toBe(SHELL_ORIGIN);

  // --- 2. Pairing completes across the reverse forward -----------------
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
  await page.getByLabel('Device name').fill('Emulator shell');
  await page.getByRole('button', { name: 'Pair' }).click();
  const shown = page.getByLabel('Verification number');
  await expect(shown).toBeVisible();
  await confirmOnHost(harness, ((await shown.textContent()) ?? '').trim());

  // --- 3. The lock gates the app, and the device credential passes it --
  // The lock screen is mounted BEFORE the app so a phone never flashes a
  // transcript on its way to being locked, so this assertion also proves
  // the gate is in front rather than behind.
  const lock = page.getByTestId('app-lock');
  await expect(lock).toBeVisible({ timeout: PAIRED_MOUNT_MS });
  await expect
    .poll(
      async () => {
        const focus = await focusedWindow(device);
        return focus !== '' && !focus.includes(SHELL_PACKAGE);
      },
      { message: "the platform's own credential prompt must take focus off the app" },
    )
    .toBe(true);
  await device.shell(`input text ${LOCK_PIN}`);
  await device.shell('input keyevent 66');
  await expect(lock).toBeHidden();

  // --- 4. The app is behind it, warm, with the seeded row --------------
  const row = page.getByTestId('thread-row').filter({ hasText: 'Shell boot thread' });
  await expect(row).toBeVisible({ timeout: PAIRED_MOUNT_MS });
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');

  // --- 5. A tap opens the thread ---------------------------------------
  await row.click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('chat-header-title')).toHaveText('Shell boot thread');

  // --- 6. The hardware back button reaches showCompactList -------------
  // KEYCODE_BACK. It arrives as the Capacitor App plugin's `backButton`
  // event, which `native/lifecycle.ts` turns into `showCompactList` — and
  // that path exists ONLY on a device, which is the whole reason this
  // file is not a `compact-*.spec.ts`.
  await device.shell('input keyevent 4');
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'list');

  // --- 7. The Capacitor bridge is really in the page -------------------
  // `isNativeShell()` branches every seam in `frontend/src/lib/native/` on
  // this, and every unit test for those seams stubs it. This is the one
  // place the real bridge answers.
  expect(
    await page.evaluate(
      () =>
        (window as Window & { Capacitor?: { isNativePlatform?: () => boolean } }).Capacitor
          ?.isNativePlatform?.() === true,
    ),
  ).toBe(true);
});
