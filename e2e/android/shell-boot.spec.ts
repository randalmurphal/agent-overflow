// The shell, inside a real Android WebView: it boots under its fixed
// origin, redeems a pairing link, passes the platform's own lock, opens a
// thread, and answers the hardware back button.
//
// FIRST RUN 2026-09-03, on a Mac against an arm64 android-36 emulator
// (Pixel-class AVD, no biometric, a device PIN). It was written earlier
// from the Playwright Android docs and the app's own contracts on a box
// with no emulator, and the first run paid for that: it found three shell
// defects the unit suites could not (a Capacitor plugin proxy that is a
// thenable, the home endpoint read after the transport had already
// dialled the shell's own origin, and the backend's bundle carrying
// stubbed native seams), and then two more in the lock (a permission
// prompt raised on top of the credential prompt, and the prompt's own
// resume re-locking the app). Do not read a green `make e2e-android` on
// a laptop with no device as evidence for any of it: the script exits 0
// when nothing is attached, on purpose.
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
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';

import {
  _android,
  expect,
  test as base,
  type AndroidDevice,
  type Locator,
  type Page,
} from '@playwright/test';

import { launchHarness, type HarnessApp } from '../src/harness.js';
import {
  RESULT_LINE,
  emit,
  seedAgentThread,
  startMock,
  textLines,
} from '../tests/agent-visibility-helpers.js';

const run = promisify(execFile);

/** The shell's application id (`mobile/capacitor.config.ts`, `appId`). */
const SHELL_PACKAGE = 'dev.agentoverflow.app';

/** Its one activity (`mobile/android/app/src/main/AndroidManifest.xml`). */
const SHELL_ACTIVITY = `${SHELL_PACKAGE}/.MainActivity`;

/**
 * The origin `mobile/capacitor.config.ts` fixes and
 * `internal/transport/shellorigin.go` admits as `ShellOrigin`. Asserting
 * it here is the third leg of that agreement: the two constants name each
 * other in comments, and this is the run that catches them disagreeing.
 */
const SHELL_ORIGIN = 'https://shell.agent-overflow.invalid';

/**
 * The intent extras `AndroidTray` puts on a notification's content
 * intent, spelled here rather than imported because Java constants do not
 * cross into TypeScript. A rename on either side is what this pair is for.
 */
const EXTRA_TARGET = 'dev.agentoverflow.app.push.TARGET';
const EXTRA_ID = 'dev.agentoverflow.app.push.ID';

/**
 * The PIN `scripts/android-smoke.sh` sets before the run and clears after.
 * The emulator has no biometric, and `native/lock.ts` passes
 * `allowDeviceCredential: true` precisely so a device without one falls
 * back to the credential it is unlocked with — so the device PIN IS the
 * app lock here, and typing it is answering the real prompt.
 */
const LOCK_PIN = '1234';

/**
 * A real phone instead of the emulator. The credential prompt there is the
 * OWNER'S own lockscreen, and typing `LOCK_PIN` at it would be a wrong-PIN
 * attempt against a real credential — Android escalates repeated wrong
 * attempts into a lockout, so the suite must never type at a prompt it did
 * not provision. With `AO_ANDROID_HUMAN_LOCK=1` the run waits for the owner
 * to answer each prompt themselves (PIN or biometric), which is also the
 * one way the biometric fallback path ever gets exercised: an emulator has
 * no finger. `scripts/android-smoke.sh` skips its PIN provisioning under
 * the same variable.
 */
const HUMAN_LOCK = process.env.AO_ANDROID_HUMAN_LOCK === '1';

/**
 * A real phone can be asleep behind its owner's keyguard, and an activity
 * started there renders frozen: DOM assertions still answer over CDP, but
 * nothing is clickable, because actionability waits on animation frames a
 * dozing display never produces (learned on the first Pixel run, where
 * every case stalled at its first click). So each case first wakes the
 * device and waits for the owner to unlock it — the one thing only they
 * can do. `KEYCODE_WAKEUP` is a no-op on a device that is already awake.
 */
async function awaitOwnerUnlock(device: AndroidDevice): Promise<void> {
  await device.shell('input keyevent KEYCODE_WAKEUP');
  await expect
    .poll(
      async () => {
        const dump = (await device.shell('dumpsys activity activities')).toString();
        return /mKeyguardShowing=false/.test(dump);
      },
      {
        message: 'the owner must unlock the phone before the suite can drive it',
        intervals: [1_000],
        timeout: 180_000,
      },
    )
    .toBe(true);
}

/**
 * Answer the platform credential prompt: typed on the emulator whose PIN
 * the smoke script set, answered by the owner's own hand on a real phone.
 * The generous human timeout is prompt-to-fingers latency, not machinery.
 */
async function passCredentialPrompt(device: AndroidDevice, lock: Locator): Promise<void> {
  if (!HUMAN_LOCK) {
    await device.shell(`input text ${LOCK_PIN}`);
    await device.shell('input keyevent 66');
  }
  await expect(lock).toBeHidden({ timeout: HUMAN_LOCK ? 120_000 : 30_000 });
}

/** Long enough for a cold WebView on an emulator, short enough to fail. */
const WEBVIEW_MS = 120_000;
/** The pairing screen probes every 3s and awaits a bounded redial after. */
const PAIRED_MOUNT_MS = 60_000;
/**
 * How long the shell's own bundle sync gets to download the harness's
 * bundle and stage it: a few MB over `adb reverse`, then the archive
 * crossing the Capacitor bridge as base64, then the unzip-and-verify.
 */
const BUNDLE_SYNC_MS = 90_000;

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

/** Whether the soft keyboard is on screen, as `dumpsys` reports it. */
async function keyboardShown(device: AndroidDevice): Promise<boolean> {
  const dump = (await device.shell('dumpsys input_method')).toString();
  return /mInputShown=true/.test(dump);
}

/**
 * The hardware back button, pressed for the APP.
 *
 * Opening a thread focuses the composer, and a focused field raises the
 * soft keyboard; a back press with the keyboard up closes the keyboard
 * and reaches nothing else, which is what every phone does. So the
 * keyboard is closed first, by the same key, and the press an assertion
 * is about is the one after it.
 */
async function pressBack(device: AndroidDevice): Promise<void> {
  if (await keyboardShown(device)) {
    await device.shell('input keyevent 4');
    await expect
      .poll(() => keyboardShown(device), { message: 'the first back press must close the keyboard' })
      .toBe(false);
  }
  await device.shell('input keyevent 4');
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

/**
 * Pair a device from HERE rather than through the screen, and answer its
 * session credential.
 *
 * The bundle case needs a paired session to read the two routes with, and
 * the screen it would otherwise drive is already proved by the case
 * above. `keyThumbprint` is the device IDENTITY: a fresh string enrols a
 * new device row, which is what this is.
 *
 * The same three lines appear in `tests/compact-shell-origin.spec.ts` and
 * `tests/offhost-helpers.ts`; they are inlined rather than shared because
 * this config has no fixtures file and importing one spec directory's
 * helpers into another's would tie two suites together for a POST.
 */
async function pairOverWire(
  harness: HarnessApp,
  endpoint: string,
  invite: PairingInvite,
  deviceKey: string,
): Promise<string> {
  const payload = JSON.parse(
    Buffer.from(fragmentOf(invite).slice('#pair='.length), 'base64url').toString('utf8'),
  ) as { token: string };
  const redeemed = await fetch(endpoint + '/auth/pair', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      token: payload.token,
      keyThumbprint: deviceKey,
      label: 'Emulator bundle reader',
      platform: 'playwright',
    }),
  });
  expect(redeemed.ok, '/auth/pair must answer a redemption naming a live link').toBe(true);
  const grant = (await redeemed.json()) as { credential: string; pairingId: string };
  await harness.rpc('ConfirmDevicePairing', grant.pairingId);
  return grant.credential;
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
  //
  // Every case starts from a phone that has never paired, on a process
  // that just started: the data is cleared and the app relaunched HERE,
  // so a case that failed with the credential prompt still up (which
  // pauses the WebView, and its timers with it) cannot strand the next
  // one at "Waiting for confirmation". The permission is granted again
  // after the clear, because the prompt it would otherwise raise is a
  // dialog no assertion here is about.
  page: async ({ device }, use) => {
    if (HUMAN_LOCK) await awaitOwnerUnlock(device);
    await device.shell(`pm clear ${SHELL_PACKAGE}`);
    await device.shell(`pm grant ${SHELL_PACKAGE} android.permission.POST_NOTIFICATIONS`);
    await device.shell(`am start -n ${SHELL_ACTIVITY}`);
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
  await passCredentialPrompt(device, lock);

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
  await pressBack(device);
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

// ---------------------------------------------------------------------
// The update channel, on the one machine where it is real.
// ---------------------------------------------------------------------
//
// Everything the bundle store DECIDES is proved on the JVM
// (`BundleStoreTest`), and the transport half is proved in a real browser
// (`tests/compact-shell-origin.spec.ts`). What is left, and what this
// case is for, is the part that is only true on a device: that the
// plugin registers under the name the shell calls, that a staged
// directory actually becomes the thing the WebView serves after a cold
// start, and that the shell's own launch clears the health flag before
// the 30-second watchdog rolls it back.
//
// The bundle staged here is the HARNESS BACKEND'S. It is a different
// build from the APK's (the harness build carries the UI trace and
// oracle flags the APK build does not, so its content id differs), and
// the test checks that difference rather than assuming it, so this is a
// genuine swap onto a bundle this phone did not ship with, which is the
// case that has to work.
test('the shell stages a bundle, boots on it, and refuses a damaged one', async ({
  device,
  harness,
  page,
}) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'shell-bundle',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title: 'Bundle thread',
            turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }],
          },
        ],
      },
    ],
  });
  const endpoint = `http://127.0.0.1:${harness.bootstrap.port}`;

  // The APP pairs through its own screen, the same door the case above
  // documents. It has to be genuinely paired: the health check runs from
  // the paired boot path (`native/boot.ts`), and an unpaired shell would
  // sit on the first-run screen until the watchdog rolled the bundle
  // back.
  const appInvite = await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full');
  await page.goto(SHELL_ORIGIN + '/' + fragmentOf(appInvite));
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
  await page.getByLabel('Device name').fill('Emulator shell');
  await page.getByRole('button', { name: 'Pair' }).click();
  const shown = page.getByLabel('Verification number');
  await expect(shown).toBeVisible();
  await confirmOnHost(harness, ((await shown.textContent()) ?? '').trim());
  await expect(page.getByTestId('app-lock')).toBeVisible({ timeout: PAIRED_MOUNT_MS });

  // The bytes are ALSO fetched from here, with a second, wire-paired
  // device: the refusals below need an archive and a manifest the test
  // can damage, and `stage` does not care who fetched them.
  const deviceKey = 'e2e-emulator-bundle-reader';
  const credential = await pairOverWire(
    harness,
    endpoint,
    await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full'),
    deviceKey,
  );
  const headers = { 'X-AO-Session': credential, 'X-AO-Device-Key': deviceKey };
  const manifestRes = await fetch(endpoint + '/bundle/manifest.json', { headers });
  expect(manifestRes.ok, 'a paired session must be able to read the manifest').toBe(true);
  const manifest = (await manifestRes.json()) as {
    id: string;
    files: Array<{ path: string; sha256: string; size: number }>;
  };
  const archiveRes = await fetch(endpoint + '/bundle/archive.zip', { headers });
  expect(archiveRes.ok, 'a paired session must be able to read the archive').toBe(true);
  const archive = Buffer.from(await archiveRes.arrayBuffer());
  expect(manifest.id).toMatch(/^[0-9a-f]{64}$/);
  expect(archive.length).toBeGreaterThan(0);

  /** One `stage` call in the page, answering the plugin's rejection message. */
  const stage = (id: string, m: unknown, base64: string): Promise<string> =>
    page.evaluate(
      async (call) => {
        const plugins = (window as Window & {
          Capacitor?: { Plugins?: { Bundle?: { stage(o: unknown): Promise<void> } } };
        }).Capacitor?.Plugins;
        if (!plugins?.Bundle) return 'the Bundle plugin did not register';
        try {
          await plugins.Bundle.stage({
            id: call.id,
            manifest: call.manifest,
            archiveBase64: call.base64,
          });
          return '';
        } catch (err) {
          return err instanceof Error ? err.message : String(err);
        }
      },
      { id, manifest: m, base64 },
    );

  const readState = (): Promise<{
    current: string;
    next: string;
    pendingHealth: string;
    lastKnownGood: string;
    rolledBack: string[];
    versionCode: number;
  }> =>
    page.evaluate(async () => {
      const plugins = (window as Window & {
        Capacitor?: { Plugins?: { Bundle?: { state(): Promise<never> } } };
      }).Capacitor?.Plugins;
      return await plugins!.Bundle!.state();
    });

  // --- The shell's own sync stages it first ----------------------------
  // The app paired, its backend's hello named a bundle that is not the
  // APK's, and `native/bundleSync.ts` downloaded it over the paired
  // session and staged it — behind the lock, without being asked. Waiting
  // for that proves the download path end to end, and it is what makes
  // the state below deterministic: the refusals run against a store that
  // has settled rather than racing the sync.
  expect(
    await page.evaluate(async () => (await fetch('/bundle-id.txt')).text()),
    'the APK must ship a different bundle than the harness serves, or the swap proves nothing',
  ).not.toBe(manifest.id);
  await expect
    .poll(async () => (await readState()).next, {
      message: 'the shell must stage the bundle its backend serves',
      timeout: BUNDLE_SYNC_MS,
    })
    .toBe(manifest.id);
  const before = await readState();
  expect(before.current, 'a phone that has never updated runs its own assets').toBe('');
  expect(before.versionCode, 'the plugin must be able to say what this APK is').toBeGreaterThan(0);

  // --- A damaged archive is refused ------------------------------------
  // Truncated: the zip's central directory is at the END, so half an
  // archive is not a zip at all. The plugin must say so and change
  // nothing.
  const truncated = archive.subarray(0, Math.floor(archive.length / 2));
  expect(await stage(manifest.id, manifest, truncated.toString('base64')))
    .not.toBe('');
  expect((await readState()).next, 'a refused stage must leave the state alone').toBe(manifest.id);

  // --- An intact archive with a lying manifest is refused --------------
  // The other half of the verification, and the one that matters: the
  // bytes arrive whole and the digest does not match what was promised.
  // The message names the file, because "the update failed" is not
  // something anybody can act on.
  const lying = {
    ...manifest,
    files: manifest.files.map((f, i) => (i === 0 ? { ...f, sha256: 'f'.repeat(64) } : f)),
  };
  expect(await stage(manifest.id, lying, archive.toString('base64')))
    .toContain(manifest.files[0].path);
  expect((await readState()).next).toBe(manifest.id);

  // --- The real one is accepted from here too --------------------------
  // The same id the sync staged, staged again from the page: `stage`
  // replaces the directory and re-records `next`, so what is on disk is
  // what THIS call verified.
  expect(await stage(manifest.id, manifest, archive.toString('base64'))).toBe('');
  const staged = await readState();
  expect(staged.next, 'a verified bundle waits for the next cold start').toBe(manifest.id);
  expect(staged.current, 'nothing swaps under a running app').toBe('');

  // --- The cold start adopts it ----------------------------------------
  // Force-stop rather than a back press: the swap happens in
  // `MainActivity.onCreate` before `super.onCreate`, so it needs a
  // process that is actually starting.
  await device.shell(`am force-stop ${SHELL_PACKAGE}`);
  await device.shell(`am start -n ${SHELL_ACTIVITY}`);
  const relaunched = await (await device.webView({ pkg: SHELL_PACKAGE }, { timeout: WEBVIEW_MS }))
    .page();

  // The origin is unchanged: a bundle swap changes which FILES the
  // WebView is served, never the origin it is served under — which is
  // what keeps the backend's CORS answer and the stored session valid
  // across an update.
  expect(await relaunched.evaluate(() => window.location.origin)).toBe(SHELL_ORIGIN);

  const after = await relaunched.evaluate(async () => {
    const plugins = (window as Window & {
      Capacitor?: { Plugins?: { Bundle?: { state(): Promise<never> } } };
    }).Capacitor?.Plugins;
    return (await plugins!.Bundle!.state()) as unknown as {
      current: string;
      next: string;
      pendingHealth: string;
      lastKnownGood: string;
    };
  });
  expect(after.current, 'the staged bundle is what this launch is serving').toBe(manifest.id);
  expect(after.next).toBe('');

  // --- And it proves itself before the watchdog fires -------------------
  // `pendingHealth` clears when the shell calls `ready()`, which it does
  // once the app has mounted and its boot has run to the end. The 30s
  // watchdog is the other outcome, so this poll is bounded well inside
  // it: a rollback here would mean the swap never booted.
  await expect
    .poll(
      async () =>
        (
          await relaunched.evaluate(async () => {
            const plugins = (window as Window & {
              Capacitor?: { Plugins?: { Bundle?: { state(): Promise<never> } } };
            }).Capacitor?.Plugins;
            return (await plugins!.Bundle!.state()) as unknown as { pendingHealth: string };
          })
        ).pendingHealth,
      {
        message: 'the shell must confirm this launch healthy before the watchdog rolls it back',
        timeout: 25_000,
      },
    )
    .toBe('');
});

// ---------------------------------------------------------------------
// A notification tap, on the one machine where the intent is real.
// ---------------------------------------------------------------------
//
// WHAT ONLY THIS CAN ANSWER. `TrayNotifierTest` proves the decision (post
// or drop, cancel or build) on the JVM. `tests/push.spec.ts` proves what
// the backend composes and who it is sent to. `native/push.test.ts`
// proves the web seam's handling of a tap once it arrives. What none of
// them can reach is the seam BETWEEN them: that a launch intent carrying
// the extras `AndroidTray` writes is read by `PushPlugin.load()`, held
// until the page asks with `takePendingTap`, and routed by
// `applyNotificationActivated` to the right thread — through the app
// LOCK, which is up when the app comes back from a tap on a dead process.
//
// The tap is delivered with `am start` and the extras, not by posting a
// real notification and clicking it: the payload of a tap is exactly its
// extras, and driving the system tray adds a UI surface no assertion here
// is about. The extras are the contract, so the extras are what is sent.
//
// The cold-start door is the one under test, so the process is force-stopped
// first. `handleOnNewIntent` is the other door and is deliberately not
// exercised here: it is the easy half, and a running app would not have
// been woken by a push in the first place.
test('a notification tap opens its thread after the lock is answered', async ({
  device,
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'shell-push-tap',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title: 'Ignored thread',
            turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }],
          },
          {
            title: 'Tapped thread',
            turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'hi' }] }],
          },
        ],
      },
    ],
  });
  const [, tappedThreadId] = seed.projects[0].threadIds;
  expect(tappedThreadId, 'the fixture must seed a second, distinct thread').toBeTruthy();

  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full');
  await page.goto(SHELL_ORIGIN + '/' + fragmentOf(invite));
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
  await page.getByLabel('Device name').fill('Emulator shell');
  await page.getByRole('button', { name: 'Pair' }).click();
  const shown = page.getByLabel('Verification number');
  await expect(shown).toBeVisible();
  await confirmOnHost(harness, ((await shown.textContent()) ?? '').trim());
  await expect(page.getByTestId('app-lock')).toBeVisible({ timeout: PAIRED_MOUNT_MS });

  // The target document is exactly what `push.MessageFor` puts under its
  // `target` key and what `AndroidTray` copies onto the intent: one JSON
  // string, opaque to Java, parsed by the page's own
  // `parseNotificationTarget`.
  const target = JSON.stringify({ kind: 'thread', threadId: tappedThreadId });
  const tag = `thread:${tappedThreadId}`;

  // A dead process is the case: a phone woken by a push was not running.
  await device.shell(`am force-stop ${SHELL_PACKAGE}`);
  await device.shell(
    `am start -n ${SHELL_ACTIVITY} --es ${EXTRA_TARGET} '${target}' --es ${EXTRA_ID} '${tag}'`,
  );

  const tapped = await (await device.webView({ pkg: SHELL_PACKAGE }, { timeout: WEBVIEW_MS })).page();

  // The lock is in front of it, and the tap survives being held there:
  // the activation queue waits for hydration and the app under the lock
  // is mounted and inert, not absent.
  const lock = tapped.getByTestId('app-lock');
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
  await passCredentialPrompt(device, lock);

  // And it lands on the thread the extras named, rather than on the list.
  await expect(tapped.locator('html')).toHaveAttribute('data-compact-screen', 'thread', {
    timeout: PAIRED_MOUNT_MS,
  });
  await expect(tapped.getByTestId('chat-header-title')).toHaveText('Tapped thread');
});

// ---------------------------------------------------------------------
// The last hop: a real message through Google, by hand.
// ---------------------------------------------------------------------
//
// Everything up to Google is machine-testable and tested: what the
// backend composes (`tests/push.spec.ts`, against the harness recorder),
// what the tray does with a message (`TrayNotifierTest`), and what a tap
// does (the case above, extras delivered with `am start`). What none of
// them can prove is that a REAL Firebase project accepts the credential,
// that Google delivers the data message, and that `PushService` on a real
// phone renders it into the tray. That needs an APK built with
// `google-services.json` (mobile/AGENTS.md § google-services.json) and
// the matching service-account key — machine-specific facts, so this case
// skips itself unless `AO_ANDROID_PUSH_CREDENTIAL` names the key file.
// It is a manual gate in the same sense as `make provider-smoke`: run it
// when the Firebase project or the push path changes.
//
// The app is backgrounded with HOME, not force-stopped: Android bars a
// force-stopped app from receiving FCM until the user relaunches it, so
// a force-stop here would be testing Google's refusal, not our delivery.
test('a real push crosses Google and lands in the tray', async ({ device, harness, page }) => {
  const credentialPath = process.env.AO_ANDROID_PUSH_CREDENTIAL ?? '';
  test.skip(
    credentialPath === '',
    'needs AO_ANDROID_PUSH_CREDENTIAL (a Firebase service-account key) and an APK built with google-services.json',
  );
  const credentialJSON = fs.readFileSync(credentialPath, 'utf8');
  await harness.rpc('SetPushSenderCredential', credentialJSON);

  // A thread on the mock provider, ready to complete a turn on demand —
  // the same staging `tests/push.spec.ts` uses against the recorder.
  await harness.rpc('HarnessSetScenario', {
    scenario: {
      version: 1,
      name: 'real-push',
      provider: 'claude',
      turns: [
        { label: 'turn-1', steps: [emit([...textLines('msg-1', 'Answer 1.'), RESULT_LINE])] },
      ],
      afterTurns: 'silent',
    },
  });
  const threadId = await seedAgentThread(harness, 'shell-push-real', 'Real push thread');
  await startMock(harness, threadId);

  // Paired through the shell's own screen, so the phone's REAL FCM token
  // is what registers: `PushPlugin.getToken()` only answers on a build
  // whose FirebaseApp initialised.
  const invite = await harness.rpc<PairingInvite>('MintDevicePairing', 'phone', 'full');
  await page.goto(SHELL_ORIGIN + '/' + fragmentOf(invite));
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Pair this device' })).toBeVisible();
  await page.getByLabel('Device name').fill('Real push phone');
  await page.getByRole('button', { name: 'Pair' }).click();
  const shown = page.getByLabel('Verification number');
  await expect(shown).toBeVisible();
  await confirmOnHost(harness, ((await shown.textContent()) ?? '').trim());
  const lock = page.getByTestId('app-lock');
  await expect(lock).toBeVisible({ timeout: PAIRED_MOUNT_MS });
  await passCredentialPrompt(device, lock);

  // The registration is the boot's to make; the backend's status line is
  // where it lands, and where "my phone stopped buzzing" gets answered.
  await expect
    .poll(
      async () =>
        ((await harness.rpc('GetPushSenderStatus')) as { registeredDevices: number })
          .registeredDevices,
      { message: 'the shell must register its FCM token after pairing', timeout: 60_000 },
    )
    .toBeGreaterThan(0);

  // Background the app, complete a turn, and the wake must cross Google.
  await device.shell('input keyevent KEYCODE_HOME');
  await harness.rpc('SendMessage', threadId, 'real push question', null);
  await expect
    .poll(
      async () => (await device.shell('dumpsys notification --noredact')).toString(),
      {
        message: 'the tray must show the pushed notification, tagged with its thread',
        intervals: [2_000],
        timeout: 90_000,
      },
    )
    .toContain(`thread:${threadId}`);

  // The phrase is one of `notify.KindPhrase`'s six, never thread content:
  // §9's redaction rule, observed on the far side of Google.
  const dump = (await device.shell('dumpsys notification --noredact')).toString();
  expect(dump).toContain('Turn complete');
  expect(dump).not.toContain('Real push thread');
});
