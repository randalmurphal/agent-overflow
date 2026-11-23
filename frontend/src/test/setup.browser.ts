// Per-test resets for the real-Chromium `browser` vitest project — the
// minimal subset of setup.ts that browser tests actually need. Chromium
// implements everything setup.ts polyfills for happy-dom (ResizeObserver,
// element.animate, execCommand, matchMedia, localStorage), so none of
// those shims belong here; importing them would paper over the real
// engine the project exists to exercise.
//
// Module-level state between tests is reset by the registry, as in the
// happy-dom projects (config/testResets.ts). What stays here is the
// browser's own posture: real storage, fonts and the page focus outlive a
// test.
import { resetRegistry } from './resetRegistry';
import { beforeAll, beforeEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import { SHIPPED_FONT_FACES } from '../fonts';
import './helpers/firstDivergence';
import { resetFrontendPreferencesForTest } from '../lib/stores/frontendPreferences.svelte';
import { resetClientDeviceNameForTest } from '../lib/stores/clientDeviceName.svelte';
import { writeFrontendValue } from '../lib/stores/frontendStorage';
import { installWaitForBackoff } from './waitForBackoff';

// The browser project pre-bundles testing-library, so its module carries no
// registration of its own; hand its cleanup to the registry under the same key.
resetRegistry.loaded('@testing-library/svelte/src/pure.js', { cleanup }, []);

installWaitForBackoff();

// Layout assertions measure text. Without the app's own faces they would
// measure whatever fallback font the host has installed, which differs by
// platform. @font-face loads lazily, so load every shipped face up front.
beforeAll(async () => {
  const loaded = await Promise.all(SHIPPED_FONT_FACES.map((face) => document.fonts.load(face)));
  const missing = SHIPPED_FONT_FACES.filter((_, index) => loaded[index].length === 0);
  if (missing.length > 0) throw new Error(`Shipped fonts did not load: ${missing.join(', ')}`);
});

beforeEach(() => {
  // The page focus outlives a test and its file's frame. The app reads it
  // (document.hasFocus() in VirtualList's focus keeping, the clipboard and
  // screen presence), so each test starts with its frame focused, whatever
  // a prior test or file left focused.
  window.focus();
  if (!document.hasFocus()) throw new Error('setup.browser.ts: window.focus() did not give the test frame the page focus');
  // Real browser storage survives between files; backend settings only seed
  // missing preferences and cannot undo a prior fixture's local UI choices.
  writeFrontendValue('preferences', {});
  resetFrontendPreferencesForTest({});
  writeFrontendValue('device-name', null);
  resetClientDeviceNameForTest();
});
