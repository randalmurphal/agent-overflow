// What `@capacitor/app`, `@capacitor/barcode-scanner`,
// `@aparajita/capacitor-biometric-auth` and `@capacitor/core` resolve to
// in every build that is not the shell's.
//
// The Capacitor packages are dependencies of `mobile/`, not of
// `frontend/`, because the desktop app must not carry them and
// `pnpm install` in this directory must not fetch an Android toolchain's
// worth of JS to build a webview bundle. But `./plugins.ts` names those
// specifiers in a dynamic `import()`, and a specifier a bundler cannot
// resolve is a build error rather than a runtime null.
//
// So `vite.config.ts` aliases all four at THIS file by default and at
// `mobile/node_modules/...` when `AO_SHELL=1` — which is what
// `mobile/scripts/build-apk.sh` sets, and the only build that does. The
// alias is the mechanism rather than an `optionalDependencies` entry or
// an `import.meta.glob` because it is decidable: exactly one of the two
// resolutions happens, it happens at config time, and nothing about it
// depends on whether an install step happened to succeed.
//
// Nothing here is ever CALLED. `./plugins.ts` asks `isNativeShell()`
// first, which is false in every build this file is part of, so the
// dynamic import is not even issued. The exports exist so the module has
// the shape the importer destructures — a named export a bundler cannot
// find is the same build error the unresolvable specifier was.

/** The Capacitor App plugin, absent outside the shell. */
export const App = null;

/** The biometric plugin, absent outside the shell. */
export const BiometricAuth = null;

/** The barcode scanner plugin, absent outside the shell. */
export const CapacitorBarcodeScanner = null;

/** The scanner's hint enum, absent outside the shell. */
export const CapacitorBarcodeScannerTypeHint = null;

/**
 * The bridge's plugin registry, absent outside the shell.
 *
 * `@capacitor/core` is aliased here for the one plugin that has no npm
 * package: `Bundle` is Java inside `mobile/android/`, so its JS side is
 * `registerPlugin('Bundle')` rather than an import. Null for the same
 * reason as the rest — `plugins.ts` type-tests it before calling, so a
 * build that resolved here answers "no plugin" rather than throwing.
 */
export const registerPlugin = null;
