import { mount } from 'svelte';
import App from './App.svelte';
import { appTitleForEnv } from './appTitle';
import { installFrontendErrorCapture } from './lib/utils/frontendErrorCapture';
import { installZombieMintProbe } from './lib/utils/zombieMintProbe';

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
// Receiver for the patched Svelte runtime's leak probe — must exist
// before the first render so mount-time mints are captured.
installZombieMintProbe();

mount(App, { target: document.getElementById('app')! });
