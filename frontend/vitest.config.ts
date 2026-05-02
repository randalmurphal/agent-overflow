import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { resolve } from 'node:path';

// Test-only config. Intentionally does NOT load the tailwindcss plugin -- tests
// render components against mocked wails bindings and don't exercise styles,
// and tailwind's vite plugin resolves project-level paths we'd otherwise have
// to stub. Aliases `@wailsio/runtime` and the generated Wails bindings to
// deterministic fakes in `src/test/mocks/`.
export default defineConfig({
  plugins: [svelte()],
  resolve: {
    // Use the browser entry for Svelte so `mount()` is available in tests.
    // Without this Vitest would resolve `svelte`'s `default` export, which
    // points at index-server.js and throws lifecycle_function_unavailable.
    conditions: ['browser'],
    alias: [
      { find: '@wailsio/runtime', replacement: resolve(__dirname, 'src/test/mocks/wailsio-runtime.ts') },
      // Matches the relative path that lib/stores/bindings.ts imports from.
      // Both the worktree path and any nested depth resolve to the mock.
      { find: '../../../bindings/agent-overflow/app.js', replacement: resolve(__dirname, 'src/test/mocks/bindings-app.ts') },
      { find: '../../../bindings/agent-overflow/internal/provider/models.js', replacement: resolve(__dirname, 'src/test/mocks/bindings-models.ts') },
      // Vitest's loader can't parse raw CSS imported outside of a
      // Svelte `<style>` block — `svelte-streamdown`'s Math element
      // does `import 'katex/dist/katex.min.css'`, which would crash
      // component tests with "Unknown file extension '.css'". Stub it
      // explicitly. (Other CSS imports flow through vite-plugin-svelte
      // or `?raw`, which are handled correctly already.)
      { find: 'katex/dist/katex.min.css', replacement: resolve(__dirname, 'src/test/mocks/empty-css.ts') },
    ],
  },
  test: {
    environment: 'happy-dom',
    globals: false,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.{test,spec}.{ts,js}'],
    // Svelte 5 component imports are ESM-first; keep transforms minimal.
    server: {
      deps: {
        inline: [/@testing-library\/svelte/, /svelte/],
      },
    },
  },
});
