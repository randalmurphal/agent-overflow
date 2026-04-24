import { defineConfig } from "vite";
import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  build: {
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
              test: /node_modules\/(lucide-svelte|@wailsio)\//,
            },
          ],
        },
      },
    },
  },
});
