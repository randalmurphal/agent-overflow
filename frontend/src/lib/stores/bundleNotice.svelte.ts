// The two sentences the bundle-sync seam is allowed to say to a person,
// and the only state behind them.
//
// **Two strings, each written once.** Everything else bundle sync does —
// choosing a backend, downloading megabytes, verifying, staging, rolling
// back — happens with nothing on the screen. The person is told only
// when there is something they could act on: a bundle that is ready and
// wants a restart, or a phone app too old to take the one its desk is
// offering. Failures are logged and retried, never surfaced; a phone
// that quietly keeps running the bundle it has is behaving correctly.
//
// They render through the existing transport banner
// (components/shared/TransportStatusBanner.svelte) rather than a second
// overlay of their own. One strip at the top of the app says what is
// true about this client's connection to its backend, and "your backend
// has a newer app than you" is exactly that kind of fact.
//
// A store, and reactive, because it is read from a `$derived` in that
// banner. It holds no wire subscription: `native/bundleSync.ts` pushes
// into it.

import { clampString } from '../transport/frames';

// A backend's display name is its hostname, which nothing here controls.
// Clamped so a long one cannot push the banner's own sentence out of a
// one-line strip.
const MACHINE_NAME_MAX = 60;

let notice = $state('');

// Whether the ready notice has been published this launch. Once a bundle
// is staged, a restart is the whole remaining action and nothing should
// replace that sentence — including a second backend whose floor this
// phone is under.
let staged = false;

/** The sentence to show, or '' when there is nothing to say. Reactive. */
export function getBundleNotice(): string {
  return notice;
}

/**
 * A bundle is downloaded, verified and staged. The next cold start picks
 * it up; nothing else is required and nothing is interrupted.
 */
export function noteBundleReady(): void {
  staged = true;
  notice = 'A newer Agent Overflow is ready. It loads the next time the app starts.';
}

/**
 * This backend's bundle needs a newer phone app than this one. Terminal
 * for the launch: no download is attempted, and the only fix is on the
 * app store side.
 */
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
