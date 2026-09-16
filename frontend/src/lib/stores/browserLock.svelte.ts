import { BeginPasskeyStepUp, VerifyBrowserUnlock } from './bindings';
import { HOME_BACKEND, withBackendTarget } from '../transport/backends';
import { answerChallenge, PasskeyAbandonedError, passkeysUsable } from '../transport/passkey';
import { createBrowserLock, BROWSER_LOCK_KEY, type BrowserLockState } from '../utils/browserLock';

export const browserLock = $state<BrowserLockState>({ enabled: false, locked: false, busy: false, error: '' });
let controller: ReturnType<typeof createBrowserLock> | null = null;

/** Installed before the application mounts so saved locks cannot flash content. */
export function installBrowserLock(changed: (locked: boolean) => void): () => void {
  if (controller) throw new Error('Browser lock is already installed');
  controller = createBrowserLock({
    storage: {
      getItem: (key) => localStorage.getItem(key),
      setItem: (key, value) => localStorage.setItem(key, value),
      removeItem: (key) => localStorage.removeItem(key),
    },
    verify: async () => {
      if (!passkeysUsable()) {
        throw new Error('Use HTTPS at the configured passkey domain and a registered passkey.');
      }
      const challenge = await withBackendTarget(HOME_BACKEND, () => BeginPasskeyStepUp());
      let response: string;
      try {
        response = await answerChallenge(challenge, 'get');
      } catch (error) {
        if (error instanceof PasskeyAbandonedError) throw new Error('The prompt was canceled or declined. Try again when you are ready.');
        throw error;
      }
      await withBackendTarget(HOME_BACKEND, () => VerifyBrowserUnlock(challenge.ceremonyId, JSON.parse(response)));
    },
    changed: (next) => {
      Object.assign(browserLock, next);
      changed(next.locked);
    },
  });
  const visibility = () => controller?.visibilityChanged(document.hidden);
  const pagehide = () => controller?.visibilityChanged(true);
  const pageshow = (event: PageTransitionEvent) => {
    if (event.persisted) controller?.pageRestored();
    visibility();
  };
  const storage = (event: StorageEvent) => {
    if (event.key === BROWSER_LOCK_KEY || event.key === null) controller?.preferencesChanged();
  };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pagehide', pagehide);
  window.addEventListener('pageshow', pageshow);
  window.addEventListener('storage', storage);
  visibility();
  return () => {
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pagehide', pagehide);
    window.removeEventListener('pageshow', pageshow);
    window.removeEventListener('storage', storage);
    controller?.dispose();
    controller = null;
    Object.assign(browserLock, { enabled: false, locked: false, busy: false, error: '' });
    changed(false);
  };
}

export function enableBrowserLock(): Promise<void> { return controller?.enable() ?? Promise.resolve(); }
export function disableBrowserLock(): void { controller?.disable(); }
export function unlockBrowser(): Promise<void> { return controller?.unlock() ?? Promise.resolve(); }
export function setBrowserLockLocalPage(value: boolean): void { controller?.setLocalPage(value); }
