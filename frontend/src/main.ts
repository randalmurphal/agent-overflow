import { mount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installBrowserHistoryGuard } from './lib/utils/browserHistoryGuard';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
import { maybeInstallZombieMintProbe } from './lib/utils/zombieMintProbe';

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
// Receiver for the patched Svelte runtime's leak probe. The probe is
// intentionally opt-in because the global callback itself shows up as
// a heap root in production/debug snapshots; enable with
// VITE_AGENT_OVERFLOW_ZOMBIE_MINT_PROBE=1 when re-rolling the Svelte
// patch or investigating a suspected regression.
maybeInstallZombieMintProbe();

mount(App, { target: document.getElementById('app')! });
