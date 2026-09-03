// The bundle id rule, on the build side, and the Vite plugin that stamps
// it into `dist`.
//
// This is the SECOND implementation of one rule. `internal/bundle` (Go)
// is the first: it hashes the embedded `frontend/dist` and publishes the
// answer as `bundleId` on the hello frame. This one hashes the same tree
// at build time and writes `bundle-id.txt` into it.
//
// **Why two.** A phone shell running the bundle its APK shipped with has
// no native state-file entry naming that bundle, and it still has to be
// able to say "the backend's id is the one I am already running".
// Without this file the shell would download and stage the very bundle
// it was already executing, on every hello, forever. The file is the
// APK's own answer to "what am I".
//
// **They cannot be allowed to drift**, so both hash
// `internal/bundle/testdata/fixturebundle/` in their own suite and both
// compare against `internal/bundle/testdata/fixturebundle.id`. Changing
// the rule means changing both and re-stamping that golden; each side's
// test failure says so.
//
// The rule, stated once here and once in `bundle.ID`:
//
//   - Files are everything under the tree EXCEPT `*.map` (emitted only by
//     `AO_SOURCEMAP=1`, requested by no page, and megabytes on a phone's
//     link) and `bundle-id.txt` itself (written after the hash, so a
//     walk that counted it would hash a tree this plugin never saw).
//   - Sorted by path, compared as UTF-8 BYTES — which is what Go's string
//     comparison does, and what JavaScript's `<` on strings does not for
//     anything outside the BMP.
//   - The id is the hex SHA-256 over `path\x00sha256\n` per file.

import { createHash } from 'node:crypto';
import { readdir, readFile, writeFile } from 'node:fs/promises';
import * as path from 'node:path';

/** Where a built bundle records its own id. Mirrors `bundle.IDFileName`. */
export const BUNDLE_ID_FILE = 'bundle-id.txt';

/** One file, as the manifest describes it. Mirrors `bundle.File`. */
export interface BundleFile {
  path: string;
  sha256: string;
  size: number;
}

/** Whether one path inside the tree is part of the bundle. Mirrors `bundle.Included`. */
export function included(name: string): boolean {
  if (name === BUNDLE_ID_FILE) return false;
  return !name.endsWith('.map');
}

/**
 * Hash every included file under `root`, sorted the way the Go side
 * sorts: by the path's UTF-8 bytes.
 *
 * Directory entries are walked, never hashed. Symlinks are followed by
 * `readFile` and are not expected here — Vite writes plain files.
 */
export async function bundleFiles(root: string): Promise<BundleFile[]> {
  const files: BundleFile[] = [];
  const walk = async (dir: string, prefix: string): Promise<void> => {
    const entries = await readdir(dir, { withFileTypes: true });
    for (const entry of entries) {
      const relative = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.isDirectory()) {
        await walk(path.join(dir, entry.name), relative);
        continue;
      }
      if (!entry.isFile() || !included(relative)) continue;
      const bytes = await readFile(path.join(dir, entry.name));
      files.push({
        path: relative,
        sha256: createHash('sha256').update(bytes).digest('hex'),
        size: bytes.byteLength,
      });
    }
  };
  await walk(root, '');
  files.sort((a, b) => Buffer.compare(Buffer.from(a.path, 'utf8'), Buffer.from(b.path, 'utf8')));
  return files;
}

/** The content id over an already-hashed file list. Mirrors `bundle.ID`. */
export function bundleId(files: readonly BundleFile[]): string {
  const sorted = [...files].sort((a, b) =>
    Buffer.compare(Buffer.from(a.path, 'utf8'), Buffer.from(b.path, 'utf8')),
  );
  const digest = createHash('sha256');
  for (const file of sorted) {
    digest.update(file.path, 'utf8');
    digest.update(Uint8Array.of(0));
    digest.update(file.sha256, 'utf8');
    digest.update(Uint8Array.of(0x0a));
  }
  return digest.digest('hex');
}

/** Walk a directory and answer its bundle id. */
export async function computeBundleId(root: string): Promise<string> {
  return bundleId(await bundleFiles(root));
}

/**
 * The Vite plugin: after a production build, stamp `dist/bundle-id.txt`.
 *
 * `closeBundle` rather than `writeBundle`, so every asset — including the
 * ones plugins emit late — is on disk before the walk. Build only: a dev
 * server has no tree to hash and no shell to answer.
 *
 * A failure here FAILS THE BUILD rather than warning. A dist with no id
 * file is a shell that re-downloads its own bundle on every connection,
 * which is exactly the failure nobody would notice until a phone was on
 * a metered link.
 */
export function bundleIdPlugin(): {
  name: string;
  apply: 'build';
  closeBundle: () => Promise<void>;
  configResolved: (config: { build: { outDir: string }; root: string }) => void;
} {
  let outDir = '';
  return {
    name: 'agent-overflow:bundle-id',
    apply: 'build',
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    async closeBundle() {
      const id = await computeBundleId(outDir);
      await writeFile(path.join(outDir, BUNDLE_ID_FILE), `${id}\n`, 'utf8');
    },
  };
}
