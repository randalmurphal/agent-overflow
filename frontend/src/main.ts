import { mount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installBrowserHistoryGuard } from './lib/utils/browserHistoryGuard';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
import type { MemoryReport } from './lib/utils/memoryReport';

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

mount(App, { target: document.getElementById('app')! });
