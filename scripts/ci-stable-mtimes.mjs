// Gives the checkout modification times that depend only on content, so a
// Go test result cached by an earlier CI run stays valid.
//
//   node scripts/ci-stable-mtimes.mjs   (from the repository root)
//
// Go keys a cached test result by the size and modification time of every
// file inside the module that the test opened, and by each directory
// listing it read: an entry's name, size, mode and time. A fresh checkout
// gives every file the time of the checkout, so every such test reran.
//
// A tracked file gets a time derived from its blob id: the same bytes get
// the same time in every checkout, and changed bytes a different one. The
// times fall between 2000 and 2009, always older than the two seconds Go
// refuses to cache. Every directory that holds a tracked path, and every
// untracked directory listed in one (.git, bin, node_modules, dist), gets
// one fixed time: a listing then changes only when its entries do, and the
// files a test reads inside are recorded on their own. Untracked files keep
// their times, so a test that reads one reruns.
//
// Run it after the last step that writes into the checkout and before
// `make go-test`.
import { execFileSync } from 'node:child_process';
import { lstatSync, lutimesSync, readdirSync, utimesSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

// directoryTime is every directory's time: 2000-01-01T00:00:00Z.
export const directoryTime = 946684800;

// blobTime maps a blob id to seconds since the epoch: 28 bits of the id
// pick the second, the next 32 the fraction.
export function blobTime(oid) {
  if (!/^[0-9a-f]{40,64}$/.test(oid)) throw new Error(`not a git object id: ${oid}`);
  const seconds = parseInt(oid.slice(0, 7), 16);
  const fraction = parseInt(oid.slice(7, 15), 16) / 2 ** 32;
  return directoryTime + seconds + fraction;
}

// trackedEntries reads the index: mode, blob id and path of every entry.
export function trackedEntries(root) {
  const out = execFileSync('git', ['-C', root, 'ls-files', '--stage', '-z'], { encoding: 'utf8', maxBuffer: 1 << 28 });
  return out.split('\0').filter(Boolean).map(line => {
    const match = /^(\d{6}) ([0-9a-f]+) \d\t(.+)$/s.exec(line);
    if (!match) throw new Error(`unexpected git ls-files line: ${JSON.stringify(line)}`);
    return { mode: match[1], oid: match[2], path: match[3] };
  });
}

export function stamp(root) {
  const entries = trackedEntries(root);
  const tracked = new Set();
  const directories = new Set(['.']);
  let files = 0;
  for (const { mode, oid, path } of entries) {
    tracked.add(path);
    for (let dir = dirname(path); dir !== '.'; dir = dirname(dir)) {
      tracked.add(dir);
      directories.add(dir);
    }
    // A gitlink names a commit in another repository, not a file here.
    if (mode === '160000') continue;
    const time = blobTime(oid);
    const target = join(root, path);
    if (mode === '120000') lutimesSync(target, time, time);
    else utimesSync(target, time, time);
    files++;
  }
  let untracked = 0;
  for (const dir of directories) {
    for (const entry of readdirSync(join(root, dir), { withFileTypes: true })) {
      const path = dir === '.' ? entry.name : `${dir}/${entry.name}`;
      if (!tracked.has(path) && entry.isDirectory()) {
        utimesSync(join(root, path), directoryTime, directoryTime);
        untracked++;
      }
    }
    utimesSync(join(root, dir), directoryTime, directoryTime);
  }
  return { files, directories: directories.size, untracked };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const root = process.cwd();
    lstatSync(join(root, '.git'));
    const { files, directories, untracked } = stamp(root);
    console.log(`ci-stable-mtimes: stamped ${files} tracked files, ${directories} tracked directories and ${untracked} untracked directories`);
  } catch (error) {
    console.error(`ci-stable-mtimes: ${error.message}`);
    process.exitCode = 1;
  }
}
