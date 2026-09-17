// The notification gate, as a pure decision.
//
// TWO SCREENS, ONE RULE. `App.notifyOS` (internal/app/app_notifications.go)
// decides whether a moment may interrupt the BACKEND MACHINE's own screen,
// from that screen's settings and that screen's focus. A browser attached
// from somewhere else is a different screen with different answers, and it
// presents for itself (`stores/browserNotificationPresenter.svelte.ts`) — so
// the same decision has to run here, against this page's settings and this
// page's focus.
//
// It is the same rule, not a similar one, and the two are held together by a
// shared decision table both sides run case for case:
// `internal/notify/testdata/gate_cases.json`
// (`internal/app/app_notification_gate_cases_test.go` and `./gate.test.ts`).
// Drift between two copies of a preference gate is invisible in production:
// the symptom is one screen staying quiet where the other spoke, from
// settings pages that read identically.
//
// PURE, AND IT DECIDES ONLY WHETHER TO INTERRUPT. No store read, no DOM, no
// transport: the caller supplies the settings and the two screen facts. It
// changes nothing about what this client is sent, fetches or renders — the
// alternative to a banner is no banner, never a stale pane.
//
// RETRACTIONS DO NOT COME HERE. A withdrawal is the opposite of an
// interruption, and gating it would strand an alert on a screen whose owner
// flipped a toggle mid-flight. The presenter branches on `retract` first, the
// same order `notifyOS` holds.

import type { NotifyCue, NotifyQuietWhen } from '../types/settings';

/**
 * The wire spellings of `notify.Kind` (internal/notify/mapping.go). Read off
 * `Send.Kind`, so these are wire input: a value outside this set is refused
 * rather than raised.
 */
export const NOTIFY_KINDS = [
  'turn-complete',
  'approval-needed',
  'error',
  'provider-signed-out',
  'workflow-attention',
  'app-update',
] as const;
export type NotifyKind = (typeof NOTIFY_KINDS)[number];

/** The wire spellings of `notify.SoundEvent` (internal/notify/sound.go). */
export type NotifySoundEvent = 'turn-complete' | 'input-needed' | 'attention';

/**
 * Why a send was not raised, or null when it may be.
 *
 * The three answers are distinct for the reason the Go codes are: "you turned
 * the kind off", "that thread is not on your sidebar" and "you were already
 * looking" are different answers to "why did I not get a notification", and a
 * single boolean cannot tell them apart in a diagnostic.
 */
export type NotificationRefusal = 'suppressed' | 'hidden_thread' | 'screen_attended';

/** The preferences the gate reads. `Settings` satisfies it structurally. */
export interface NotificationGateSettings {
  notificationsEnabled: boolean;
  notifyTurnComplete: boolean;
  notifyApprovalNeeded: boolean;
  notifyError: boolean;
  notifyProviderSignedOut: boolean;
  notifyWorkflowAttention: boolean;
  notifyAppUpdate: boolean;
  notifyHiddenThreads: boolean;
  notifyQuietWhen: NotifyQuietWhen;
  notificationSoundsEnabled: boolean;
  notifySoundTurnComplete: boolean;
  notifySoundInputNeeded: boolean;
  notifySoundAttention: boolean;
  notifySoundCueTurnComplete: NotifyCue;
  notifySoundCueInputNeeded: NotifyCue;
  notifySoundCueAttention: NotifyCue;
}

/** The part of a `notify.Send` frame the gate reads. */
export interface NotificationGateSend {
  kind: string;
  hiddenThread?: boolean;
  target?: { kind?: string; threadId?: string };
}

/**
 * What this screen last said about itself: whether the app is in front, and
 * whether the send's own thread is on screen. The presenter derives both from
 * the same source `stores/screenPresence.ts` reports to the backend, so a
 * remote page and the desk answer "already looking" from the same facts.
 */
export interface NotificationScreenFacts {
  focused: boolean;
  threadVisible: boolean;
}

/**
 * Whether one kind may interrupt this screen at all.
 *
 * TOTAL over the closed set with no permissive default, mirroring
 * `notificationKindEnabledIn`: a kind this build does not declare is one the
 * user was never offered a way to silence, and raising it would be
 * interrupting somebody with something they cannot turn off.
 */
function kindEnabled(settings: NotificationGateSettings, kind: string): boolean {
  if (!settings.notificationsEnabled) return false;
  switch (kind) {
    case 'turn-complete':
      return settings.notifyTurnComplete;
    case 'approval-needed':
      return settings.notifyApprovalNeeded;
    case 'error':
      return settings.notifyError;
    case 'provider-signed-out':
      return settings.notifyProviderSignedOut;
    case 'workflow-attention':
      return settings.notifyWorkflowAttention;
    case 'app-update':
      return settings.notifyAppUpdate;
    default:
      return false;
  }
}

/**
 * Whether this screen is already showing what the send was about to say,
 * mirroring `screenAttendedIn`.
 *
 * `hasThreadTarget` is its own argument rather than something folded into
 * `threadVisible` because it is the narrowing the two thread readings depend
 * on: a send that does not NAME a thread has no thread for a pane to be
 * showing, so it is raised under both and judged by focus alone under the
 * third.
 */
function screenAttended(
  quietWhen: string,
  focused: boolean,
  threadVisible: boolean,
  hasThreadTarget: boolean,
): boolean {
  const visible = threadVisible && hasThreadTarget;
  switch (quietWhen) {
    case 'never':
      return false;
    case 'focused':
      return focused;
    case 'threadVisible':
      return visible;
    case 'focusedAndThreadVisible':
      return focused && visible;
    default:
      // The four readings are all a settings write may carry; an unknown one
      // is a settings bug, and raising the notification is the answer that
      // loses nothing.
      console.warn(`notifications: unknown notifyQuietWhen ${quietWhen}, raising`);
      return false;
  }
}

/**
 * The whole gate for one PRESENTATION, in the order `App.notifyOS` holds it:
 * the per-kind toggle under the master switch, then the hidden-thread opt-in,
 * then the attended screen.
 *
 * The order is observable, not an implementation detail — it is which reason
 * a refusal reports — so the shared table asserts it.
 */
export function notificationRefusal(
  settings: NotificationGateSettings,
  send: NotificationGateSend,
  screen: NotificationScreenFacts,
): NotificationRefusal | null {
  if (!kindEnabled(settings, send.kind)) return 'suppressed';
  if (send.hiddenThread === true && !settings.notifyHiddenThreads) return 'hidden_thread';
  if (screenAttended(
    settings.notifyQuietWhen,
    screen.focused,
    screen.threadVisible,
    send.target?.kind === 'thread',
  )) return 'screen_attended';
  return null;
}

/**
 * Which sound event a kind belongs to, mirroring `notify.SoundEventFor`.
 * Three events, not six: the kinds answer three questions a person reacts to
 * differently, and a cue per kind would be a distinction nobody makes by ear.
 */
export function soundEventFor(kind: string): NotifySoundEvent | null {
  switch (kind) {
    case 'turn-complete':
      return 'turn-complete';
    case 'approval-needed':
      return 'input-needed';
    case 'error':
    case 'provider-signed-out':
    case 'workflow-attention':
    case 'app-update':
      return 'attention';
    default:
      return null;
  }
}

/**
 * The cue this screen plays for one kind, or null when it plays none.
 *
 * The same reading as `notificationSoundCueIn` over `SoundEventFor`: the
 * sounds master switch, then this event's own toggle, then the cue chosen for
 * it. TOTAL with no permissive default, for the reason the Go copy is — an
 * event this build does not know has no preference behind it, and playing an
 * unrequested sound is worse than playing none.
 *
 * The value is returned VERBATIM, including `system` and `custom:<id>`.
 * Neither is this function's business: `system` means the banner carries the
 * platform's own sound and nothing is played in the page, and a `custom:<id>`
 * names a file in the backend host's sounds library that
 * `stores/notificationSound.ts` resolves (substituting the event's default
 * when this screen's listing does not hold it). Rewriting either here would
 * be a second reading of a preference that has already been read.
 *
 * It answers the same for a send the gate refused: it is a settings reading,
 * not a decision. The caller asks only after `notificationRefusal` said yes,
 * which is what keeps a cue from firing where a banner was suppressed.
 */
export function soundCueFor(settings: NotificationGateSettings, kind: string): string | null {
  if (!settings.notificationSoundsEnabled) return null;
  switch (soundEventFor(kind)) {
    case 'turn-complete':
      return settings.notifySoundTurnComplete ? settings.notifySoundCueTurnComplete : null;
    case 'input-needed':
      return settings.notifySoundInputNeeded ? settings.notifySoundCueInputNeeded : null;
    case 'attention':
      return settings.notifySoundAttention ? settings.notifySoundCueAttention : null;
    default:
      return null;
  }
}
