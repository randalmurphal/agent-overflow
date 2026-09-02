import { mount, unmount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installBrowserHistoryGuard } from './lib/utils/browserHistoryGuard';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
import { installStepUpProof } from './lib/transport/stepUp';
import {
  adoptPairingEndpoint,
  installNativeShell,
  prepareNativeShell,
} from './lib/native/boot';
import type { MemoryReport } from './lib/utils/memoryReport';
import {
  revealDrainStats,
  type RevealDrainSummary,
} from './lib/utils/revealDrainProbe';

// Self-hosted fonts. Four weights of each family covers every surface
// the app uses today (body/medium/semibold/bold). Loaded before the
// global stylesheet so the @font-face declarations beat any cascading
// font-family rules.
import '@fontsource/geist-sans/400.css';
import '@fontsource/geist-sans/500.css';
import '@fontsource/geist-sans/600.css';
import '@fontsource/geist-sans/700.css';
import '@fontsource/geist-mono/400.css';
import '@fontsource/geist-mono/500.css';
import '@fontsource/geist-mono/600.css';

import './app.css';

document.title = appTitleForEnv(import.meta.env);

// Install before mount so mount-time exceptions are captured too.
installFrontendErrorCapture();
installBrowserHistoryGuard();
// How a remote screen satisfies a step-up gate. Installed into the
// transport rather than imported by it (the ceremony is itself two RPCs),
// and installed HERE so it covers every `//ao:stepup` method in the app
// from the first call — including the pairing screen's, which mounts
// below without the rest of the app.
installStepUpProof();
// On-demand memory accounting for console / CDP probes. The stub keeps
// the collector chunk out of the startup graph entirely; the dynamic
// import resolves from the module cache on every call after the first.
(window as Window & { __aoMemoryReport?: () => Promise<MemoryReport> }).__aoMemoryReport = () =>
  import('./lib/utils/memoryReport').then((m) => m.collectMemoryReport());
// How much of the reveal queue is still draining, for a bench or a profile
// whose measurement window has to outlast `provider:turn_completed`. Same
// Unlike the memory report, the idle-memory-trim gate already needs this
// module in every desktop build. Call it directly instead of issuing an
// ineffective dynamic import that cannot create a lazy chunk. The global is
// still installed in every build because a harness bench ships with UI_TRACE
// unset.
(window as Window & { __aoRevealDrain?: () => Promise<RevealDrainSummary> }).__aoRevealDrain = () =>
  revealDrainStats();

// A `#pair=` fragment means this page was opened from a pairing link
// (docs/specs/remote-access.md §4): mount the pairing screen instead of
// the app, and boot the app only after the flow finishes. Lazily
// imported so ordinary boots never load it. The fragment never reaches
// the server (fragments don't), and it is stripped before the app
// mounts so a reload after pairing is an ordinary boot.
//
// The phone shell reaches the same screen by a different door — a QR code
// the camera read rather than a URL the browser was navigated to — so the
// pairing mount below is shared, and there is one redemption flow in this
// app rather than one per way of arriving at it.
async function mountApp(): Promise<void> {
  const target = document.getElementById('app')!;
  // The shell's boot, and the only thing in this file that is not the
  // same for every client. It runs before anything mounts, because a
  // phone's home backend is not the origin that served it and the very
  // first fetch of the app's boot fan-out has to be addressed correctly
  // (lib/native/boot.ts, lib/transport/homeEndpoint.ts). A static import:
  // everything it needs is already in App's graph, and a lazy chunk here
  // would cost every desktop boot one round trip for a check that
  // answers "no shell" in a microsecond.
  const shell = prepareNativeShell();

  // The pairing hash is checked FIRST, for every client. A shell can
  // arrive with one too (the emulator smoke navigates to it, and an app
  // link would), and a shell that has never paired learns its endpoint
  // from the payload exactly as the first-run scanner's does.
  if (location.hash.startsWith('#pair=')) {
    const session = await import('./lib/transport/deviceSession');
    let payload: import('./lib/transport/deviceSession').PairingPayload | null = null;
    let parseError = '';
    try {
      payload = session.parsePairingFragment(location.hash);
    } catch (err) {
      parseError = err instanceof Error ? err.message : String(err);
    }
    if (shell.shell && payload !== null) {
      const problem = adoptPairingEndpoint(payload);
      if (problem !== '') {
        payload = null;
        parseError = problem;
      }
    }
    await mountPairing(target, payload, parseError, shell.shell);
    return;
  }

  if (shell.shell && !shell.paired) {
    await mountFirstRun(target);
    return;
  }

  if (shell.shell) {
    await mountUnderLock(target);
    return;
  }
  mount(App, { target });
}

/**
 * The pairing screen, and what happens on the other side of it.
 *
 * `redialAfterPairing` is AWAITED and the screen stays up for it. The app
 * issues its whole boot fan-out on mount, and a transport still
 * mid-redial rejects that fan-out wholesale — which is the burst of
 * errors a freshly paired browser was shown for a pairing that worked
 * (see wsClient.redialAfterPairing). The wait is bounded there, so an
 * unreachable backend still mounts the app and lets its own banner say
 * so.
 */
async function mountPairing(
  target: HTMLElement,
  payload: import('./lib/transport/deviceSession').PairingPayload | null,
  parseError: string,
  shell: boolean,
): Promise<void> {
  const { default: PairingScreen } = await import(
    './lib/components/pairing/PairingScreen.svelte'
  );
  let screen: ReturnType<typeof mount> | null = null;
  screen = mount(PairingScreen, {
    target,
    props: {
      payload,
      parseError,
      onDone: () => {
        history.replaceState(null, '', location.pathname + location.search);
        void (async () => {
          // Any socket opened while this screen was up dialed before the
          // credential existed; the app must attach under the session
          // that was just confirmed. Module cache, not a new chunk —
          // App's static graph already carries the client.
          const { wsClient } = await import('./lib/transport/wsClient');
          await wsClient.redialAfterPairing();
          if (screen) await unmount(screen);
          if (shell) {
            await mountUnderLock(target);
            return;
          }
          mount(App, { target });
        })();
      },
    },
  });
}

/**
 * A phone that has never paired: one screen with one button. What the
 * camera reads is the same payload a `#pair=` fragment carries, and the
 * screen has already set the home endpoint from it by the time this
 * callback runs, so the pairing flow below is the ordinary one.
 */
async function mountFirstRun(target: HTMLElement): Promise<void> {
  const { default: FirstRunScreen } = await import(
    './lib/components/native/FirstRunScreen.svelte'
  );
  let screen: ReturnType<typeof mount> | null = null;
  screen = mount(FirstRunScreen, {
    target,
    props: {
      onScanned: (payload: import('./lib/transport/deviceSession').PairingPayload) => {
        void (async () => {
          if (screen) await unmount(screen);
          screen = null;
          await mountPairing(target, payload, '', true);
        })();
      },
    },
  });
}

/**
 * The lock sits over a MOUNTED app rather than in front of one that has
 * not booted, and both reasons are about what the person sees: the app
 * behind the gate is warm the moment it passes rather than starting its
 * boot fan-out then, and a resume that re-locks does not throw away the
 * thread they were reading.
 *
 * The lock screen is mounted BEFORE the app, so a phone never flashes a
 * transcript on its way to being locked.
 */
async function mountUnderLock(target: HTMLElement): Promise<void> {
  const { default: LockScreen } = await import('./lib/components/native/LockScreen.svelte');
  const overlay = document.createElement('div');
  overlay.id = 'app-lock';
  document.body.appendChild(overlay);

  let lockScreen: ReturnType<typeof mount> | null = null;
  let unlock: () => void = () => {};
  const show = (locked: boolean): void => {
    // The app under the gate is INERT while it is locked: the lock
    // screen paints over it, and inert is what keeps focus, the
    // keyboard and a screen reader from reaching what the paint hides.
    target.inert = locked;
    if (locked && lockScreen === null) {
      lockScreen = mount(LockScreen, { target: overlay, props: { onUnlock: () => unlock() } });
      return;
    }
    if (!locked && lockScreen !== null) {
      void unmount(lockScreen);
      lockScreen = null;
    }
  };

  show(true);
  mount(App, { target });
  const lock = await installNativeShell(show);
  unlock = () => void lock.unlock();
  // A shell whose platform offers no gate at all answers unlocked, and
  // the screen has to come down for it — otherwise a phone with no
  // biometric plugin would be a permanent lock screen.
  show(lock.locked());
}

void mountApp();
