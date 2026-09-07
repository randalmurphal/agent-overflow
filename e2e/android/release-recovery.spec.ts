// Exercise the actual signed APK using Android accessibility, never CDP or
// a debug-enabled variant. The runner verifies its signature and manifest.
// A real LAN listener and Android radio changes reach the pinned HTTP/WS
// bridge; browser route mocks cannot establish this native lifecycle contract.
import { _android, expect, test, type AndroidDevice, type AndroidSelector } from '@playwright/test';
import { launchHarness } from '../src/harness.js';
import { compareBundleVersions } from '../../frontend/src/lib/native/bundleVersion.ts';
import { execFileSync } from 'node:child_process';
import { RESULT_LINE, advance, claudeScenario, emit, seedAgentThread, startMock, textLines, waitForGate } from '../tests/agent-visibility-helpers.js';

const PACKAGE = 'dev.agentoverflow.app';
const ACTIVITY = `${PACKAGE}/.MainActivity`;
const TIMEOUT = 60_000;

// Chromium exposes ARIA names as Android accessibility text. Wait through
// UiAutomation rather than treating a connected socket as rendered progress.
const named = (text: string | RegExp): AndroidSelector => ({ text, pkg: PACKAGE });

async function waitForNode(device: AndroidDevice, selector: AndroidSelector, present = true): Promise<void> {
  // Native selector queries can report absence before the WebView's next
  // accessibility update (including a driver null-node exception). Retry reads
  // only; never retry a tap or a send.
  await expect.poll(async () => {
    try { await device.info(selector); return true; }
    catch { return false; }
  }, { message: `Android node ${JSON.stringify(selector)} must be ${present ? 'present' : 'gone'}`, timeout: TIMEOUT }).toBe(present);
}

async function waitForStableNode(device: AndroidDevice, selector: AndroidSelector): Promise<void> {
  // Native tap injects coordinates without Page locator actionability. Filling
  // a field opens the IME and moves buttons: await enabled, stable bounds first.
  let previous = '';
  await expect.poll(async () => {
    try {
      const node = await device.info(selector);
      const bounds = JSON.stringify(node.bounds);
      const ready = node.enabled && node.bounds.width > 0 && node.bounds.height > 0 && bounds === previous;
      previous = bounds;
      return ready;
    } catch { previous = ''; return false; }
  }, { message: `Android action ${JSON.stringify(selector)} must settle`, timeout: TIMEOUT, intervals: [250] }).toBe(true);
}

async function tapNode(device: AndroidDevice, selector: AndroidSelector): Promise<void> {
  await waitForStableNode(device, selector);
  await device.tap(selector);
}

async function fillNode(device: AndroidDevice, selector: AndroidSelector, value: string): Promise<void> {
  await tapNode(device, selector);
  const focused = { ...selector, focused: true };
  await waitForStableNode(device, focused);
  await device.fill(focused, value);
  await expect.poll(async () => (await device.info(focused)).text).toBe(value);
}

async function unlock(device: AndroidDevice): Promise<void> {
  await waitForNode(device, { res: 'com.android.systemui:id/lockPassword' });
  await tapNode(device, { res: 'com.android.systemui:id/lockPassword' });
  await device.fill({ res: 'com.android.systemui:id/lockPassword', focused: true }, '1234');
  await device.shell('input keyevent KEYCODE_ENTER');
  await waitForNode(device, { res: 'com.android.systemui:id/lockPassword' }, false);
}

async function background(device: AndroidDevice): Promise<void> {
  await device.shell('input keyevent KEYCODE_HOME');
  await expect.poll(async () => {
    const dump = (await device.shell('dumpsys window')).toString();
    const focus = /mCurrentFocus=(.*)/.exec(dump)?.[1]?.trim() ?? '';
    return focus !== '' && !focus.includes(PACKAGE);
  }, { message: 'Android must background the real app before the provider advances' }).toBe(true);
}

async function screenLocked(device: AndroidDevice, locked: boolean): Promise<void> {
  await expect.poll(async () => (await device.shell('dumpsys activity activities')).toString(), {
    message: `Android keyguard must be ${locked ? 'locked' : 'unlocked'}`,
  }).toContain(`mKeyguardShowing=${locked}`);
}

async function unlockScreen(device: AndroidDevice): Promise<void> {
  await device.shell('input keyevent KEYCODE_WAKEUP');
  await device.shell('input keyevent KEYCODE_MENU');
  await waitForNode(device, { res: 'com.android.systemui:id/pinEntry' });
  // System keyguard does not accept WebView/IME text injection. Use its real
  // numeric buttons for the PIN the runner provisioned on this emulator.
  for (const digit of '1234') await tapNode(device, { res: `com.android.systemui:id/key${digit}` });
  await tapNode(device, { res: 'com.android.systemui:id/key_enter' });
  await screenLocked(device, false);
}

test('the signed release recovers live and completed turns across Android suspension and LAN loss', async ({}, testInfo) => {
  const devices = await _android.devices();
  const device = devices.find((candidate) => candidate.serial() === process.env.AO_ANDROID_SERIAL);
  expect(device, 'the runner must select the disposable emulator explicitly').toBeDefined();
  const phone = device!;
  expect((await phone.shell('getprop ro.kernel.qemu')).toString().trim()).toBe('1');
  const harness = await launchHarness();
  const wifi = (await phone.shell('settings get global wifi_on')).toString().trim() === '1';
  const data = (await phone.shell('settings get global mobile_data')).toString().trim() === '1';
  try {
    const packagedVersion = JSON.parse(execFileSync('unzip', ['-p', process.env.AO_ANDROID_RELEASE_APK!, 'assets/public/bundle-release.json'], { encoding: 'utf8' })).version;
    const release = await (await fetch(new URL('/bundle-release.json', harness.url))).json();
    expect(compareBundleVersions(release.version, packagedVersion), 'adoption requires a strictly newer fixture release').toBe(1);
    await harness.rpc('SetNetworkSettings', { bindAll: true });
    const threadId = await seedAgentThread(harness, 'release-recovery', 'Release recovery');
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario('release-lifecycle', [
        emit(textLines('release-start', 'Release turn started.')),
        { waitSignal: { name: 'resume-active' } },
        emit(textLines('release-resumed', 'Release turn kept working.')),
        { waitSignal: { name: 'finish-offline' } },
        emit([...textLines('release-finished', 'Release turn finished offline.'), RESULT_LINE]),
      ]),
    });
    const invite = await harness.rpc<{ url: string; linkId: string }>('MintDevicePairing', 'phone', 'full');
    const payload = JSON.parse(Buffer.from(invite.url.split('#pair=')[1], 'base64url').toString('utf8')) as { endpoint: string; certFingerprint: string };
    expect(invite.url).not.toContain('127.0.0.1');
    expect(payload.certFingerprint).toMatch(/^sha256:[0-9a-f]{64}$/);

    await phone.shell(`pm clear ${PACKAGE}`);
    await phone.shell(`pm grant ${PACKAGE} android.permission.POST_NOTIFICATIONS`);
    await phone.shell(`am start -n ${ACTIVITY}`);
    await waitForNode(phone, named('Use a link'));
    await tapNode(phone, named('Use a link'));
    await waitForNode(phone, { clazz: 'android.widget.EditText', pkg: PACKAGE });
    await fillNode(phone, { clazz: 'android.widget.EditText', pkg: PACKAGE }, invite.url);
    await tapNode(phone, named('Connect'));
    await waitForNode(phone, named('Pair this device'));
    await fillNode(phone, { clazz: 'android.widget.EditText', pkg: PACKAGE }, 'Release emulator');
    await tapNode(phone, named('Pair'));
    await expect.poll(async () => {
      const overview = await harness.rpc<{ pendingPairings?: Array<{ linkId: string; redeemed: boolean }> }>('GetAccessOverview');
      return overview.pendingPairings?.some((entry) => entry.linkId === invite.linkId && entry.redeemed) ?? false;
    }).toBe(true);
    // The WebView smoke compares the displayed digits. Android exposes the
    // paragraph's ARIA label here, so wait for the actual confirmation state.
    await waitForNode(phone, named('Waiting for confirmation'));
    await harness.rpc('ConfirmDevicePairing', invite.linkId);
    await unlock(phone);
    await waitForNode(phone, named('Release recovery'));

    // The APK can be an existing candidate. Require the real update-ready UI
    // before a cold start so this lifecycle test executes the current host's
    // production frontend, not silently the candidate's older assets.
    const updateReady = named('A newer Agent Overflow is ready. It loads the next time the app starts.');
    try { await waitForNode(phone, updateReady); }
    catch (cause) {
      throw new Error('No update-ready notice: this case requires the newer Android bundle fixture built by make e2e-android.', { cause });
    }
    await phone.shell(`am force-stop ${PACKAGE}`);
    await phone.shell(`am start -n ${ACTIVITY}`);
    await unlock(phone);
    await waitForNode(phone, named('Release recovery'));
    await waitForNode(phone, updateReady, false);
    await tapNode(phone, named('Release recovery'));
    await waitForNode(phone, { clazz: 'android.widget.EditText', pkg: PACKAGE });
    const pid = (await phone.shell(`pidof ${PACKAGE}`)).toString().trim();
    expect(pid).toMatch(/^\d+$/);
    // Chromium forces WebView debugging on userdebug Android, even when the
    // release app passes false. Check runtime exposure only on a user OS.
    // https://chromium.googlesource.com/chromium/src/+/main/android_webview/glue/java/src/com/android/webview/chromium/SharedStatics.java
    if ((await phone.shell('getprop ro.debuggable')).toString().trim() === '0') {
      const sockets = (await phone.shell('cat /proc/net/unix')).toString();
      expect(sockets, 'the socket inventory must be readable').toMatch(/^Num\s+RefCount/m);
      expect(sockets.split(/\s+/)).not.toContain(`@webview_devtools_remote_${pid}`);
    }

    const mockId = await startMock(harness, threadId);
    const composer = { clazz: 'android.widget.EditText', pkg: PACKAGE };
    await fillNode(phone, composer, 'Run release lifecycle');
    await tapNode(phone, named('Send message'));
    await waitForGate(harness, 'resume-active');
    await waitForNode(phone, named('Release turn started.'));
    await waitForNode(phone, named('Interrupt current turn'));
    await background(phone);
    await advance(harness, mockId, 'resume-active');
    await waitForGate(harness, 'finish-offline');
    await phone.shell(`am start -n ${ACTIVITY}`);
    await waitForNode(phone, named('Release turn kept working.'));
    await waitForNode(phone, named('Interrupt current turn'));

    await phone.shell('svc wifi disable');
    await phone.shell('svc data disable');
    await waitForNode(phone, named(/.*Reconnecting.*/));
    await phone.shell('input keyevent KEYCODE_SLEEP');
    await screenLocked(phone, true);
    await advance(harness, mockId, 'finish-offline');
    await harness.waitForEvent('provider:turn_completed');
    expect((await harness.rpc<{ activeTurn?: unknown }>('GetThreadLiveState', threadId)).activeTurn).toBeFalsy();
    await unlockScreen(phone);
    await phone.shell(`am start -n ${ACTIVITY}`);
    await waitForNode(phone, named(/.*Reconnecting.*/));
    if (wifi) await phone.shell('svc wifi enable');
    if (data) await phone.shell('svc data enable');
    await waitForNode(phone, named('Release turn finished offline.'));
    await waitForNode(phone, named('Interrupt current turn'), false);
    expect((await phone.info({ clazz: 'android.widget.EditText', pkg: PACKAGE })).enabled).toBe(true);
    expect((await phone.shell(`pidof ${PACKAGE}`)).toString().trim(), 'recovery must not restart the app').toBe(pid);
    await testInfo.attach('recovered-release-screen', { body: await phone.screenshot({ path: testInfo.outputPath('release-screen.png') }), contentType: 'image/png' });
  } catch (error) {
    await testInfo.attach('failed-release-screen', { body: await phone.screenshot({ path: testInfo.outputPath('release-screen.png') }), contentType: 'image/png' }).catch(() => {});
    throw error;
  } finally {
    if (wifi) await phone.shell('svc wifi enable');
    if (data) await phone.shell('svc data enable');
    await phone.shell(`am force-stop ${PACKAGE}`);
    await harness.close();
    await phone.close();
  }
});
