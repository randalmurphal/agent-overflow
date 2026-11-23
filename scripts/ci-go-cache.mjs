// Keeps a CI Go build cache to what one run used, so a saved cache does not
// carry every earlier run's entries forward.
//
//   node scripts/ci-go-cache.mjs backdate GOCACHE STATE   after the restore
//   node scripts/ci-go-cache.mjs prune GOCACHE STATE      before the save
//
// Go refreshes an entry's modification time when it uses one that is more
// than an hour old, and its own trim deletes entries unused for five days.
// backdate sets every restored entry two hours back and records that time
// and the entry count in STATE; prune then deletes the entries still at
// that time, the ones this run did not use. An entry is a file or directory
// ending -a or -d in one of the cache's two-hex-digit directories; the
// cache's own top-level files stay, and so does every output a kept index
// record names.
//
// If not one restored entry was refreshed although the run used the cache,
// Go no longer refreshes as this relies on: prune then keeps everything and
// says so, so the cache grows instead of going cold.
import { lstatSync, readdirSync, readFileSync, rmSync, utimesSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

// backdateSeconds is how far back restored entries are set: past Go's
// one-hour refresh threshold, far short of its five-day trim.
export const backdateSeconds = 2 * 60 * 60;

export function entries(cache) {
  const found = [];
  for (const shard of readdirSync(cache)) {
    if (!/^[0-9a-f]{2}$/.test(shard)) continue;
    for (const name of readdirSync(join(cache, shard))) {
      if (/-[ad]$/.test(name)) found.push(join(cache, shard, name));
    }
  }
  return found;
}

function bytes(path) {
  const stats = lstatSync(path);
  if (!stats.isDirectory()) return stats.size;
  let total = 0;
  for (const name of readdirSync(path)) total += bytes(join(path, name));
  return total;
}

export function backdate(cache, now = Date.now() / 1000) {
  const time = Math.floor(now) - backdateSeconds;
  const all = entries(cache);
  for (const path of all) utimesSync(path, time, time);
  return { time, entries: all.length };
}

// outputOf reads the output an index record (-a) names: Go writes
// "v1 ACTION OUTPUT SIZE TIME" and keeps the output as OUTPUT-d.
export function outputOf(cache, record) {
  const fields = readFileSync(record, 'utf8').split(' ');
  if (fields[0] !== 'v1' || !/^[0-9a-f]{64}$/.test(fields[2] ?? '')) return undefined;
  return join(cache, fields[2].slice(0, 2), `${fields[2]}-d`);
}

export function prune(cache, state) {
  let unused = [];
  const used = [];
  for (const path of entries(cache)) {
    if (lstatSync(path).mtimeMs / 1000 <= state.time) unused.push(path);
    else used.push(path);
  }
  const refreshed = state.entries - unused.length;
  // Go writes a fresh index record for an output it already has without
  // touching the output, so an output a kept record names stays however
  // old it looks.
  const named = new Set(used.filter(path => path.endsWith('-a')).map(path => outputOf(cache, path)));
  const rescued = unused.filter(path => named.has(path));
  unused = unused.filter(path => !named.has(path));
  const kept = used.length + rescued.length;
  const keptBytes = [...used, ...rescued].reduce((sum, path) => sum + bytes(path), 0);
  const unusedBytes = unused.reduce((sum, path) => sum + bytes(path), 0);
  if (state.entries > 0 && refreshed <= 0) {
    return { kept: kept + unused.length, keptBytes: keptBytes + unusedBytes, deleted: 0, deletedBytes: 0, keptAll: true };
  }
  for (const path of unused) rmSync(path, { recursive: true, force: true });
  return { kept, keptBytes, deleted: unused.length, deletedBytes: unusedBytes, keptAll: false };
}

const mib = n => `${(n / 2 ** 20).toFixed(1)} MiB`;

function main([command, cache, statePath]) {
  if (!['backdate', 'prune'].includes(command) || !cache || !statePath) {
    throw new Error('usage: ci-go-cache.mjs backdate|prune GOCACHE STATE');
  }
  if (command === 'backdate') {
    const state = backdate(cache);
    writeFileSync(statePath, JSON.stringify(state));
    console.log(`ci-go-cache: set ${state.entries} restored entries back to ${new Date(state.time * 1000).toISOString()}`);
    return;
  }
  const state = JSON.parse(readFileSync(statePath, 'utf8'));
  const result = prune(cache, state);
  if (result.keptAll) {
    console.log(`::warning title=Go build cache not pruned::None of the ${state.entries} restored entries was refreshed during the run, so the run's use of them cannot be told apart; kept all ${result.kept} entries (${mib(result.keptBytes)}). Check whether Go still refreshes an entry's time when it uses one over an hour old (scripts/ci-go-cache.mjs).`);
    return;
  }
  console.log(`ci-go-cache: kept ${result.kept} entries (${mib(result.keptBytes)}), deleted ${result.deleted} this run did not use (${mib(result.deletedBytes)})`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    console.error(`ci-go-cache: ${error.message}`);
    process.exitCode = 1;
  }
}
