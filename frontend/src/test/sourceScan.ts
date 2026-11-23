// Shared scaffolding for the source-reading tripwires.
//
// Two suites read the whole `src/` tree and compare what they find against a
// shrink-only allowlist: `lib/architecture.test.ts` (state-ownership doctrine)
// and `lib/themeTokens.test.ts` (theme-token conformance). The walk, the
// exclusion set, the vacuity guard, the repo-relative path shape and the
// both-directions allowlist comparison were duplicated near-verbatim between
// them; they live here so a fix to one is a fix to both.
//
// This file sits under `src/test/`, which BOTH scanners exclude — so the
// scaffolding can name the shapes it hunts for without becoming an offender.

import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect } from 'vitest';

/** Absolute path of `frontend/src`. */
export const SRC_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// The files a tripwire applies to: everything under `src/` except tests,
// operator-run drivers, and this `src/test/` helper tree.
//
// Tests exist to drive, mock and PIN the very seams these rules protect — a
// test importing `GetGitStatus`, or asserting the exact hex an xterm theme
// produces, is doing its job. `vendor/` is outside `src/` entirely; its
// divergence ledger governs it.
//
// Read through an import so that `vitest --changed` selects a tripwire when
// any file it scans changes.
const SOURCES: Map<string, string> = new Map(
  Object.entries(
    import.meta.glob<string>(
      ['/src/**/*.{ts,js,svelte,css}', '!/src/test/**', '!**/*.{test,spec}.{ts,js}', '!**/*.manual.ts'],
      { query: '?raw', import: 'default', eager: true },
    ),
  ).map(([key, text]) => [join(SRC_ROOT, ...key.split('/').slice(2)), text]),
);

/**
 * The scanned files whose name matches `extensions`, as absolute paths.
 * Callers pass the extension pattern because the scanners differ: the
 * architecture rules are about imports (`.ts`/`.svelte`) while the token
 * rules also cover stylesheets.
 *
 * Throws when nothing matches: a rule that silently scanned zero files
 * would pass vacuously forever.
 */
export function scannedSources(extensions: RegExp, under = SRC_ROOT): string[] {
  const files = [...SOURCES.keys()].filter((file) => file.startsWith(under + sep) && extensions.test(file));
  if (files.length === 0) throw new Error('no source files found; the rules would pass vacuously');
  return files;
}

/** The text of a scanned file. */
export function sourceText(file: string): string {
  const text = SOURCES.get(file);
  if (text === undefined) throw new Error(`${repoPath(file)} is not a scanned source`);
  return text;
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
