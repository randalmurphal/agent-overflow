import { defineConfig, configDefaults } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { playwright } from '@vitest/browser-playwright';
import { resolve } from 'node:path';

// Shared resolve config for the happy-dom suite. Use the browser entry for
// Svelte so `mount()` is available in tests (without this Vitest resolves
// svelte's `default` export -> index-server.js -> lifecycle_function_unavailable).
// Aliases `@wailsio/runtime` and the generated Wails bindings to deterministic
// fakes in `src/test/mocks/`.
const happyDomResolve = {
  conditions: ['browser'],
  alias: [
    { find: '@wailsio/runtime', replacement: resolve(__dirname, 'src/test/mocks/wailsio-runtime.ts') },
    // Matches the relative path that lib/stores/bindings.ts imports from.
    // Both the worktree path and any nested depth resolve to the mock.
    { find: '../../../bindings/agent-overflow/app.js', replacement: resolve(__dirname, 'src/test/mocks/bindings-app.ts') },
    { find: '../../../bindings/agent-overflow/internal/provider/models.js', replacement: resolve(__dirname, 'src/test/mocks/bindings-models.ts') },
    // Vitest's loader can't parse raw CSS imported outside of a Svelte
    // `<style>` block -- `svelte-streamdown`'s Math element does
    // `import 'katex/dist/katex.min.css'`, which would crash component tests
    // with "Unknown file extension '.css'". Stub it explicitly.
    { find: 'katex/dist/katex.min.css', replacement: resolve(__dirname, 'src/test/mocks/empty-css.ts') },
    // auto-animate's element poller leaves untracked node timers running
    // after environment teardown (setInterval -> requestAnimationFrame on
    // a torn-down happy-dom global), which flakes the suite with unhandled
    // ReferenceErrors under load. Animations don't matter in tests — stub
    // the module. See src/test/mocks/auto-animate.ts.
    { find: '@formkit/auto-animate', replacement: resolve(__dirname, 'src/test/mocks/auto-animate.ts') },
  ],
};

// Three projects:
//  - `unit`: the default happy-dom component/store suite. Deliberately does NOT
//    load tailwindcss -- those tests render against mocked bindings and don't
//    exercise styles, and tailwind's plugin resolves project-level paths we'd
//    otherwise have to stub.
//  - `browser`: real-Chromium layout suite. happy-dom reports zero geometry, so
//    pixel invariants (a row's trailing margin must stay inside its content box;
//    markdown rows stay flush) can only be verified with a real layout engine.
//    Loads tailwindcss so the test can import the production app.css and assert
//    against the real cascade. Files matched by `*.browser.test.ts`.
//  - `manual`: operator-driven investigation drivers, matched by `*.manual.ts`.
//    They need a locally generated corpus that cannot be committed, and a
//    genuine finding WEDGES THE WORKER rather than failing an assertion —
//    which is the repro signal, and precisely why they must never be
//    collectable by a gate. A separate project (rather than a naming
//    convention alone) is what makes that structural: `*.manual.ts` matches
//    neither other project's include glob, and no project runs unless it is
//    named.
//
// The default `pnpm test` runs ONLY the unit project, so the `make test` /
// `make verify` gate needs no browser binary (`make install` does not provision
// one). Run the browser suite explicitly with `pnpm test:browser`, which needs
// `pnpm exec playwright install chromium`; run a manual driver with
// `pnpm test:manual`.
export default defineConfig({
  test: {
    projects: [
      {
        plugins: [svelte()],
        resolve: happyDomResolve,
        test: {
          name: 'unit',
          environment: 'happy-dom',
          environmentOptions: {
            happyDOM: {
              settings: {
                navigation: {
                  // Component tests assert iframe attributes and mocked
                  // postMessage behavior; they do not need happy-dom to perform
                  // real /design/... iframe navigations. Letting those fetches
                  // run leaves aborted async tasks behind during cleanup and
                  // floods stderr with teardown noise.
                  disableChildFrameNavigation: true,
                },
              },
            },
          },
          globals: false,
          setupFiles: ['./src/test/setup.ts'],
          include: ['src/**/*.{test,spec}.{ts,js}'],
          // Real-browser layout tests run in the `browser` project below.
          exclude: [...configDefaults.exclude, 'src/**/*.browser.{test,spec}.{ts,js}'],
          // Svelte 5 component imports are ESM-first; keep transforms minimal.
          server: {
            deps: {
              inline: [/@testing-library\/svelte/, /svelte/],
            },
          },
        },
      },
      {
        // Mounting real Svelte components (MessageTimeline + rows) in the
        // browser project needs the svelte plugin AND the same bindings/runtime
        // aliases the unit project uses -- otherwise component imports of the
        // generated Wails bindings resolve to the real transport client and the
        // mount throws. tailwindcss stays so app.css compiles against the real
        // cascade. Order: svelte first so .svelte transforms run before tailwind
        // post-processes the emitted CSS.
        plugins: [svelte(), tailwindcss()],
        resolve: happyDomResolve,
        // Scan the browser test files (and their component imports) up front
        // so Vite's dep optimizer pre-bundles everything they reach. Without
        // this, deps discovered mid-run (@testing-library/svelte from the
        // setup file; lucide icons / streamdown / idiomorph from mounting the
        // real MessageTimeline) trigger a re-optimize + full reload that
        // fails collection ("failed to find the current suite") on the first
        // run after a cache wipe.
        optimizeDeps: {
          include: [
            '@testing-library/svelte',
            '@lucide/svelte/icons/circle',
            '@lucide/svelte/icons/circle-check',
          ],
          entries: [
            'src/**/*.browser.{test,spec}.{ts,js}',
            'src/test/setup.browser.ts',
          ],
        },
        test: {
          name: 'browser',
          include: ['src/**/*.browser.{test,spec}.{ts,js}'],
          // Chromium needs none of setup.ts's happy-dom polyfills — only the
          // cross-test resets of module-level stores/caches (see the file).
          setupFiles: ['./src/test/setup.browser.ts'],
          browser: {
            enabled: true,
            provider: playwright(),
            headless: true,
            instances: [{ browser: 'chromium' }],
            // These are geometry assertions on invisible off-screen divs; a
            // failure screenshot is useless and would litter the tree with
            // `.vitest-attachments/` + `__screenshots__/` byproducts.
            screenshotFailures: false,
          },
        },
      },
      {
        // Same environment as `unit` — the drivers mount real components and
        // reach the same mocked bindings — with a disjoint include glob and a
        // timeout that lets a long sweep finish rather than being cut off
        // mid-corpus.
        plugins: [svelte()],
        resolve: happyDomResolve,
        test: {
          name: 'manual',
          environment: 'happy-dom',
          globals: false,
          setupFiles: ['./src/test/setup.ts'],
          include: ['src/**/*.manual.ts'],
          testTimeout: 300_000,
          server: {
            deps: {
              inline: [/@testing-library\/svelte/, /svelte/],
            },
          },
        },
      },
    ],
  },
});
