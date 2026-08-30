#!/usr/bin/env node
// Scoped typecheck of a few TypeScript files, for a tight edit loop.
//
//   cd frontend
//   pnpm run check:file src/lib/stores/thread.svelte.ts src/lib/markdown/boundary/index.ts
//
// `pnpm run check` builds a program over all 5965 files and takes ~20s.
// While you are iterating on three of them that is the whole loop. This
// builds a program over just those three plus what they import, which
// measured at ~2s.
//
// ── What it covers, exactly ───────────────────────────────────────────────
//
// It runs `tsc` — NOT svelte-check — over a generated tsconfig that
// `extends` the real one, so every compiler option (strict,
// noUnusedLocals, verbatimModuleSyntax, the type roots) is identical.
// Errors in the files you named and in any `.ts` they pull in are real
// errors, reported the same way `pnpm run check` reports them.
//
// Two things it does NOT do, and cannot:
//
//   1. `.svelte` files. svelte-check has no per-file or per-module mode:
//      `--workspace <dir>` is the finest scope it offers, and it is not
//      even a speedup — narrowing to `src/lib/markdown` MEASURED SLOWER
//      than the whole project (23s vs 20s), because the cost is building
//      the TypeScript program, not running the diagnostics. There is no
//      scoped `.svelte` check to ship, so this script refuses `.svelte`
//      arguments rather than pretending.
//
//   2. Svelte component TYPES imported into a `.ts` file. Those resolve
//      through svelte's ambient `declare module '*.svelte'`, so the
//      import succeeds and the props are not checked. `pnpm run check` is
//      the only thing that checks them.
//
// So: fast loop here, `pnpm run check` before you call it done. The
// header printed on every run says the same thing, because a tool that
// silently checks less than you think it does is worse than no tool.
//
// ── Why the extra files ───────────────────────────────────────────────────
//
// A subset program silently drops every ambient declaration the omitted
// files carried, and TypeScript reports the absence as ordinary errors in
// YOUR file. Two sources, both pulled back in below:
//
//   * `*.d.ts` — `src/vite-env.d.ts` is what makes `import.meta.env`
//     exist. Without it every `import.meta.env.DEV` in the subgraph is a
//     TS2339, which is exactly the kind of false red that trains people
//     to distrust a checker.
//   * `declare global` modules — `utils/paneGeometryProbe.ts` declares
//     the `window.__paneGeometry*` probes that `utils/uiRenderTrace.ts`
//     reads WITHOUT importing it. Scanned for rather than listed, so the
//     next one to appear is picked up without anyone remembering to.

import { spawnSync } from 'node:child_process';
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const FRONTEND = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const CONFIG_DIR = join(FRONTEND, 'node_modules', '.cache', 'ao-check-file');

/** Every `.ts` under `src/` whose ambient `declare global` the subset needs. */
function ambientGlobalCarriers() {
  const carriers = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(path);
      } else if (entry.name.endsWith('.ts') && !entry.name.endsWith('.d.ts')) {
        if (readFileSync(path, 'utf8').includes('declare global')) carriers.push(path);
      }
    }
  };
  walk(join(FRONTEND, 'src'));
  return carriers;
}

const requested = process.argv.slice(2);
if (requested.length === 0) {
  console.error('usage: pnpm run check:file <file.ts> [more.ts ...]');
  process.exit(2);
}

const svelte = requested.filter((path) => path.endsWith('.svelte'));
if (svelte.length > 0) {
  console.error(
    `check:file cannot check .svelte files (${svelte.join(', ')}).\n`
      + 'svelte-check has no per-file mode, and its narrowest scope,\n'
      + '--workspace <dir>, measured SLOWER than checking everything.\n'
      + 'Run `pnpm run check`.',
  );
  process.exit(2);
}

mkdirSync(CONFIG_DIR, { recursive: true });
const configPath = join(CONFIG_DIR, 'tsconfig.json');
writeFileSync(
  configPath,
  `${JSON.stringify(
    {
      extends: join(FRONTEND, 'tsconfig.json'),
      include: [join(FRONTEND, 'src/**/*.d.ts'), join(FRONTEND, 'bindings/**/*.d.ts')],
      files: [...ambientGlobalCarriers(), ...requested.map((path) => resolve(path))],
    },
    null,
    2,
  )}\n`,
);

console.error(
  `check:file — tsc over ${requested.length} file(s) + their imports. `
    + 'Does NOT check .svelte files or Svelte component props; '
    + 'run `pnpm run check` before calling it done.',
);

const tsc = spawnSync(
  join(FRONTEND, 'node_modules', '.bin', 'tsc'),
  ['-p', configPath],
  { stdio: 'inherit' },
);
process.exit(tsc.status ?? 1);
