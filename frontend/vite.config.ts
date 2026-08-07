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
const transportShim = resolve(__dirname, "src/lib/transport/runtime.ts");

export default defineConfig({
  resolve: {
    alias: {
      "@wailsio/runtime": transportShim,
    },
  },
  plugins: [tailwindcss(), svelte()],
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
    sourcemap: false,
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
              name: "capture-vendor",
              test: /node_modules\/modern-screenshot\//,
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
            // svelte-streamdown ships a sizeable amount of code
            // (Streamdown core + marked + Element renderers + token
            // utilities) and only the chat surface uses it. Splitting
            // it off keeps the main `index.js` bundle from ballooning
            // on initial load and means library upgrades don't bust
            // unrelated chunks.
            //
            // `vendor/` is in the alternation because svelte-streamdown
            // is vendored in-repo (`frontend/vendor/svelte-streamdown`,
            // see its VENDOR.md). Rolldown resolves the pnpm symlink to
            // its real path, so the module ids are `vendor/...`, not
            // `node_modules/...` — matching only the latter silently
            // dumps the whole library back into the entry chunk.
            {
              name: "markdown-vendor",
              test: /(?:node_modules|vendor)\/(svelte-streamdown|marked|idiomorph)\//,
            },
          ],
        },
      },
    },
  },
});
