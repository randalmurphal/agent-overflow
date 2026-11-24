// Bundle-sync notices describe actionable state: a verified newer release
// awaiting restart, or a release requiring a newer APK. Cancelling the native
// pending selection clears its notice. Dismissal lasts while that selection
// remains staged; reconnects must not show the same notice again.
//
// The bundle updater (native/bundleSync.ts) is the only writer, and what it
// has confirmed is published here as `bundleUpdate`: a release downloading,
// one staged for the next start, or neither. A refusal's remedy on the
// phone reads it (stores/versionMismatch.ts).

import { clampString } from '../transport/frames';

// A backend's display name is its hostname, which nothing here controls.
// Clamped so a long one cannot push the banner's own sentence out of a
// one-line strip.
const MACHINE_NAME_MAX = 60;

let notice = $state('');

/** What the bundle updater has confirmed about the next release. */
export type BundleUpdate =
  | { readonly kind: 'none' }
  | { readonly kind: 'downloading' | 'ready'; readonly version: string };

const NO_UPDATE: BundleUpdate = { kind: 'none' };

// A release staged for the next start keeps dismissal stable until the
// pending update is cancelled.
let update: BundleUpdate = NO_UPDATE;

/**
 * The native pending selection was cancelled, or is being: no restart
 * loads it, so neither the notice nor a remedy may promise one.
 */
export function clearBundleNotice(): void {
  update = NO_UPDATE;
  notice = '';
}

/** Not reactive: read when wording a refusal (stores/versionMismatch.ts). */
export function bundleUpdate(): BundleUpdate {
  return update;
}

/** The updater started fetching release `version`. */
export function noteBundleDownloading(version: string): void {
  if (update.kind === 'ready') return;
  update = { kind: 'downloading', version };
}

/** The fetch ended without staging anything. */
export function noteBundleDownloadStopped(): void {
  if (update.kind === 'downloading') update = NO_UPDATE;
}

/** The sentence to show, or '' when there is nothing to say. Reactive. */
export function getBundleNotice(): string {
  return notice;
}

/**
 * A bundle of release `version` is downloaded, verified and staged. The
 * next cold start picks it up; nothing else is required and nothing is
 * interrupted.
 */
export function noteBundleReady(version: string): void {
  const told = update.kind === 'ready';
  update = { kind: 'ready', version };
  if (told) return;
  notice = 'A newer Agent Overflow is ready. It loads the next time the app starts.';
}

/**
 * The person has read the sentence. It re-appears next launch if the
 * staged bundle still has not been adopted, which is the right nag rate:
 * once per launch, never once per minute. The staged release survives so
 * a later floor refusal still cannot replace what dismissal cleared.
 */
export function dismissBundleNotice(): void {
  notice = '';
}

export function noteBundleTooOld(machineName: string): void {
  if (update.kind === 'ready') return;
  const machine = clampString(machineName.trim() || 'this backend', MACHINE_NAME_MAX);
  notice = `This app is too old for ${machine}. Install a newer Agent Overflow on this phone.`;
}

/** Test seam: forget what was published. */
export function __resetBundleNoticeForTest(): void {
  notice = '';
  update = NO_UPDATE;
}
