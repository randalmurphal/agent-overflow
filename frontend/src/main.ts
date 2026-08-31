import { mount, unmount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installBrowserHistoryGuard } from './lib/utils/browserHistoryGuard';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
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
async function mountApp(): Promise<void> {
  const target = document.getElementById('app')!;
  if (location.hash.startsWith('#pair=')) {
    const [{ default: PairingScreen }, session] = await Promise.all([
      import('./lib/components/pairing/PairingScreen.svelte'),
      import('./lib/transport/deviceSession'),
    ]);
    let payload: import('./lib/transport/deviceSession').PairingPayload | null = null;
    let parseError = '';
    try {
      payload = session.parsePairingFragment(location.hash);
    } catch (err) {
      parseError = err instanceof Error ? err.message : String(err);
    }
    let screen: ReturnType<typeof mount> | null = null;
    screen = mount(PairingScreen, {
      target,
      props: {
        payload,
        parseError,
        onDone: () => {
          history.replaceState(null, '', location.pathname + location.search);
          void (async () => {
            // Any socket opened while this screen was up dialed before
            // the credential existed; the app must attach under the
            // session that was just confirmed. Module cache, not a new
            // chunk — App's static graph already carries the client.
            //
            // AWAITED, and the screen stays up for it. The app issues
            // its whole boot fan-out on mount, and a transport still
            // mid-redial rejects that fan-out wholesale — which is the
            // burst of errors a freshly paired browser was shown for a
            // pairing that worked (see wsClient.redialAfterPairing). The
            // wait is bounded there, so an unreachable backend still
            // mounts the app and lets its own banner say so.
            const { wsClient } = await import('./lib/transport/wsClient');
            await wsClient.redialAfterPairing();
            if (screen) await unmount(screen);
            mount(App, { target });
          })();
        },
      },
    });
    return;
  }
  mount(App, { target });
}

void mountApp();
