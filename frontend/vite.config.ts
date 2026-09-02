import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { resolve } from "node:path";

// Phase B aliases @wailsio/runtime to the local transport shim. The shim
// exposes the same surface the generated bindings depend on (Call.ByID,
// CancellablePromise, Create.* factories, Events.On/Emit) but routes
// every call through the WebSocket transport in /internal/transport.
// Generated bindings remain byte-identical — this alias is the only
// change at the build layer required to swap Wails native IPC for the
// localhost WS server.
const transportShim = resolve(import.meta.dirname, "src/lib/transport/runtime.ts");

// The three Capacitor plugins the phone shell uses are dependencies of
// `mobile/`, not of this package: the desktop app must not carry them,
// and `pnpm install` here must not fetch an Android toolchain's worth of
// JS to build a webview bundle. But `src/lib/native/plugins.ts` names
// those specifiers in a dynamic `import()`, and a specifier a bundler
// cannot resolve is a build error rather than a runtime null.
//
// So they are ALIASED, and the alias is what decides which build this is.
// `AO_SHELL=1` -- set by `mobile/scripts/build-apk.sh` and by nothing
// else -- points them at the real packages under `mobile/node_modules`;
// every other build points them at a local stub whose exports are all
// null. The alias is the mechanism rather than `optionalDependencies` or
// an `import.meta.glob` because it is DECIDABLE: exactly one resolution
// happens, it happens at config time, and nothing about it depends on
// whether an install step in another directory succeeded.
//
// Nothing in the stub is ever called. `plugins.ts` asks `isNativeShell()`
// before it issues the import, and that is false in every build the stub
// is part of.
const shellBuild = process.env.AO_SHELL === "1";
const mobileModules = resolve(import.meta.dirname, "..", "mobile", "node_modules");
const capacitorAbsent = resolve(import.meta.dirname, "src/lib/native/capacitorAbsent.ts");
const CAPACITOR_PLUGINS = [
  "@capacitor/app",
  "@capacitor/barcode-scanner",
  "@aparajita/capacitor-biometric-auth",
] as const;
const capacitorAlias = Object.fromEntries(
  CAPACITOR_PLUGINS.map((name) => [
    name,
    shellBuild ? resolve(mobileModules, name) : capacitorAbsent,
  ]),
);

export default defineConfig({
  resolve: {
    alias: {
      "@wailsio/runtime": transportShim,
      ...capacitorAlias,
    },
  },

  // `configFile: false`: there is no `svelte.config.js`. The only one this
  // project ever had suppressed compiler warnings from `vendor/`, and that
  // tree is first-party `src/` code now — every warning in it is ours to
  // fix. Stating the absence keeps the plugin from logging a "no Svelte
  // config found" line on every build, test and dev start.
  plugins: [tailwindcss(), svelte({ configFile: false })],
  server: {
    watch: {
      // Belt-and-braces: `.claude/worktrees/agent-*/` and
      // `.playwright-mcp/` live above this config's project root
      // (frontend/), so Vite's chokidar watcher should not see them
      // anyway. We pin the ignore here so a future config change
      // (e.g. adjusting root or adding fs.allow) can't accidentally
      // pull thousands of worktree files into the watcher and crash
      // the dev server. The Wails3 dev_mode watcher in build/config.yml
      // carries the load-bearing exclude.
      ignored: ['**/.claude/**', '**/.playwright-mcp/**'],
    },
  },
  build: {
    // Production builds ship without sourcemaps. Vite 8 already defaults
    // to false; pinned explicitly so a future default flip can't leak the
    // source map into a shipped binary.
    //
    // AO_SOURCEMAP=1 is the perf-investigation escape hatch: 'hidden'
    // emits `assets/*.js.map` beside the bundles with NO sourceMappingURL
    // comment, so the served JS is byte-identical and DevTools stays
    // unaware — only the perfprobe scripts fetch the maps to resolve
    // profile frames to real file:line (`scripts/perfprobe/lib/
    // sourcemap.mjs`). Never set it for a release build: the maps get
    // embedded into the binary's assets like any other dist file.
    sourcemap: process.env.AO_SOURCEMAP === '1' ? 'hidden' : false,
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            {
              name: "svelte-vendor",
              test: /node_modules\/(svelte|esm-env)\//,
            },
            {
              name: "terminal-vendor",
              test: /node_modules\/(@xterm)\//,
            },
            {
              name: "ui-vendor",
              test: /node_modules\/@lucide\/svelte\//,
            },
            // Transport shim is small but every chat surface imports
            // through it (the @wailsio/runtime alias). Keep it in its
            // own chunk so a future change to the transport doesn't
            // bust the ui-vendor cache for unrelated upgrades.
            {
              name: "transport-vendor",
              test: /src\/lib\/transport\//,
            },
            // The markdown renderer is a sizeable amount of code
            // (Streamdown core + the absorbed marked lexer + element
            // renderers + token utilities) and only the chat surface
            // uses it. Splitting it off keeps the main `index.js`
            // bundle from ballooning on initial load and means upgrades
            // don't bust unrelated chunks.
            //
            // `src/lib/markdown/` is first-party source (the tree used
            // to be a vendored package plus the `marked` dependency; see
            // its LICENSE), so the alternation matches the in-repo path
            // and the one node_modules half it still pulls in. The
            // `src/` alternative deliberately requires the trailing
            // slash so `markdown/` matches the directory, never a
            // sibling file. Verify against `dist/assets` after any
            // edit here: a regex that matches nothing fails silently by
            // folding ~153KB into the entry chunk.
            {
              name: "markdown-vendor",
              test: /(?:node_modules\/idiomorph|src\/lib\/markdown)\//,
            },
          ],
        },
      },
    },
  },
});
