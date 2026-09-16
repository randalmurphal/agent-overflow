import { DEFAULT_LOCK_WINDOW_MS, shouldLock } from './lockTiming';
import { errString } from './errors';

export const BROWSER_LOCK_KEY = 'agent-overflow:browserLock';

export interface BrowserLockState {
  enabled: boolean;
  locked: boolean;
  busy: boolean;
  error: string;
}

/** Preferences are origin-local; unlock state is never stored or shared. */
export function createBrowserLock(options: {
  storage: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>;
  verify: () => Promise<void>;
  changed: (state: BrowserLockState) => void;
  now?: () => number;
}) {
  const now = options.now ?? Date.now;
  let state: BrowserLockState = { enabled: false, locked: false, busy: false, error: '' };
  let hidden = false;
  let pausedAt: number | null = null;
  let owed = true;
  let disposed = false;
  let exempt = false;
  let generation = 0;
  let pending: Promise<void> | null = null;

  function publish(patch: Partial<BrowserLockState> = {}): void {
    state = { ...state, ...patch };
    if (!disposed) options.changed({ ...state });
  }

  function storedEnabled(): boolean {
    return options.storage.getItem(BROWSER_LOCK_KEY) !== null;
  }

  function readPreference(): void {
    try {
      const enabled = storedEnabled();
      // Another tab changing preferences cannot dismiss this tab's lock.
      if (!enabled && state.locked) return;
      if (enabled === state.enabled) return;
      generation++;
      owed = true;
      pausedAt = hidden ? now() : null;
      publish({ enabled, locked: enabled && !exempt, error: '' });
    } catch (error) {
      owed = true;
      publish({ enabled: true, locked: !exempt, error: `Could not read browser lock settings: ${errString(error)}` });
    }
  }

  readPreference();

  async function authenticate(enable: boolean): Promise<void> {
    if (pending || disposed || exempt) return pending ?? undefined;
    const attempt = generation;
    publish({ busy: true, error: '' });
    pending = (async () => {
      try {
        await options.verify();
        if (disposed || generation !== attempt) return;
        if (enable) options.storage.setItem(BROWSER_LOCK_KEY, 'enabled');
        const enabled = storedEnabled();
        owed = false;
        // A proof arriving in the background never exposes content there.
        // The next foreground event still checks the full background interval.
        publish({ enabled, locked: enabled && hidden, error: '' });
      } catch (error) {
        if (!disposed && generation === attempt) {
          publish({ error: `Could not verify your passkey: ${errString(error)}` });
        }
      }
    })();
    try {
      await pending;
    } finally {
      pending = null;
      if (!disposed) publish({ busy: false });
    }
  }

  return {
    snapshot: () => ({ ...state }),
    enable: () => authenticate(true),
    unlock: () => state.locked ? authenticate(false) : Promise.resolve(),
    disable(): void {
      if (state.locked || state.busy || disposed) return;
      try {
        options.storage.removeItem(BROWSER_LOCK_KEY);
        generation++;
        owed = true;
        pausedAt = null;
        publish({ enabled: false, locked: false, error: '' });
      } catch (error) {
        publish({ error: `Could not save browser lock settings: ${errString(error)}` });
      }
    },
    visibilityChanged(nextHidden: boolean): void {
      if (disposed || nextHidden === hidden) return;
      hidden = nextHidden;
      if (!state.enabled || exempt) return;
      if (hidden) {
        pausedAt = now();
        publish({ locked: true });
      } else {
        owed ||= shouldLock(pausedAt, now(), DEFAULT_LOCK_WINDOW_MS);
        publish({ locked: owed });
      }
    },
    // Page restoration is a new entry even when the browser preserves JS state.
    pageRestored(): void {
      if (disposed || !state.enabled || exempt) return;
      generation++;
      owed = true;
      publish({ locked: true, error: '' });
    },
    preferencesChanged: readPreference,
    setLocalPage(value: boolean): void {
      if (exempt === value || disposed) return;
      exempt = value;
      generation++;
      owed = true;
      publish({ locked: state.enabled && !exempt, error: '' });
    },
    dispose(): void {
      disposed = true;
      generation++;
    },
  };
}
