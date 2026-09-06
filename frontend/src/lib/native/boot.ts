import { pairingEndpoint } from './networkTrust';
// What the shell does before anything mounts.
//
// `main.ts` calls `prepareNativeShell()` first and reads the answer to
// decide which of three things to put on the screen: the first-run
// scanner, the lock, or the app. Everything platform-shaped lives here so
// `main.ts` keeps one shape for every client.
//
// Endpoints are read before the first fetch. A legacy HOME slot is restored
// when present; every new pairing has its own computer ID and descriptor.
// No pairing may repoint another computer's live transport.
//
// With no saved computers the shell shows first run. Removing the first
// computer does not hide or erase any of the remaining connections.

import { applyNotificationActivated } from '../stores/eventsNotification';
import { parseNotificationTarget } from '../stores/notificationActivationQueue';
import { attachedBackends, duplicateLegacyHomeBackend, onBackendsChanged, restoreHomeBackend, setBackendSource, syncAttachedBackends } from '../transport/backends';
import { initializeSelectedBackend } from '../stores/selectedBackend.svelte';
import { onBeforeBackendDetach } from '../transport/detachSteps';
import type { PairingPayload } from '../transport/deviceSession';
import { setHomeEndpoint, storedBackendEndpoint } from '../transport/homeEndpoint';
import { storedBackendDescriptors } from '../transport/manifestBackends';
import type { AppLock } from './lock';
import { isNativeShell } from './platform';
import { installNativeKeybindings } from './keybindings';

export interface ShellBoot {
  /** False in every browser build; nothing below it applies. */
  shell: boolean;
  /** True when at least one computer is saved on this frontend. */
  paired: boolean;
}

function initializeComputerSelection(): void {
  const computers = attachedBackends();
  const canonical = duplicateLegacyHomeBackend();
  // Source sync attaches entries one by one. Do not temporarily choose a
  // different computer just because its descriptor precedes the repaired one.
  if (canonical && !computers.some((computer) => computer.id === canonical)) return;
  initializeSelectedBackend(computers, canonical ?? undefined);
}

// A pre-identity legacy slot may converge only after bootstrap. Update an
// unresolved HOME choice then; explicit computer selections stay untouched.
onBackendsChanged(() => { if (isNativeShell()) initializeComputerSelection(); });

/**
 * Address the backends this device has paired with, and answer what
 * `main.ts` should show.
 *
 * The attached list comes from client-local storage rather than from the
 * home manifest, through the one injectable `BackendSource` that exists
 * for exactly this (`transport/backends.ts`): a desktop reads the list
 * its local process proxies, a phone reads the machines it paired with
 * itself, and nothing below that seam branches on which client it is.
 */
export function prepareNativeShell(): ShellBoot {
  if (!isNativeShell()) return { shell: false, paired: false };
  installNativeKeybindings();

  // Installed before the endpoint is set, so a manifest that resolves
  // early cannot publish the desktop-shaped list over it.
  setBackendSource(storedBackendDescriptors);

  const home = storedBackendEndpoint();
  if (home !== '') {
    setHomeEndpoint(home);
    restoreHomeBackend();
  }
  syncAttachedBackends();
  const computers = attachedBackends();
  initializeComputerSelection();
  return { shell: true, paired: computers.length > 0 };
}

/**
 * Validate the address from a scan or app link before showing pairing.
 * PairingScreen stores it in the selected computer's credential slot when
 * the user starts pairing; scanning alone changes no existing connection.
 *
 * Answers a sentence for a person when the payload names nowhere a
 * credential could be presented, else `''`.
 */
export function adoptPairingEndpoint(payload: PairingPayload): string {
  try {
    // Validate without repointing an existing connection. PairingScreen
    // stores the endpoint in the computer's own slot when pairing begins.
    pairingEndpoint(payload);
  } catch (error) {
    if (error instanceof Error && !(error instanceof TypeError)) return error.message;
    return 'That pairing link does not say where the app is. Ask for a new one.';
  }
  return '';
}

/**
 * Everything a paired shell installs once the endpoint is known: the
 * lifecycle subscriptions, the app lock, and the update channel. Split
 * from the function above because the first-run screen needs the
 * endpoint decision and not these — a device that has not paired has
 * nothing to lock, no lease to state, and no backend to take a bundle
 * from.
 */
export async function installNativeShell(
  onLockChange: (locked: boolean) => void,
): Promise<AppLock> {
  const [{ installNativeLifecycle }, { installAppLock }, bundles, push, presenter] =
    await Promise.all([
      import('./lifecycle'),
      import('./lock'),
      import('./bundleSync'),
      import('./push'),
      import('../stores/pushPresenter.svelte'),
    ]);
  await installNativeLifecycle();
  // None of these is awaited, and none may be. The update channel is
  // background work behind a lock screen that is already up: a download
  // that took a minute must not hold the gate. The lock below is what
  // this function's caller is waiting for.
  //
  // The health confirmation goes here because reaching here is the
  // check: the app has mounted and this bundle's boot ran to its end
  // (./bundleSync.ts, `reportBundleHealthy`).
  void bundles.startBundleSync();
  void bundles.confirmLaunchHealthy();
  // Push, in three pieces that are deliberately independent: telling the
  // backends where to reach this phone, presenting what arrives over the
  // socket while the app is in the background, and following a tap. Each
  // is useful without the others — a phone with a denied permission still
  // presents nothing and follows nothing, and one whose build carries no
  // push configuration still does everything else (§ Push in
  // mobile/AGENTS.md).
  void presenter.startPushPresenter();
  void push.watchPushTaps((target) => {
    void routeNotificationTap(target);
  });
  // The step that has to run while a connection is still open. Installed
  // once, on the shell only, and the two removal doors call it
  // (transport/detachSteps.ts).
  onBeforeBackendDetach((backend) => {
    void push.unregisterPushFrom(backend);
  });
  // Registration waits for the GATE. Its first act is the platform's
  // notification-permission prompt, and a prompt raised while the lock's
  // own prompt is up is two system dialogs on one screen: the credential
  // the person types lands on whichever is on top (2026-09-03, the
  // emulator smoke, where every unlock failed that way). Nothing else
  // here raises a dialog, so nothing else waits.
  const startPush = onceUnlocked(() => void push.startPushRegistration());
  // The lock is handed back rather than kept here: the caller owns the
  // screen the gate is in front of, so it owns the retry button too.
  const lock = await installAppLock({
    onChange: (locked) => {
      onLockChange(locked);
      startPush(locked);
    },
  });
  // A lock that never publishes (an APK without the biometric plugin) is
  // permanently open, and nothing waits on a gate that is not there.
  startPush(lock.locked());
  return lock;
}

/**
 * Run `start` once, the first time the lock reports open.
 *
 * The lock publishes every change, so a cover going up and down on a
 * trip to another app would otherwise start the work again; the
 * pause-and-resume the credential prompt itself causes is one such trip.
 */
export function onceUnlocked(start: () => void): (locked: boolean) => void {
  let started = false;
  return (locked) => {
    if (locked || started) return;
    started = true;
    start();
  };
}

/**
 * A notification tap, onto the route the desktop already uses.
 *
 * Nothing new is built here on purpose. `applyNotificationActivated` is
 * the same door the Windows launcher's clicks come through, and the
 * activation queue behind it already waits for hydration — which is what
 * makes a tap that cold-launched the app land on the right thread once
 * the registry is loaded, and what makes a tap through the lock screen
 * land the moment the person unlocks (the app under the lock is mounted
 * and inert, not absent).
 */
async function routeNotificationTap(value: unknown): Promise<void> {
  const target = parseNotificationTarget(value);
  if (target === null) {
    console.warn('push: a tap named a route this build could not read', value);
    return;
  }
  applyNotificationActivated(target);
}
