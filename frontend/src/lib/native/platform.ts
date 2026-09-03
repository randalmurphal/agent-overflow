// Whether this bundle is running inside the phone shell.
//
// The one question the native seams in this directory branch on, and it
// is answered by the RUNTIME rather than by a build flag: the same bundle
// ships to the desktop, to `--connect`, to a paired browser and inside
// the APK, and only the last of those has a Capacitor bridge in the page.
// A build flag would need the SPA to be built twice for reasons that have
// nothing to do with what it renders.
//
// Every read is a feature test with optional chaining all the way down —
// `window.Capacitor` is injected by the native bridge and is simply
// absent everywhere else, and an absent property is a TypeError when it
// is called rather than a degraded feature (the `crypto.randomUUID`
// class; see `transport/deviceKey.ts`).

interface CapacitorBridge {
  isNativePlatform?: () => boolean;
  getPlatform?: () => string;
}

function bridge(): CapacitorBridge | undefined {
  if (typeof window === 'undefined') return undefined;
  return (window as Window & { Capacitor?: CapacitorBridge }).Capacitor;
}

/**
 * True only inside the native shell. False in every browser, including
 * one on a phone: what this asks is whether there is a native side to
 * talk to, never how wide the viewport is. Layout is `stores/layoutMode`'s
 * question and is deliberately answered from the viewport instead
 * (`docs/specs/remote-access.md` § "Layout mode, not device class").
 */
export function isNativeShell(): boolean {
  try {
    return bridge()?.isNativePlatform?.() === true;
  } catch {
    return false;
  }
}

/** `'android'`, `'ios'`, or `'web'` when there is no native side. */
export function nativePlatform(): string {
  try {
    return bridge()?.getPlatform?.() ?? 'web';
  } catch {
    return 'web';
  }
}
