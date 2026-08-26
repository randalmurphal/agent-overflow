import { mount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installBrowserHistoryGuard } from './lib/utils/browserHistoryGuard';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
import type { MemoryReport } from './lib/utils/memoryReport';
import type { RevealDrainSummary } from './lib/utils/revealDrainProbe';

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
// stub shape as the memory report above, and for the same reason. Unlike
// the UI_TRACE-gated diagnostics globals this one is installed in EVERY
// build: a harness binary ships with UI_TRACE unset, and it is exactly the
// build a bench runs against.
(window as Window & { __aoRevealDrain?: () => Promise<RevealDrainSummary> }).__aoRevealDrain = () =>
  import('./lib/utils/revealDrainProbe').then((m) => m.revealDrainStats());

mount(App, { target: document.getElementById('app')! });
