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
// Four things it DOES own, because they are properties of the speaker rather
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
//   - THE SUBSTITUTION FOR A MISSING CUSTOM CUE. A `custom:<id>` names a file
//     in the BACKEND HOST's sounds directory, and the frame can name one this
//     screen's listing does not have — deleted a second ago, or not yet
//     loaded. Playing the event's default instead is a PLAYBACK
//     substitution, not a preference read: the frame already carries which
//     event it is, and the defaults come from the generated mirror of
//     DefaultSettings, so nothing here consults what the user chose.

import { SETTINGS_DEFAULTS } from '../generated/settingsDefaults';
import boopCue from '../assets/sounds/boop.wav?url';
import chordCue from '../assets/sounds/chord.wav?url';
import humCue from '../assets/sounds/hum.wav?url';
import knockCue from '../assets/sounds/knock.wav?url';
import marimbaCue from '../assets/sounds/marimba.wav?url';
import popCue from '../assets/sounds/pop.wav?url';
import swooshCue from '../assets/sounds/swoosh.wav?url';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { customSoundUrl, ensureCustomSounds, onCustomSoundsChanged } from './sounds.svelte';

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

/** The prefix a cue value carries when it names a file, not a bundled asset. */
const CUSTOM_PREFIX = 'custom:';

/**
 * The built-in cue each sound EVENT falls back to, mirroring
 * settings.DefaultSettings. Read only to substitute for a `custom:<id>` this
 * screen's listing does not have — never to decide what to play.
 */
const EVENT_DEFAULT_CUE: Readonly<Record<string, string>> = {
  'turn-complete': SETTINGS_DEFAULTS.notifySoundCueTurnComplete,
  'input-needed': SETTINGS_DEFAULTS.notifySoundCueInputNeeded,
  attention: SETTINGS_DEFAULTS.notifySoundCueAttention,
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
const MISSING_CUSTOM_CUE = 'custom notification cue is missing, played the default';
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

/**
 * The URL one cue value plays from, or null when this screen cannot play it.
 *
 * `Object.hasOwn` rather than a bare index: a cue value arrives on the wire,
 * and `CUE_URLS['constructor']` is a truthy inherited property.
 */
function urlFor(cue: string): string | null {
  if (cue.startsWith(CUSTOM_PREFIX)) return customSoundUrl(cue.slice(CUSTOM_PREFIX.length));
  return Object.hasOwn(CUE_URLS, cue) ? CUE_URLS[cue] : null;
}

function elementFor(cue: string): HTMLAudioElement | null {
  const url = urlFor(cue);
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
 * Drop every cached element backed by an object URL the sounds listing has
 * just revoked. Keeping one would mean a cue that silently plays nothing for
 * the rest of the page's life, and the element itself holds the decoded audio.
 */
function dropCustomElements(): void {
  for (const [cue, element] of elements) {
    if (!cue.startsWith(CUSTOM_PREFIX)) continue;
    element.pause();
    element.removeAttribute('src');
    element.load();
    elements.delete(cue);
  }
}

/**
 * Resolve what will actually be played, substituting the event's default for a
 * `custom:<id>` this screen does not have.
 */
function resolveCue(cue: string, event?: string): { cue: string; element: HTMLAudioElement } | null {
  const element = elementFor(cue);
  if (element) return { cue, element };
  if (cue.startsWith(CUSTOM_PREFIX)) {
    const fallbackCue = event !== undefined && Object.hasOwn(EVENT_DEFAULT_CUE, event)
      ? EVENT_DEFAULT_CUE[event]
      : null;
    const fallback = fallbackCue === null ? null : elementFor(fallbackCue);
    if (fallback && fallbackCue !== null) {
      reportFrontendDiagnostic(MISSING_CUSTOM_CUE, cue);
      return { cue: fallbackCue, element: fallback };
    }
  }
  reportFrontendDiagnostic(UNKNOWN_CUE, cue);
  return null;
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
  // The speaker also acquires the cue library here, for the page's lifetime.
  // It is NOT a preference read — the listing is the same for every screen
  // attached to this backend and says nothing about what the user chose — but
  // without it a `custom:<id>` frame would have no bytes to play the first
  // time it arrived, and a notification's whole value is being on time.
  ensureCustomSounds();
  const cancelLibrary = onCustomSoundsChanged(dropCustomElements);
  const arm = (): void => {
    unlocked = true;
  };
  const options: AddEventListenerOptions = { capture: true, once: true, passive: true };
  const events: readonly string[] = ['pointerdown', 'keydown', 'touchstart'];
  for (const name of events) window.addEventListener(name, arm, options);
  return () => {
    cancelLibrary();
    for (const name of events) window.removeEventListener(name, arm, { capture: true });
  };
}

/**
 * Plays one cue, subject to the cooldown and the gesture unlock.
 *
 * `event` names the sound event the cue was chosen for, and is used for one
 * thing only: substituting that event's default when `cue` is a `custom:<id>`
 * this screen's listing does not have. The `notification:sound` frame carries
 * it; the Settings preview passes the row it is auditioning.
 *
 * Never throws and never returns a rejected promise: every caller is an event
 * handler, and a notification that could break the event pump would be worse
 * than a notification nobody hears.
 */
export function playNotificationCue(cue: string, event?: string): void {
  if (!unlocked) {
    reportFrontendDiagnostic(NOT_UNLOCKED, cue);
    return;
  }
  const at = now();
  if (at - lastPlayedAt < NOTIFICATION_SOUND_COOLDOWN_MS) return;
  const resolved = resolveCue(cue, event);
  if (!resolved) return;
  // Claim the window BEFORE play() resolves: two frames in the same tick must
  // not both get through while the first is still starting.
  lastPlayedAt = at;
  const { element } = resolved;
  try {
    element.currentTime = 0;
    const started = element.play();
    // Older engines return undefined rather than a promise.
    if (started && typeof started.catch === 'function') {
      started.catch((cause: unknown) => {
        reportFrontendDiagnostic(PLAYBACK_FAILED, `${resolved.cue}: ${errString(cause)}`);
      });
    }
  } catch (cause) {
    reportFrontendDiagnostic(PLAYBACK_FAILED, `${resolved.cue}: ${errString(cause)}`);
  }
}

/** Event-handler entry point: validates the frame, then plays. */
export function applyNotificationSoundEvent(value: unknown): void {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return;
  const frame = value as Partial<NotificationSoundEvent>;
  if (typeof frame.cue !== 'string' || frame.cue === '') return;
  playNotificationCue(frame.cue, typeof frame.event === 'string' ? frame.event : undefined);
}

/** Test-only: drops the cooldown, the unlock and the cached elements. */
export function __resetNotificationSoundForTest(): void {
  dropCustomElements();
  elements.clear();
  lastPlayedAt = Number.NEGATIVE_INFINITY;
  unlocked = false;
}

/** Test-only: arms playback without dispatching a real gesture. */
export function __unlockNotificationSoundForTest(): void {
  unlocked = true;
}
