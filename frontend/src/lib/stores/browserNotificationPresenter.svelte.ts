// The REMOTE browser's notification presenter: the third screen kind, beside
// the host's native banner and the phone shell's tray
// (./pushPresenter.svelte.ts).
//
// EVERY SCREEN DECIDES FOR ITSELF. `App.notifyOS` raises a banner for the
// BACKEND MACHINE's own screen, from that machine's settings and that
// machine's focus, and publishes its cue on `notification:sound` — a channel
// that is loopback-only precisely because the decision behind it belongs to
// one screen. A browser attached from another room receives the
// `notification:send` frame (AudienceAny) and is a different screen: its own
// per-kind toggles, its own hidden-thread opt-in, its own quiet-when reading
// and its own speaker. Until this module existed nothing consumed that frame
// there, so those preferences were rows in a settings page that decided
// nothing.
//
// INSTALLED ONLY WHERE THE HOST DOES NOT ALREADY PRESENT. A loopback page IS
// the host's screen — `notifyOS` raised a native banner for it, and a second
// Web Notification would be the same moment twice. The native shell has its
// own presenter. Both are refused at install rather than at each frame, so
// there is no per-send branch to get wrong.
//
// PERMISSION, CAPABILITY AND PREFERENCE ARE SEPARATE STATES
// (components/settings/AGENTS.md). This module never asks for permission: a
// `Notification.requestPermission()` call from an event handler is not a user
// gesture and every engine either refuses it or, worse, shows a prompt the
// person cannot connect to anything they did. Settings owns the ask
// (NotificationsSection.svelte). Where permission is absent or denied, or the
// engine has no `Notification` at all (an insecure context is the common
// case for a LAN page), the CUE still plays — the same rule
// `publishNotificationSound` states host-side: on a screen whose permission
// was refused, the sound is the only channel left, and withholding it would
// silence the user twice for one platform refusal.

import { isNativeShell } from '../native/platform';
import { notificationRefusal, soundCueFor, soundEventFor } from '../notifications/gate';
import { notificationTag } from '../notifications/tag';
import { pageServedOverLoopback } from '../transport/bootstrap';
import { backendKeyForOrigin } from '../transport/backends';
import type { EventOrigin } from '../transport/handle';
import { applyNotificationActivated } from './eventsNotification';
import { parseNotificationTarget, type NotificationTarget } from './notificationActivationQueue';
import { playNotificationCue } from './notificationSound';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { currentScreenPresence } from './screenPresence';
import { getSettings } from './settings.svelte';
import { wailsEventOn } from './wailsEvents';

/** The `notify.Send` shape, as the wire spells it. */
interface NotificationSend {
  id?: unknown;
  kind?: unknown;
  title?: unknown;
  body?: unknown;
  retract?: unknown;
  hiddenThread?: unknown;
  target?: unknown;
}

// Every notification this page put on screen, by tag, so a retraction can
// close exactly the one it names. The Web Notification API has no "close the
// notification with this tag" call — only the handle does — and a tag that
// was never raised is simply absent, which is the no-op every other presenter
// answers for the same case.
const presented = new Map<string, Notification>();
let cancel: (() => void) | null = null;

// Stable diagnostic message; the variable part rides the detail argument.
// A diagnostic rather than console.warn because a console line is invisible
// in a production build, and a screen that silently never shows a banner
// is the one symptom this presenter must not have.
const RAISE_FAILED = 'browser notification could not be raised';

/**
 * Whether this page presents notifications itself.
 *
 * Exported so the settings page asks the same question rather than composing
 * its own: the permission callout and the local sound preview exist exactly
 * where this presenter does, and two spellings of "am I the remote screen"
 * would eventually disagree about one of the two.
 */
export function pagePresentsNotificationsLocally(): boolean {
  return !pageServedOverLoopback() && !isNativeShell();
}

/** Whether this engine can put a notification on screen at all. */
export function browserNotificationsAvailable(): boolean {
  return typeof Notification === 'function';
}

/**
 * The permission this page holds, or 'unavailable' where the API is absent.
 *
 * A capability that does not exist is a THIRD state rather than a denial: the
 * settings page offers an Allow button for 'default' and says "blocked" for
 * 'denied', and neither sentence is true of a page served over plain HTTP,
 * where the constructor is simply not there to ask.
 */
export function browserNotificationPermission(): NotificationPermission | 'unavailable' {
  return browserNotificationsAvailable() ? Notification.permission : 'unavailable';
}

/**
 * Start presenting this backend's notifications on this screen.
 *
 * Answers a teardown. Installed from `events.ts`'s subscription root, which
 * owns the disposer; a page that is not the remote screen subscribes to
 * nothing and answers a teardown that does nothing.
 */
export function startBrowserNotificationPresenter(): () => void {
  if (!pagePresentsNotificationsLocally() || cancel !== null) return stopBrowserNotificationPresenter;
  cancel = wailsEventOn<NotificationSend>('notification:send', present);
  return stopBrowserNotificationPresenter;
}

/**
 * Drop the subscription and close every notification this page raised.
 *
 * Closing is the point rather than tidiness: the handles outlive the page's
 * event listeners, so a banner left behind would be one nothing can retract —
 * its `onclick` would route into a torn-down app, and the moment it describes
 * has no live subscriber to withdraw it.
 */
export function stopBrowserNotificationPresenter(): void {
  cancel?.();
  cancel = null;
  for (const notification of presented.values()) close(notification);
  presented.clear();
}

/**
 * Closing a notification is a platform call like any other and can throw on a
 * handle the engine has already collected. It is never worth failing a frame
 * over: the outcome the caller wants — this tag is no longer ours — is
 * recorded either way.
 */
function close(notification: Notification): void {
  try {
    notification.close();
  } catch (err) {
    console.warn('notifications: closing a presented notification failed', err);
  }
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function present(send: NotificationSend, origin: EventOrigin, _sequence: number | undefined, replayed: boolean): void {
  const id = text(send.id);
  if (id === '') return;
  // Namespaced by backend, exactly as the phone's tray tag is: notification
  // ids are stable per MOMENT, not per machine (`provider-auth:claude` is the
  // same string on every backend), so without the prefix one computer's
  // sign-out notice would silently replace another's.
  const tag = notificationTag(id, origin.backendId);

  // A RETRACTION IS NEVER GATED, by preferences or by replay. It withdraws
  // something already on this screen, which is the opposite of an
  // interruption — and gating it would let a toggle flipped mid-flight, or a
  // reconnect, strand the very alert it was meant to stop.
  if (send.retract === true) {
    const open = presented.get(tag);
    if (open === undefined) return;
    presented.delete(tag);
    close(open);
    return;
  }

  const settings = getSettings(backendKeyForOrigin(origin.backendId));
  const screen = currentScreenPresence();
  const refusal = notificationRefusal(
    settings,
    {
      kind: text(send.kind),
      hiddenThread: send.hiddenThread === true,
      target: { kind: text((send.target as { kind?: unknown } | null)?.kind) },
    },
    { focused: screen.focused, threadVisible: threadVisible(send.target, screen.threads) },
  );
  if (refusal !== null) return;

  const cue = soundCueFor(settings, text(send.kind));
  raise(send, tag, cue);
  // THE CUE IS THE SECOND PRESENTATION, and it rides the one decision above
  // rather than asking again — the same structure `notifyOS` holds, so a
  // sound can never fire where a banner was suppressed. It plays even where
  // `raise` put nothing on screen, because a screen with no notification
  // permission still has speakers.
  //
  // `system` names the platform's own banner sound, which `raise` asked for
  // by leaving `silent` false. Playing a cue for it too would be the double
  // sound the host's `hostBannerSilentIn` exists to prevent.
  //
  // NOT FOR A REPLAYED FRAME. A banner re-raised after a reconnect replaces
  // itself by tag and still describes something true; a cue names a MOMENT,
  // and one replayed minutes later is a noise with nothing behind it.
  if (cue !== null && cue !== 'system' && !replayed) {
    // The event travels so a `custom:<id>` this screen's listing has lost
    // falls back to that event's default rather than to silence — the same
    // substitution a host-sent frame gets, and the reason that frame carries
    // its event at all.
    playNotificationCue(cue, soundEventFor(text(send.kind)) ?? undefined);
  }
}

/**
 * Whether the send's own thread is one this screen is showing. Only a target
 * that NAMES a thread can be — the gate depends on that narrowing, and it is
 * stated here rather than left to a falsy id because a send with no thread
 * must never match a pane showing none.
 */
function threadVisible(target: unknown, threads: readonly string[]): boolean {
  if (!target || typeof target !== 'object') return false;
  const { kind, threadId } = target as { kind?: unknown; threadId?: unknown };
  if (kind !== 'thread' || typeof threadId !== 'string' || threadId === '') return false;
  return threads.includes(threadId);
}

/**
 * Put one notification on this screen, replacing whatever this page raised
 * under the same tag.
 *
 * `silent` is this SCREEN's answer, resolved from the settings read above —
 * never `send.silent`, which is the host presenter's answer about the desk's
 * speakers and says nothing about this room. The banner keeps the platform's
 * own sound in exactly one case, the same one the host keeps it in: this
 * screen's cue for the event IS the system sound.
 */
function raise(send: NotificationSend, tag: string, cue: string | null): void {
  if (!browserNotificationsAvailable() || Notification.permission !== 'granted') return;
  const target = parseNotificationTarget(send.target);
  let notification: Notification;
  try {
    notification = new Notification(text(send.title), {
      body: text(send.body),
      tag,
      silent: cue !== 'system',
    });
  } catch (err) {
    // A constructor that throws is a capability failure, not a refusal: some
    // engines throw for a page without an active service worker, and an
    // unhandled one here would take down the event pump for every other
    // subscriber on this frame.
    reportFrontendDiagnostic(RAISE_FAILED, errString(err));
    return;
  }
  presented.set(tag, notification);
  notification.onclose = () => {
    // Only if this handle is still the one under the tag: a replacement has
    // already taken the slot, and dropping it here would leave the new
    // notification unretractable.
    if (presented.get(tag) === notification) presented.delete(tag);
  };
  notification.onclick = () => activate(send, notification, tag, target);
}

/**
 * A click brings the window forward and routes the target the same way the
 * host's `notification:activated` handler does — through the bounded
 * pre-hydration queue, which owns retry and the "no longer available" toast.
 *
 * The notification is closed after routing: a tag left open would be
 * withdrawn later by a retraction the person has already acted on.
 */
function activate(send: NotificationSend, notification: Notification, tag: string, target: NotificationTarget | null): void {
  try {
    window.focus();
  } catch (err) {
    // A window an engine refuses to focus is not a reason to drop the route;
    // the person clicked the notification and the pane must still open.
    console.warn('notifications: focusing the window from a notification failed', err);
  }
  if (presented.get(tag) === notification) presented.delete(tag);
  close(notification);
  // A `none` target parses and routes; the queue is what reports that it
  // opens nothing. `null` is a frame whose target is not a shape any
  // presenter may act on, which is the same warning the host path makes.
  if (target !== null) applyNotificationActivated(target);
  else console.warn('notification:activated: invalid target', send.target);
}

/** Test-only: drop the subscription and every recorded handle. */
export function __resetBrowserNotificationPresenterForTest(): void {
  stopBrowserNotificationPresenter();
}
