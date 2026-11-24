// Shared scaffolding for the source-reading tripwires.
//
// Suites that read the `src/` tree compare what they find against a
// shrink-only allowlist: `lib/architecture.test.ts` (state-ownership doctrine)
// and `lib/themeTokens.test.ts` (theme-token conformance) among them. The
// exclusion set, the vacuity guard, the repo-relative path shape and the
// both-directions allowlist comparison were duplicated near-verbatim between
// them; they live here so a fix to one is a fix to both.
//
// This file sits under `src/test/`, which every scanner excludes — so the
// scaffolding can name the shapes it hunts for without becoming an offender.

import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect } from 'vitest';

/** Absolute path of `frontend/src`. */
export const SRC_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// The files a tripwire applies to: those its glob names under `src/`,
// except tests, operator-run drivers, and this `src/test/` helper tree.
//
// Tests exist to drive, mock and PIN the very seams these rules protect — a
// test importing a binding, or asserting the exact hex an xterm theme
// produces, is doing its job. `vendor/` is outside `src/` entirely; its
// divergence ledger governs it.
//
// Each tripwire reads its files through its own `import.meta.glob`, so it
// loads only what it scans and `vitest --changed` selects it when one of
// those files changes. Vite expands a glob where it is written, so the
// pattern cannot live here; this checks what each glob loaded.
const NOT_SCANNED = /^\/src\/test\/|\.(?:test|spec)\.[tj]s$|\.manual\.ts$/;

export type ScannedSources = {
  /**
   * The scanned files whose name matches `extensions`, under `under`, as
   * absolute paths. Throws when nothing matches: a rule that silently
   * scanned zero files would pass vacuously forever.
   */
  files(extensions: RegExp, under?: string): string[];
  /** The text of a scanned file. */
  text(file: string): string;
};

/**
 * The sources `glob` loaded: a tripwire's own
 * `import.meta.glob<string>([...], { query: '?raw', import: 'default', eager: true })`
 * with patterns from `/src/` that exclude tests, `*.manual.ts` and `/src/test/`.
 */
export function scannedSources(glob: Record<string, string>): ScannedSources {
  const sources = new Map<string, string>();
  for (const [key, text] of Object.entries(glob)) {
    if (!key.startsWith('/src/')) throw new Error(`${key}: a tripwire's glob names files from /src/`);
    if (NOT_SCANNED.test(key)) throw new Error(`${key}: a tripwire's glob must exclude tests, operator drivers and /src/test/`);
    sources.set(join(SRC_ROOT, ...key.split('/').slice(2)), text);
  }
  return {
    files(extensions, under = SRC_ROOT) {
      const files = [...sources.keys()].filter((file) => file.startsWith(under + sep) && extensions.test(file));
      if (files.length === 0) throw new Error('no source files found; the rules would pass vacuously');
      return files;
    },
    text(file) {
      const text = sources.get(file);
      if (text === undefined) throw new Error(`${repoPath(file)} is not a scanned source`);
      return text;
    },
  };
}

/** `src/`-relative path with forward slashes — the allowlist key format. */
export function repoPath(file: string): string {
  return relative(SRC_ROOT, file).split(sep).join('/');
}

/**
 * Compare offenders against an allowlist in both directions. New offenders are
 * a regression; allowlisted files that stopped offending are a stale exception
 * that would silently grandfather the next one.
 *
 * `headline` names what the suite found (so the failure reads as its own rule,
 * not as a generic scan), `fixHint` says what to do instead.
 */
export function expectAllowlistExact(
  offenders: Map<string, string[]>,
  allowlist: Record<string, string>,
  headline: string,
  fixHint: string,
): void {
  const unexpected = [...offenders.entries()]
    .filter(([file]) => !(file in allowlist))
    .map(([file, why]) => `${file}\n    ${why.join('\n    ')}`);
  expect(unexpected, `${headline} ${fixHint}`).toEqual([]);

  const stale = Object.keys(allowlist).filter((file) => !offenders.has(file));
  expect(
    stale,
    'Allowlist entries that no longer offend. The list is shrink-only: delete them.',
  ).toEqual([]);
}
