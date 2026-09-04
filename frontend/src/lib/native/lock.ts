// The app lock: the strongest gate the platform offers, in front of the
// whole app (docs/specs/remote-access.md § "Opening the app" — the phone
// is more sensitive than anything else on it).
//
// The lock screen goes up the moment the OS pauses the app, and the WebView
// stays behind `components/native/LockScreen.svelte` until the gate passes.
// Covering on PAUSE rather than on resume is what keeps the app's own
// pixels out of the task switcher's thumbnail and off the screen for the
// frame before a resume handler can run. The background WINDOW is still
// what decides whether resuming asks for a prompt or simply lifts the
// cover: a person switching apps for five seconds is not asked again,
// unless a prompt was already owed when they left. There is no
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
  // Two facts, not one. `covered` is whether the lock screen is painted;
  // `owed` is whether the platform prompt has to PASS before it can come
  // down. They part ways on a short trip: the cover goes up on every
  // pause and comes down on a quick return, but a prompt that was owed
  // before the trip (a cold start still waiting, a prompt that was
  // dismissed) is still owed after it. Folding them into one flag let a
  // three-second trip to another app lift a prompt nobody had passed.
  let covered = true;
  let owed = true;
  let lastPausedAt: number | null = null;
  // The prompt in flight, so a second request joins it. The platform's
  // own dialog can pause and resume this app (the device-credential
  // fallback is another activity), and that resume must not stack a
  // second prompt on the one it interrupted.
  let prompting: Promise<boolean> | null = null;
  // Whether the pause most recently seen was the PROMPT'S OWN. The
  // platform draws the prompt in an activity of its own, so this app is
  // paused while it is up and resumed when it closes — and on Android
  // that resume lands AFTER the answer. It is not a trip, and reading it
  // as one did two things (2026-09-03, the emulator smoke): a passed
  // prompt was followed by a second one, because the success had cleared
  // `lastPausedAt` and the resume then read as a cold start; and a
  // dismissed prompt was raised again on the spot, with no way out of it
  // short of killing the app.
  let pausedForPrompt = false;
  const publish = (): void => options.onChange?.(covered);

  const prompt = async (): Promise<boolean> => {
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
    owed = false;
    covered = false;
    lastPausedAt = null;
    publish();
    return true;
  };

  const unlock = (): Promise<boolean> => {
    if (prompting === null) {
      prompting = prompt().finally(() => {
        prompting = null;
      });
    }
    return prompting;
  };

  const cover = (): void => {
    if (covered) return;
    covered = true;
    publish();
  };

  const app = await appPlugin();
  const handles = app
    ? await Promise.all([
        app.addListener('pause', () => {
          if (prompting !== null) pausedForPrompt = true;
          else lastPausedAt = Date.now();
          // Covered on the way OUT, not on the way back in. A cover that
          // waited for resume left the app's own pixels on screen for the
          // whole time it was away: in the task switcher's thumbnail, and
          // for the frame between the window being shown again and the
          // resume handler running. What resume decides is whether to
          // PROMPT, never whether the screen is covered.
          cover();
        }),
        app.addListener('resume', () => {
          // The prompt closing. Its answer decided the state, or is about
          // to; there is no decision to make here, and a dismissed prompt
          // waits for the button rather than coming straight back.
          if (pausedForPrompt) {
            pausedForPrompt = false;
            return;
          }
          // A prompt owed before the trip is owed after it, however short
          // the trip was. `unlock` joins a prompt still up rather than
          // raising a second one.
          if (owed) {
            void unlock();
            return;
          }
          if (shouldLock(lastPausedAt, Date.now(), windowMs)) {
            // Past the window: the prompt is owed again. A person who just
            // picked the phone back up should be looking at the platform's
            // prompt, not at a button that asks for it.
            owed = true;
            cover();
            void unlock();
            return;
          }
          // A short trip: lift the cover the pause put up, and ask nothing.
          if (!covered) return;
          covered = false;
          lastPausedAt = null;
          publish();
        }),
      ])
    : [];

  // Cold start: the gate runs before anything is on screen. The caller
  // mounts the lock screen first and this settles behind it.
  publish();
  void unlock();

  return {
    locked: () => covered,
    unlock,
    dispose: () => {
      for (const handle of handles) void handle.remove();
    },
  };
}
