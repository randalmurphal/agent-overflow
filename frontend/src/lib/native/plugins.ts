// The Capacitor plugins the shell uses, loaded lazily and never
// statically.
//
// One module owns the imports so the alias story is stated once and the
// seams beside it read as ordinary code. Every loader answers null off the
// shell, so a caller writes `const app = await appPlugin(); if (!app)
// return;` and its web fallback is that early return rather than a
// branch on a platform name.
//
// **Dynamic, and guarded before the import is issued.** A static import
// would pull the plugin into the startup chunk of every build — fetched,
// parsed and evaluated on every desktop launch — which is the same
// bundling argument `lib/architecture.test.ts` rule 4 makes about the
// harness bridge. The guard is `isNativeShell()`, so on the desktop the
// import is not merely resolved to a stub, it never runs.
//
// **A failed load is null, not a throw.** These are optional capabilities
// on a platform that can decline them (a plugin the APK was built
// without, a bridge that has not finished installing). A seam that threw
// would take down whatever called it — which for the lifecycle seam is
// `main.ts`, before anything has mounted that could show an error.

import { isNativeShell } from './platform';

/** The Capacitor App plugin: pause/resume, the hardware back button. */
export interface AppPlugin {
  addListener(
    event: 'pause' | 'resume',
    handler: () => void,
  ): Promise<{ remove: () => Promise<void> }>;
  addListener(
    event: 'backButton',
    handler: (info: { canGoBack: boolean }) => void,
  ): Promise<{ remove: () => Promise<void> }>;
  exitApp(): Promise<void>;
}

/** The platform's biometric / device-credential prompt. */
export interface BiometricPlugin {
  authenticate(options: {
    reason?: string;
    allowDeviceCredential?: boolean;
    cancelTitle?: string;
    androidTitle?: string;
  }): Promise<void>;
}

/** The camera QR scanner. */
export interface ScannerPlugin {
  scanBarcode(options: { hint: number }): Promise<{ ScanResult: string }>;
}

/** One file as the backend's manifest describes it. Mirrors `bundle.File`. */
export interface BundleManifestFile {
  path: string;
  sha256: string;
  size: number;
}

/** A backend's bundle manifest, as `GET /bundle/manifest.json` answers it. */
export interface BundleManifest {
  id: string;
  version: string;
  minShellBuild: number;
  files: BundleManifestFile[];
}

/** What the shell is running, as the native store recorded it. */
export interface BundleState {
  /** The id being served, or '' when that is the APK's own assets. */
  current: string;
  /** A staged bundle the next cold start will adopt, or ''. */
  next: string;
  /** The id this launch has not yet confirmed healthy, or ''. */
  pendingHealth: string;
  /** The last id that reached `ready()`, or ''. */
  lastKnownGood: string;
  /** Ids that failed their first boot. Never downloaded again. */
  rolledBack: string[];
  /** The APK's own `versionCode`. 0 when the platform could not say. */
  versionCode: number;
}

/**
 * The in-app `Bundle` plugin: stage a downloaded bundle, confirm this
 * launch healthy, read what is installed.
 *
 * The only plugin here with no npm package — it is Java in
 * `mobile/android/app/src/main/java/dev/agentoverflow/app/`, registered
 * by `MainActivity`, so the JS half is a `registerPlugin` call rather
 * than an import (mobile/AGENTS.md § The bundle plugin).
 */
export interface BundleSyncPlugin {
  stage(options: {
    id: string;
    manifest: BundleManifest;
    archiveBase64: string;
  }): Promise<void>;
  ready(): Promise<void>;
  state(): Promise<BundleState>;
}

/** One notification, as the tray renders it. Mirrors `notify.Send`. */
export interface PushPresentation {
  id: string;
  kind: string;
  title: string;
  body: string;
  /** The tap route, as its own JSON document. Opaque to the shell. */
  target: string;
}

/** A tap that reached the app, from the launch intent or a live one. */
export interface PushTap {
  id: string;
  /** The same JSON document the presentation carried. */
  target: string;
}

/**
 * The in-app `Push` plugin: the permission, the registration token, the
 * tray, and the tap.
 *
 * Local Java like `Bundle` is, and for a stronger reason: the published
 * push plugin renders only Google-composed notifications and cannot
 * cancel one, and cancelling is half of this feature (mobile/AGENTS.md
 * § Push).
 */
export interface PushNotificationPlugin {
  requestPermission(): Promise<{ granted: boolean }>;
  /**
   * `configured` is false on a build with no Firebase configuration file,
   * which is a fact about the APK rather than a failure — hence a value
   * and not a rejection.
   */
  getToken(): Promise<{ configured: boolean; token: string }>;
  present(options: PushPresentation): Promise<void>;
  retract(options: { id: string }): Promise<void>;
  /** The tap this launch started with, once. `{}` when there was none. */
  takePendingTap(): Promise<Partial<PushTap>>;
  addListener(
    event: 'tap',
    handler: (tap: PushTap) => void,
  ): Promise<{ remove: () => Promise<void> }>;
  addListener(
    event: 'tokenRefresh',
    handler: (event: { token: string }) => void,
  ): Promise<{ remove: () => Promise<void> }>;
}

/**
 * Hide `then` from a Capacitor plugin proxy.
 *
 * `registerPlugin` answers EVERY property read with a method wrapper, so
 * the proxy is a thenable whose `then` rejects with `"App.then()" is not
 * implemented`. Resolving a promise with it — which is what every
 * `async` accessor below does by returning it — makes the promise
 * machinery call that `then` with its own resolvers, and the outer
 * promise then never settles: `await appPlugin()` hung forever and took
 * the lifecycle, the lock prompt and the bundle health check with it
 * (first device run, 2026-09-03). Nothing else about the proxy changes;
 * every other read forwards to it.
 */
export function unthenable<T extends object>(plugin: T): T {
  return new Proxy(plugin, {
    get: (target, prop) => (prop === 'then' ? undefined : Reflect.get(target, prop, target)),
  });
}

async function loadModule<T>(load: () => Promise<T>): Promise<T | null> {
  if (!isNativeShell()) return null;
  try {
    return await load();
  } catch (err) {
    console.warn('native: a plugin did not load', err);
    return null;
  }
}

export async function appPlugin(): Promise<AppPlugin | null> {
  const mod = await loadModule(() => import('@capacitor/app'));
  const plugin = (mod?.App as AppPlugin | null) ?? null;
  return plugin && unthenable(plugin);
}

export async function biometricPlugin(): Promise<BiometricPlugin | null> {
  const mod = await loadModule(() => import('@aparajita/capacitor-biometric-auth'));
  const plugin = (mod?.BiometricAuth as BiometricPlugin | null) ?? null;
  return plugin && unthenable(plugin);
}

export async function scannerPlugin(): Promise<ScannerPlugin | null> {
  const mod = await loadModule(() => import('@capacitor/barcode-scanner'));
  const plugin = (mod?.CapacitorBarcodeScanner as ScannerPlugin | null) ?? null;
  return plugin && unthenable(plugin);
}

/**
 * The bundle-update plugin, or null off the shell.
 *
 * Registered rather than imported, and TYPE-TESTED before it is called:
 * `registerPlugin` is one of the stub's null exports in every build that
 * is not the shell's, and an APK built before this plugin existed would
 * answer a proxy whose calls reject. Both read as "no bundle sync" and
 * `bundleSync.ts` does nothing, which is the correct behaviour for a
 * phone that simply keeps running the bundle it has.
 */
export async function bundlePlugin(): Promise<BundleSyncPlugin | null> {
  const mod = await loadModule(() => import('@capacitor/core'));
  const register = mod?.registerPlugin as
    | ((name: string) => BundleSyncPlugin)
    | null
    | undefined;
  if (typeof register !== 'function') return null;
  try {
    return unthenable(register('Bundle'));
  } catch (err) {
    console.warn('native: the bundle plugin did not register', err);
    return null;
  }
}

/**
 * The push plugin, or null off the shell.
 *
 * Registered rather than imported, exactly as `bundlePlugin` is and for
 * the same reasons: it is Java in this repo with no npm package, and an
 * APK built before it existed answers a proxy whose calls reject. Both
 * read as "this phone cannot be woken", which `native/push.ts` treats as
 * a normal outcome rather than an error.
 */
export async function pushPlugin(): Promise<PushNotificationPlugin | null> {
  const mod = await loadModule(() => import('@capacitor/core'));
  const register = mod?.registerPlugin as
    | ((name: string) => PushNotificationPlugin)
    | null
    | undefined;
  if (typeof register !== 'function') return null;
  try {
    return unthenable(register('Push'));
  } catch (err) {
    console.warn('native: the push plugin did not register', err);
    return null;
  }
}

/**
 * The scanner's "this is a QR code" hint. Read off the plugin's own enum
 * when it is there and defaulted to its QR_CODE value otherwise, because
 * the enum is a plain number the plugin forwards to the native side and a
 * missing import must not turn into `undefined` on the wire.
 */
export async function qrCodeHint(): Promise<number> {
  const mod = await loadModule(() => import('@capacitor/barcode-scanner'));
  const hint = (mod?.CapacitorBarcodeScannerTypeHint as { QR_CODE?: number } | null)?.QR_CODE;
  return typeof hint === 'number' ? hint : 0;
}
