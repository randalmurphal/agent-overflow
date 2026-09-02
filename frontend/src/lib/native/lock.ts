// The app lock: the strongest gate the platform offers, in front of the
// whole app (docs/specs/remote-access.md § "Opening the app" — the phone
// is more sensitive than anything else on it).
//
// It runs on cold start and on resume after a background window has
// elapsed, and until it passes the WebView shows
// `components/native/LockScreen.svelte` over everything. There is no
// passkey ceremony in the WebView and there does not need to be: step-up
// already lives at pairing-mint on the GRANTING side, which is the
// owner's desktop with its passkey. What this gate protects is the app
// being OPEN on a phone somebody picked up.
//
// **No biometrics enrolled is not a refusal.** `allowDeviceCredential`
// lets the platform fall back to the PIN, pattern or password the phone
// is already unlocked with, which is the strongest gate a device without
// a fingerprint reader has. Refusing to open instead would lock the owner
// out of their own app for a hardware feature they never had.
//
// **The window is a per-device setting**, in localStorage under
// `agent-overflow:lockWindowMs`, defaulting to five minutes. A Settings
// row for it is deliberately not in this wave: the value is a fact about
// this phone rather than about the backend, so it belongs beside the
// other client-local preferences whenever those get a compact screen.

import { appPlugin, biometricPlugin } from './plugins';
import { isNativeShell } from './platform';

const LOCK_WINDOW_STORE_KEY = 'agent-overflow:lockWindowMs';

/** Five minutes. The spec's default, and the one a person notices least. */
export const DEFAULT_LOCK_WINDOW_MS = 5 * 60_000;

/**
 * Whether a resume has to re-prompt.
 *
 * Pure, and the whole decision — which is why it is testable without a
 * plugin, a clock or a phone. Three cases, and each of them is a real
 * one rather than defensive padding:
 *
 *   - **Never paused (`null`)** is the COLD START, and it locks. That is
 *     the case the gate exists for.
 *   - **A window of zero or less** locks every time. Somebody who set it
 *     that way asked for exactly that.
 *   - **A `lastPausedAt` in the future** — a clock that moved backwards
 *     while the app was away, which is ordinary on a phone that just
 *     picked up network time — locks rather than trusting the arithmetic.
 *     Erring toward one prompt is the cheap direction.
 */
export function shouldLock(
  lastPausedAt: number | null,
  now: number,
  windowMs: number,
): boolean {
  if (lastPausedAt === null) return true;
  if (windowMs <= 0) return true;
  if (lastPausedAt > now) return true;
  return now - lastPausedAt >= windowMs;
}

/** The window this device is set to, clamped to something sane. */
export function lockWindowMs(): number {
  if (typeof localStorage === 'undefined') return DEFAULT_LOCK_WINDOW_MS;
  let raw: string | null;
  try {
    raw = localStorage.getItem(LOCK_WINDOW_STORE_KEY);
  } catch {
    return DEFAULT_LOCK_WINDOW_MS;
  }
  // Trimmed and emptiness-checked BEFORE the parse, because `Number('')`
  // is 0 and 0 is a legitimate setting meaning "prompt every time" — so a
  // half-written entry would silently become the strictest possible lock
  // rather than reading as unset.
  const trimmed = (raw ?? '').trim();
  if (trimmed === '') return DEFAULT_LOCK_WINDOW_MS;
  const parsed = Number(trimmed);
  // Not a number, or negative, reads as unset for the same reason.
  if (!Number.isFinite(parsed) || parsed < 0) return DEFAULT_LOCK_WINDOW_MS;
  return parsed;
}

export interface AppLock {
  /** Whether the lock screen should be showing right now. */
  locked: () => boolean;
  /** Run the prompt. Answers whether the app is now unlocked. */
  unlock: () => Promise<boolean>;
  /** Stop listening. */
  dispose: () => void;
}

export interface AppLockOptions {
  /** How long the app may be backgrounded before the next resume prompts. */
  backgroundWindowMs?: number;
  /** Told whenever `locked()` would answer differently. */
  onChange?: (locked: boolean) => void;
}

/**
 * Install the lock. Off the shell this answers an object that is
 * permanently unlocked and listens to nothing, which is what lets
 * `main.ts` call it without asking where it is running.
 *
 * The pause timestamp is kept HERE rather than read back from the
 * lifecycle seam: the two subscribe to the same plugin events, and the
 * lock's copy has to be written before any resume handler reads it, which
 * ordering between two independent subscribers cannot promise.
 */
export async function installAppLock(options: AppLockOptions = {}): Promise<AppLock> {
  const inert: AppLock = { locked: () => false, unlock: async () => true, dispose: () => {} };
  if (!isNativeShell()) return inert;

  const biometric = await biometricPlugin();
  if (!biometric) return inert;

  const windowMs = options.backgroundWindowMs ?? lockWindowMs();
  let locked = true;
  let lastPausedAt: number | null = null;
  const publish = (): void => options.onChange?.(locked);

  const unlock = async (): Promise<boolean> => {
    try {
      await biometric.authenticate({
        reason: 'Unlock Agent Overflow',
        androidTitle: 'Unlock Agent Overflow',
        // A phone with no enrolled biometric still has the credential it
        // is unlocked with, and that is the strongest gate it offers.
        allowDeviceCredential: true,
      });
    } catch {
      // Dismissed, failed, or unavailable. All three mean the app stays
      // shut and the person presses Unlock again; none of them is worth
      // a message that accuses somebody of a fault they did not commit.
      return false;
    }
    locked = false;
    lastPausedAt = null;
    publish();
    return true;
  };

  const app = await appPlugin();
  const handles = app
    ? await Promise.all([
        app.addListener('pause', () => {
          lastPausedAt = Date.now();
        }),
        app.addListener('resume', () => {
          if (locked) return;
          if (!shouldLock(lastPausedAt, Date.now(), windowMs)) return;
          locked = true;
          publish();
          // The prompt comes up on its own, as it does on a cold start.
          // A person who just picked the phone back up should be looking
          // at the platform's prompt, not at a button that asks for it.
          void unlock();
        }),
      ])
    : [];

  // Cold start: the gate runs before anything is on screen. The caller
  // mounts the lock screen first and this settles behind it.
  publish();
  void unlock();

  return {
    locked: () => locked,
    unlock,
    dispose: () => {
      for (const handle of handles) void handle.remove();
    },
  };
}
