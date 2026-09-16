import { expect, type AndroidDevice, type AndroidSelector } from '@playwright/test';

const TIMEOUT = 60_000;

export async function waitForNode(device: AndroidDevice, selector: AndroidSelector, present = true): Promise<void> {
  // Native selector queries can report absence before the WebView's next
  // accessibility update (including a driver null-node exception). Retry reads
  // only; never retry a tap or a send.
  await expect.poll(async () => {
    try { await device.info(selector); return true; }
    catch { return false; }
  }, { message: `Android node ${JSON.stringify(selector)} must be ${present ? 'present' : 'gone'}`, timeout: TIMEOUT }).toBe(present);
}

export async function waitForStableNode(device: AndroidDevice, selector: AndroidSelector): Promise<void> {
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

export async function tapNode(device: AndroidDevice, selector: AndroidSelector): Promise<void> {
  await waitForStableNode(device, selector);
  await device.tap(selector, { duration: 100, timeout: TIMEOUT });
}

export async function screenLocked(device: AndroidDevice, locked: boolean): Promise<void> {
  await expect.poll(async () => (await device.shell('dumpsys activity activities')).toString(), {
    message: `Android keyguard must be ${locked ? 'locked' : 'unlocked'}`,
  }).toContain(`mKeyguardShowing=${locked}`);
}

export async function unlockScreen(device: AndroidDevice): Promise<void> {
  await device.shell('input keyevent KEYCODE_WAKEUP');
  await expect.poll(async () => (await device.shell('dumpsys power')).toString())
    .toContain('mWakefulness=Awake');
  await waitForStableNode(device, { res: 'com.android.systemui:id/legacy_window_root' });
  await device.shell('input keyevent KEYCODE_MENU');
  await waitForNode(device, { res: 'com.android.systemui:id/pinEntry' });
  // System keyguard does not accept WebView/IME text injection. Use its real
  // numeric buttons for the PIN the runner provisioned on this emulator.
  for (const digit of '1234') await tapNode(device, { res: `com.android.systemui:id/key${digit}` });
  await tapNode(device, { res: 'com.android.systemui:id/key_enter' });
  await screenLocked(device, false);
}

export async function prepareEmulatorScreen(device: AndroidDevice): Promise<void> {
  expect((await device.shell('getprop ro.kernel.qemu')).toString().trim()).toBe('1');
  // Exercise the runner's PIN once before launching the app. Installing a PIN
  // can raise the keyguard after a prior dismiss request has returned.
  await device.shell('input keyevent KEYCODE_SLEEP');
  await expect.poll(async () => (await device.shell('dumpsys power')).toString())
    .toMatch(/mWakefulness=(Asleep|Dozing)/);
  await screenLocked(device, true);
  await unlockScreen(device);
}
