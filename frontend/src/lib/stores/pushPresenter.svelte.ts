// The phone's OTHER notification path: the one that does not go through
// Google (docs/specs/remote-access.md §9, "Push").
//
// A shell whose socket is still up while the OS has the app in the
// background is told about a moment twice — once on the wire, once by
// Google — and the wire arrives first and says MORE: it carries the
// thread's own title, which the pushed payload may not, because that one
// transits a third party. So when the socket is there, this is what
// presents.
//
// **Shell only.** A browser and a desktop have their own presenters
// (the OS notification on the host, the app's own badges everywhere
// else), and this subscription is never installed for them.
//
// **Background only.** In the foreground the app's own badges are the
// presentation, exactly as on the desktop, and a tray notification over
// an app the person is looking at is the double notification the whole
// design avoids.
//
// **Retractions are never gated on the lease.** What a retraction
// withdraws was posted while the app was in the background, and the app
// coming forward does not take it off the tray — the same rule
// `notifyOS` and `TrayNotifier` hold, for the same reason.

import { isNativeShell } from '../native/platform';
import { pushPlugin, type PushNotificationPlugin } from '../native/plugins';
import { HOME_BACKEND } from '../transport/backendKey';
import { clientLease } from '../transport/lease';
import { wailsEventOn } from './wailsEvents';

/** The `notify.Send` shape, as the wire spells it. */
interface NotificationSend {
  id?: unknown;
  kind?: unknown;
  title?: unknown;
  body?: unknown;
  retract?: unknown;
  target?: unknown;
}

let plugin: PushNotificationPlugin | null = null;
let cancel: (() => void) | null = null;

/**
 * Start presenting backend notifications on this phone's tray.
 *
 * Answers a teardown. Called from `native/boot.ts`; never awaited, and on
 * nothing's critical path.
 */
export async function startPushPresenter(): Promise<() => void> {
  if (!isNativeShell() || cancel !== null) return stopPushPresenter;
  plugin = await pushPlugin();
  if (plugin === null) return stopPushPresenter;

  cancel = wailsEventOn<NotificationSend>('notification:send', (send, origin) => {
    void present(send, origin.backendId);
  });
  return stopPushPresenter;
}

/** Drop the subscription. */
export function stopPushPresenter(): void {
  cancel?.();
  cancel = null;
  plugin = null;
}

/**
 * The tray tag for one send, which is what makes a later state change
 * REPLACE a notification and a retraction cancel exactly it.
 *
 * HOME KEEPS THE PLAIN ID, and that is deliberate rather than an
 * omission: the pushed path tags with the plain id too, and the two
 * paths can both fire for one moment on a backgrounded phone whose
 * socket is still alive. Sharing the tag makes the second one REPLACE the
 * first, which is one notification; namespacing home would make it two.
 *
 * A second attached backend gets its own namespace, because not every
 * notification id is unique across machines — `provider-auth:claude` is
 * the same string on every backend the owner runs, and without the prefix
 * one machine's sign-out notice would silently replace another's.
 */
export function pushTag(id: string, backendId: string): string {
  if (backendId === '' || backendId === HOME_BACKEND) return id;
  return `${backendId}|${id}`;
}

async function present(send: NotificationSend, backendId: string): Promise<void> {
  const bridge = plugin;
  if (bridge === null) return;
  const id = typeof send.id === 'string' ? send.id : '';
  if (id === '') return;
  const tag = pushTag(id, backendId);

  try {
    if (send.retract === true) {
      await bridge.retract({ id: tag });
      return;
    }
    if (clientLease() !== 'background') return;
    await bridge.present({
      id: tag,
      kind: text(send.kind),
      title: text(send.title),
      body: text(send.body),
      // The route travels as its own document, the same shape the pushed
      // payload carries, so the native side has one thing to forward and
      // one thing to put on the launch intent.
      target: send.target === undefined ? '' : JSON.stringify(send.target),
    });
  } catch (err) {
    console.warn('push: this notification could not be presented', err);
  }
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}
