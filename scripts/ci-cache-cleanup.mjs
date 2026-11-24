// A push's last job, and the one place entries are deleted: keeps each
// slot's newest entry, retires the rest, the cache key shapes this pipeline
// used before its namespace and entries of a slot it no longer has. It
// fails when what it keeps does not fit beside everything else under
// GitHub's 10 GB cap, and warns when two generations would not.
//
//   node scripts/ci-cache-cleanup.mjs   (CI, a push to main; needs GH_TOKEN
//                                        with actions: write)
//
// It deletes only entries in this pipeline's namespace (ci-cache.mjs
// prefix) and the legacy shapes ci-cache.mjs names. Everything else, such as
// what another workflow's setup-go saved, is counted and named, never
// touched.
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { deleteCache, knownSlot, legacyShapes, listCaches, prefix, repositoryAndRun, slotOf, superseded } from './ci-cache.mjs';

// capBytes is GitHub's cache cap for a repository. Past it GitHub evicts
// entries, the one accessed longest ago first. Kept entries that do not fit
// beside the others are evicted on every push and the jobs that restore
// them run cold, so that fails this job (over). A push's jobs each save a
// new entry while the previous one stays until this job retires it; when
// two generations do not fit, the previous one, which this job retires
// anyway, is evicted early, and a run that starts in that window can miss
// it. That costs one cold job, so it is a warning (tight).
export const capBytes = 10e9;

export function plan(caches, runStartedAt) {
  const remove = [];
  const warnings = [];
  const managed = [];
  const other = [];
  for (const cache of caches) {
    if (cache.key.startsWith(prefix)) {
      if (cache.ref !== 'refs/heads/main') {
        remove.push(cache);
        warnings.push(`${cache.ref} saved ${cache.key}; only pushes to main save caches`);
      } else if (!knownSlot(slotOf(cache.key))) {
        remove.push(cache);
        warnings.push(`${cache.key} is of a slot scripts/ci-cache.mjs no longer names`);
      } else managed.push(cache);
    } else if (legacyShapes.some(shape => shape.test(cache.key))) {
      remove.push(cache);
    } else {
      other.push(cache);
    }
  }
  // A slot keeps its newest entry even when that predates this run (a job
  // this run skipped saved nothing), and every entry made since the run
  // started (superseded): this run's own save, and that of a newer run
  // finishing beside it. The rest are retired.
  const retired = new Set();
  for (const slot of new Set(managed.map(cache => slotOf(cache.key)))) {
    const old = superseded(managed, slot, runStartedAt).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
    const newer = managed.some(cache => slotOf(cache.key) === slot && !old.includes(cache));
    for (const cache of newer ? old : old.slice(1)) retired.add(cache);
  }
  remove.push(...retired);
  const keep = managed.filter(cache => !retired.has(cache));
  const bytes = list => list.reduce((sum, cache) => sum + cache.size_in_bytes, 0);
  const managedBytes = bytes(keep);
  const otherBytes = bytes(other);
  return {
    keep, other, remove, warnings, managedBytes, otherBytes,
    over: managedBytes + otherBytes > capBytes, tight: 2 * managedBytes + otherBytes > capBytes,
  };
}

const gb = n => `${(n / 1e9).toFixed(2)} GB`;

function main() {
  const { repository, startedAt } = repositoryAndRun();
  const result = plan(listCaches(repository), startedAt);
  for (const warning of result.warnings) console.log(`::warning title=Cache saved outside main::${warning}`);
  for (const cache of result.remove) deleteCache(repository, cache);
  for (const cache of result.keep.sort((a, b) => b.size_in_bytes - a.size_in_bytes)) console.log(`kept ${cache.key} (${gb(cache.size_in_bytes)})`);
  for (const cache of result.other) console.log(`not this pipeline's: ${cache.key} (${gb(cache.size_in_bytes)}, ${cache.ref})`);
  const held = `${gb(result.managedBytes)} (this pipeline's ${result.keep.length} entries) + ${gb(result.otherBytes)} (other caches) = ${gb(result.managedBytes + result.otherBytes)}`;
  const peak = `2 x ${gb(result.managedBytes)} (this pipeline's entries, held twice while a push saves) + ${gb(result.otherBytes)} (other caches) = ${gb(2 * result.managedBytes + result.otherBytes)}`;
  const remedy = 'shrink the largest slots above, drop a slot, or remove the other caches';
  console.log(`ci-cache-cleanup: ${held}; ${peak}; GitHub's cap is ${gb(capBytes)}`);
  if (result.over) {
    throw new Error(`${held}, over GitHub's ${gb(capBytes)} cap, so GitHub evicts entries the next jobs restore; ${remedy}`);
  }
  if (result.tight) {
    console.log(`::warning title=Actions caches near the cap::${peak}, over GitHub's ${gb(capBytes)} cap, so a push evicts the previous entries before this job retires them and a run that starts meanwhile can miss one; ${remedy}`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main();
  } catch (error) {
    console.error(`::error title=Actions cache cleanup::${error.message}`);
    process.exitCode = 1;
  }
}
