// The notification cue player.
//
// It DECIDES NOTHING. `App.notifyOS` is the one gate — the per-kind toggle,
// the hidden-thread opt-in, the attended-screen reading, the sound master
// switch, the per-event sound toggle and which cue that event uses are all
// resolved host-side against the backend machine's own screen — and this
// module plays what the `notification:sound` frame names. No second reading
// of settings happens here, so a sound can never fire where a banner was
// suppressed.
//
// Three things it DOES own, because they are properties of the speaker rather
// than of the moment:
//
//   - THE COOLDOWN. Four threads finishing inside a second is four frames and
//     one sound. A burst that stacked would be louder than any single cue and
//     would read as a fault.
//   - THE GESTURE UNLOCK. Every browser engine refuses audio started before
//     the page has been interacted with. The first pointer or key event arms
//     playback; frames arriving before that are dropped with a note, not
//     queued — a cue replayed when the user finally clicks names a moment
//     that has passed.
//   - FAILING VISIBLY. A refused or unavailable `play()` is reported once
//     through `reportFrontendDiagnostic` (console-only would be invisible in
//     a production build) and never thrown: audio is the one part of a
//     notification a headless run, a locked-down webview or a machine with no
//     sound card simply does not have.

import boopCue from '../assets/sounds/boop.wav?url';
import chordCue from '../assets/sounds/chord.wav?url';
import humCue from '../assets/sounds/hum.wav?url';
import knockCue from '../assets/sounds/knock.wav?url';
import marimbaCue from '../assets/sounds/marimba.wav?url';
import popCue from '../assets/sounds/pop.wav?url';
import swooshCue from '../assets/sounds/swoosh.wav?url';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

/**
 * The built-in cues, keyed by the value `settings.NotifyCue*` carries.
 * `system` is deliberately absent: it names the OS banner's own sound, and
 * the host never publishes a cue frame for it.
 */
const CUE_URLS: Readonly<Record<string, string>> = {
  swoosh: swooshCue,
  marimba: marimbaCue,
  chord: chordCue,
  knock: knockCue,
  pop: popCue,
  hum: humCue,
  boop: boopCue,
};

/**
 * Minimum spacing between cues. Long enough that the longest cue (~0.8 s) has
 * finished before another can start, so two never overlap, and short enough
 * that two genuinely separate events a couple of seconds apart are both heard.
 */
export const NOTIFICATION_SOUND_COOLDOWN_MS = 1_500;

// Diagnostic messages are CONSTANT (frontendErrorCapture dedupes on them);
// the variable part rides the detail argument.
const PLAYBACK_FAILED = 'notification cue playback failed';
const UNKNOWN_CUE = 'notification cue is not a built-in';
const NOT_UNLOCKED = 'notification cue suppressed before first user gesture';

const elements = new Map<string, HTMLAudioElement>();
let lastPlayedAt = Number.NEGATIVE_INFINITY;
let unlocked = false;

/** The wire payload of `notification:sound`. */
export interface NotificationSoundEvent {
  event: string;
  cue: string;
}

function now(): number {
  return typeof performance !== 'undefined' ? performance.now() : Date.now();
}

function elementFor(cue: string): HTMLAudioElement | null {
  const url = CUE_URLS[cue];
  if (!url) return null;
  const existing = elements.get(cue);
  if (existing) return existing;
  if (typeof Audio !== 'function') return null;
  const element = new Audio(url);
  // `auto` only after a cue has been asked for once: the assets are never
  // fetched on a screen that never raises a notification.
  element.preload = 'auto';
  elements.set(cue, element);
  return element;
}

/**
 * Arms playback on the first user interaction with the page.
 *
 * `once` listeners on the capture phase so a handler that stops propagation
 * cannot starve the unlock. Returns the teardown so a test (or a remounting
 * app root) can drop the listeners; production installs it for the page's
 * lifetime.
 */
export function installNotificationSoundUnlock(): () => void {
  if (typeof window === 'undefined') return () => {};
  const arm = (): void => {
    unlocked = true;
  };
  const options: AddEventListenerOptions = { capture: true, once: true, passive: true };
  const events: readonly string[] = ['pointerdown', 'keydown', 'touchstart'];
  for (const name of events) window.addEventListener(name, arm, options);
  return () => {
    for (const name of events) window.removeEventListener(name, arm, { capture: true });
  };
}

/**
 * Plays one cue, subject to the cooldown and the gesture unlock.
 *
 * Never throws and never returns a rejected promise: every caller is an event
 * handler, and a notification that could break the event pump would be worse
 * than a notification nobody hears.
 */
export function playNotificationCue(cue: string): void {
  if (!unlocked) {
    reportFrontendDiagnostic(NOT_UNLOCKED, cue);
    return;
  }
  const at = now();
  if (at - lastPlayedAt < NOTIFICATION_SOUND_COOLDOWN_MS) return;
  const element = elementFor(cue);
  if (!element) {
    reportFrontendDiagnostic(UNKNOWN_CUE, cue);
    return;
  }
  // Claim the window BEFORE play() resolves: two frames in the same tick must
  // not both get through while the first is still starting.
  lastPlayedAt = at;
  try {
    element.currentTime = 0;
    const started = element.play();
    // Older engines return undefined rather than a promise.
    if (started && typeof started.catch === 'function') {
      started.catch((cause: unknown) => {
        reportFrontendDiagnostic(PLAYBACK_FAILED, `${cue}: ${errString(cause)}`);
      });
    }
  } catch (cause) {
    reportFrontendDiagnostic(PLAYBACK_FAILED, `${cue}: ${errString(cause)}`);
  }
}

/** Event-handler entry point: validates the frame, then plays. */
export function applyNotificationSoundEvent(value: unknown): void {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return;
  const frame = value as Partial<NotificationSoundEvent>;
  if (typeof frame.cue !== 'string' || frame.cue === '') return;
  playNotificationCue(frame.cue);
}

/** Test-only: drops the cooldown, the unlock and the cached elements. */
export function __resetNotificationSoundForTest(): void {
  elements.clear();
  lastPlayedAt = Number.NEGATIVE_INFINITY;
  unlocked = false;
}

/** Test-only: arms playback without dispatching a real gesture. */
export function __unlockNotificationSoundForTest(): void {
  unlocked = true;
}
