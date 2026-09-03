// The phone shell's half of push: tell every attached backend where to
// reach this device (docs/specs/remote-access.md §9, "Push").
//
// The backend does the deciding — which moments are worth a notification,
// which phones may be woken, what the payload is allowed to say. This
// module owns one fact and its lifecycle: THE REGISTRATION TOKEN, and
// which backends have been told it.
//
// **Shell only, and inert everywhere else.** Every export returns
// immediately off a shell, the same shape every other seam in this
// directory has, so a browser build never resolves a Capacitor module.
//
// **A denied permission is remembered for THIS BOOT and asked again on
// the next cold start.** No prompt storm, no retry loop, and no
// permanent refusal either: a person who says no once and changes their
// mind restarts the app, which is a thing people already do.
//
// **Registering is idempotent and cheap.** The backend keys one row per
// device, so a re-registration replaces rather than accumulates. That is
// what lets this module answer "a backend was attached" and "the token
// rotated" with the same call and no bookkeeping about which pairs it has
// already done.

import { RegisterPushToken, UnregisterPushToken } from '../stores/bindings';
import { attachedBackends, onBackendsChanged, withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { isNativeShell } from './platform';
import { pushPlugin, type PushNotificationPlugin, type PushTap } from './plugins';

/** The platform this registration is for. One shell, one value. */
const PLATFORM = 'android';

let plugin: PushNotificationPlugin | null = null;
let token = '';
/** Set once a permission prompt is refused, for this launch only. */
let refusedThisBoot = false;
let installed: (() => void)[] = [];
let removeListeners: (() => Promise<void>)[] = [];

/**
 * Ask for the permission, get the token, and register it with every
 * attached backend — then keep doing that as backends come and go.
 *
 * Answers a teardown. Called from `native/boot.ts` beside the bundle
 * sync, and like it: never awaited, never on anything's critical path. A
 * phone that never gets here is a phone that is not woken, which is the
 * pre-push behaviour and not a failure.
 */
export async function startPushRegistration(): Promise<() => void> {
  if (!isNativeShell()) return () => {};
  plugin = await pushPlugin();
  // No plugin is an APK built before this seam existed. It keeps working
  // in every other way; it just cannot be woken.
  if (plugin === null) return () => {};

  if (!(await ensurePermission(plugin))) return () => {};
  if (!(await ensureToken(plugin))) return () => {};

  await registerEverywhere();

  removeListeners = [];
  // A rotated token is the one event that invalidates what every backend
  // was told, so it re-registers all of them rather than tracking which.
  const refresh = await plugin.addListener('tokenRefresh', (event) => {
    if (event.token === '' || event.token === token) return;
    token = event.token;
    void registerEverywhere();
  });
  removeListeners.push(refresh.remove);

  // A backend attached from Settings after boot has never been told this
  // token. Re-registering everywhere is one RPC per backend on an event
  // that happens when a person adds a machine, and it removes the need to
  // remember which ones already know.
  installed = [onBackendsChanged(() => void registerEverywhere())];

  return stopPushRegistration;
}

/** Drop every subscription and forget this launch's token. */
export function stopPushRegistration(): void {
  for (const cancel of installed) cancel();
  installed = [];
  for (const remove of removeListeners) void remove();
  removeListeners = [];
  plugin = null;
  token = '';
  refusedThisBoot = false;
}

/**
 * Stop one backend waking this phone.
 *
 * **Called BEFORE the socket closes**, which is why it is a step in
 * `transport/backendAttach.ts`'s removal door rather than something this
 * module watches for: the registration lives on that backend, and the
 * only way to withdraw it is over the connection being taken away. A
 * device that has already dropped the socket has no way to say "stop
 * waking me", and the backend would keep sending until the token died of
 * old age.
 *
 * Never throws. A backend that is unreachable at the moment it is
 * detached cannot be told anything, and failing the detach over it would
 * leave the person with a machine they cannot remove.
 */
export async function unregisterPushFrom(backend: BackendKey): Promise<void> {
  if (!isNativeShell()) return;
  try {
    await withBackendTarget(backend, () => UnregisterPushToken());
  } catch (err) {
    console.warn('push: could not withdraw this phone from a backend being detached', err);
  }
}

/**
 * The tap route this launch started with, plus every later tap.
 *
 * Both doors, because they are different launches of the same gesture: a
 * tap on a RUNNING app arrives as an event, and a tap that woke a DEAD
 * one arrives as the intent the process started with — long before any
 * listener exists, which is why the native side holds it until asked.
 *
 * The target is parsed by the caller, not here: this module has no
 * opinion about routes, and the page's own `parseNotificationTarget` is
 * the one reader of that shape.
 */
export async function watchPushTaps(onTap: (target: unknown) => void): Promise<() => void> {
  if (!isNativeShell()) return () => {};
  const bridge = plugin ?? (await pushPlugin());
  if (bridge === null) return () => {};

  const deliver = (tap: Partial<PushTap>): void => {
    const raw = tap.target ?? '';
    if (raw === '') return;
    try {
      onTap(JSON.parse(raw));
    } catch (err) {
      console.warn('push: a tap carried a route this build could not read', err);
    }
  };

  const live = await bridge.addListener('tap', deliver);
  try {
    deliver(await bridge.takePendingTap());
  } catch (err) {
    console.warn('push: this launch could not say whether it started from a tap', err);
  }
  return () => {
    void live.remove();
  };
}

/**
 * Ask once per boot. A refusal is remembered for the launch, so nothing
 * below asks again and no loop can turn into a prompt storm.
 */
async function ensurePermission(bridge: PushNotificationPlugin): Promise<boolean> {
  if (refusedThisBoot) return false;
  try {
    const answer = await bridge.requestPermission();
    if (!answer.granted) {
      refusedThisBoot = true;
      return false;
    }
    return true;
  } catch (err) {
    console.warn('push: the notification permission could not be asked for', err);
    refusedThisBoot = true;
    return false;
  }
}

/**
 * The registration token, or false when this build has no Firebase
 * configuration.
 *
 * `configured: false` is not an error and is not logged as one: it is the
 * state of every build made without the owner's configuration file, and
 * saying so once at info level is the whole of what a person needs.
 */
async function ensureToken(bridge: PushNotificationPlugin): Promise<boolean> {
  try {
    const answer = await bridge.getToken();
    if (!answer.configured) {
      console.info('push: this build carries no push configuration; this phone will not be woken');
      return false;
    }
    if (answer.token === '') return false;
    token = answer.token;
    return true;
  } catch (err) {
    console.warn('push: this phone could not obtain a registration token', err);
    return false;
  }
}

/**
 * Tell every attached backend. One call each, routed per backend, because
 * a registration belongs to the machine that will do the waking — and the
 * DEVICE it registers is the one behind that connection's own session, so
 * the call carries no device id at all.
 */
async function registerEverywhere(): Promise<void> {
  if (token === '') return;
  for (const entry of attachedBackends()) {
    try {
      await withBackendTarget(entry.id, () => RegisterPushToken(PLATFORM, token));
    } catch (err) {
      // One unreachable backend must not stop the others being told.
      console.warn(`push: ${entry.id} did not record this phone`, err);
    }
  }
}

/** Test seam: what this module believes it registered. */
export function __pushTokenForTest(): string {
  return token;
}
