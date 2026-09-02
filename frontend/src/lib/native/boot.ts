// What the shell does before anything mounts.
//
// `main.ts` calls `prepareNativeShell()` first and reads the answer to
// decide which of three things to put on the screen: the first-run
// scanner, the lock, or the app. Everything platform-shaped lives here so
// `main.ts` keeps one shape for every client.
//
// **The endpoints are read before the first fetch, not after.** A phone
// is the one client whose home backend is not the origin that served it,
// so `transport/homeEndpoint.setHomeEndpoint` has to be called before
// anything can address `/bootstrap.json` — which on this client means
// before `App.svelte` mounts, since mounting it issues the whole boot
// fan-out.
//
// **A shell with no stored endpoint has never paired**, which is the
// first-run screen and not an error: the answer to "where is my backend"
// is the QR code on the owner's own desktop.

import { setBackendSource, syncAttachedBackends } from '../transport/backends';
import { setHomeEndpoint, storedBackendEndpoint } from '../transport/homeEndpoint';
import { storedBackendDescriptors } from '../transport/manifestBackends';
import type { AppLock } from './lock';
import { isNativeShell } from './platform';

export interface ShellBoot {
  /** False in every browser build; nothing below it applies. */
  shell: boolean;
  /** True when this launch already knows where its home backend is. */
  paired: boolean;
}

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

  // Installed before the endpoint is set, so a manifest that resolves
  // early cannot publish the desktop-shaped list over it.
  setBackendSource(storedBackendDescriptors);

  const home = storedBackendEndpoint();
  if (home === '') return { shell: true, paired: false };
  setHomeEndpoint(home);
  syncAttachedBackends();
  return { shell: true, paired: true };
}

/**
 * Everything a paired shell installs once the endpoint is known: the
 * lifecycle subscriptions and the app lock. Split from the function above
 * because the first-run screen needs the endpoint decision and not these
 * — a device that has not paired has nothing to lock and no lease to
 * state.
 */
export async function installNativeShell(
  onLockChange: (locked: boolean) => void,
): Promise<AppLock> {
  const [{ installNativeLifecycle }, { installAppLock }] = await Promise.all([
    import('./lifecycle'),
    import('./lock'),
  ]);
  await installNativeLifecycle();
  // The lock is handed back rather than kept here: the caller owns the
  // screen the gate is in front of, so it owns the retry button too.
  return await installAppLock({ onChange: onLockChange });
}
