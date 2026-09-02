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
  return (mod?.App as AppPlugin | null) ?? null;
}

export async function biometricPlugin(): Promise<BiometricPlugin | null> {
  const mod = await loadModule(() => import('@aparajita/capacitor-biometric-auth'));
  return (mod?.BiometricAuth as BiometricPlugin | null) ?? null;
}

export async function scannerPlugin(): Promise<ScannerPlugin | null> {
  const mod = await loadModule(() => import('@capacitor/barcode-scanner'));
  return (mod?.CapacitorBarcodeScanner as ScannerPlugin | null) ?? null;
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
    return register('Bundle');
  } catch (err) {
    console.warn('native: the bundle plugin did not register', err);
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
