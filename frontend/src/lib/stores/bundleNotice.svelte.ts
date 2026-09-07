// Bundle-sync notices describe actionable state: a verified newer release
// awaiting restart, or a release requiring a newer APK. Cancelling the native
// pending selection clears its notice. Dismissal lasts while that selection
// remains staged; reconnects must not show the same notice again.

import { clampString } from '../transport/frames';

// A backend's display name is its hostname, which nothing here controls.
// Clamped so a long one cannot push the banner's own sentence out of a
// one-line strip.
const MACHINE_NAME_MAX = 60;

let notice = $state('');

// Keep dismissal stable until the pending update is cancelled.
let staged = false;

/** The native pending selection was cancelled; no restart is needed. */
export function clearBundleNotice(): void {
  staged = false;
  notice = '';
}

/** The sentence to show, or '' when there is nothing to say. Reactive. */
export function getBundleNotice(): string {
  return notice;
}

/**
 * A bundle is downloaded, verified and staged. The next cold start picks
 * it up; nothing else is required and nothing is interrupted.
 */
export function noteBundleReady(): void {
  if (staged) return;
  staged = true;
  notice = 'A newer Agent Overflow is ready. It loads the next time the app starts.';
}

/**
 * The person has read the sentence. It re-appears next launch if the
 * staged bundle still has not been adopted, which is the right nag rate:
 * once per launch, never once per minute. The `staged` latch survives so
 * a later floor refusal still cannot replace what dismissal cleared.
 */
export function dismissBundleNotice(): void {
  notice = '';
}

export function noteBundleTooOld(machineName: string): void {
  if (staged) return;
  const machine = clampString(machineName.trim() || 'this backend', MACHINE_NAME_MAX);
  notice = `This app is too old for ${machine}. Install a newer Agent Overflow on this phone.`;
}

/** Test seam: forget what was published. */
export function __resetBundleNoticeForTest(): void {
  notice = '';
  staged = false;
}
