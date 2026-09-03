import { defineConfig } from '@playwright/test';

// The emulator smoke's own config, separate from `playwright.config.ts`
// for one reason: nothing in `android/` may ever be picked up by the
// `desktop` or `compact` projects. Those two run a Chromium this machine
// launches; this one drives a WebView inside a device that is already
// running, and a spec written for one is nonsense under the other.
//
// **No `projects`, and no browser.** The `page` fixture here does not come
// from a browser Playwright launched — `android/shell-boot.spec.ts` builds
// it out of the shell's own WebView (`_android` → `device.webView()`), so
// the `use` block below deliberately names no device descriptor, no
// viewport and no browser channel. The layout is whatever the emulator's
// screen is, which is the point: the compact layout is chosen from the
// viewport (frontend/AGENTS.md § Compact), so the phone picks it itself.
//
// **One worker, no retries.** There is one device, one app install and one
// device PIN, all of them global to the emulator; a second worker would be
// two suites driving one phone. A retry would re-run against an app that
// is already paired and already unlocked, which is a different test.
//
// The timeouts are the emulator's, not a laptop's: a cold WebView, a
// pairing exchange over `adb reverse`, and the platform's own credential
// prompt all sit inside the single test.
export default defineConfig({
  testDir: './android',
  timeout: 300_000,
  expect: { timeout: 30_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: 'list',
  use: {
    // Both would attach to a browser context this config never creates.
    trace: 'off',
    screenshot: 'off',
  },
});
