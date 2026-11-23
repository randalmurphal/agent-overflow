// After a push's jobs saved their caches: retires what each save's own
// retire could not (scripts/ci-cache.mjs), retires the cache key shapes
// this pipeline used before its namespace, and fails loudly when the
// repository's caches pass the budget under GitHub's 10 GB cap, before
// GitHub starts evicting the caches jobs restore.
//
//   node scripts/ci-cache-cleanup.mjs   (CI, a push to main; needs GH_TOKEN
//                                        with actions: write)
//
// It deletes only entries in this pipeline's namespace (ci-cache.mjs
// prefix) and the legacy shapes ci-cache.mjs names. Everything else, such as what
// another workflow's setup-go saved, is counted and named, never touched.
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { deleteCache, legacyShapes, listCaches, prefix, repositoryAndRun, slotOf, superseded } from './ci-cache.mjs';

// budgetBytes leaves 2 GB of GitHub's 10 GB for the entries a run saves
// before their retire, and for caches this pipeline does not own.
export const budgetBytes = 8e9;

export function plan(caches, runStartedAt, budget = budgetBytes) {
  const remove = [];
  const warnings = [];
  const managed = [];
  const other = [];
  for (const cache of caches) {
    if (cache.key.startsWith(prefix)) {
      if (cache.ref !== 'refs/heads/main') {
        remove.push(cache);
        warnings.push(`${cache.ref} saved ${cache.key}; only pushes to main save caches`);
      } else managed.push(cache);
    } else if (legacyShapes.some(shape => shape.test(cache.key))) {
      remove.push(cache);
    } else {
      other.push(cache);
    }
  }
  // A slot keeps its newest entry even when that predates this run (a job
  // this run skipped saved nothing), and every entry made since the run
  // started (superseded); the rest are retired.
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
  return { keep, other, remove, warnings, managedBytes, otherBytes, over: managedBytes + otherBytes > budget };
}

const gb = n => `${(n / 1e9).toFixed(2)} GB`;

function main() {
  const { repository, startedAt } = repositoryAndRun();
  const result = plan(listCaches(repository), startedAt);
  for (const warning of result.warnings) console.log(`::warning title=Cache saved outside main::${warning}`);
  for (const cache of result.remove) deleteCache(repository, cache);
  for (const cache of result.keep.sort((a, b) => b.size_in_bytes - a.size_in_bytes)) console.log(`kept ${cache.key} (${gb(cache.size_in_bytes)})`);
  for (const cache of result.other) console.log(`not this pipeline's: ${cache.key} (${gb(cache.size_in_bytes)}, ${cache.ref})`);
  console.log(`ci-cache-cleanup: this pipeline's ${result.keep.length} entries hold ${gb(result.managedBytes)}, other caches ${gb(result.otherBytes)}, of the ${gb(budgetBytes)} budget`);
  if (result.over) {
    throw new Error(`the caches total ${gb(result.managedBytes + result.otherBytes)} (this pipeline's ${gb(result.managedBytes)}, others ${gb(result.otherBytes)}), over the ${gb(budgetBytes)} budget under GitHub's 10 GB cap; ` +
      'shrink the largest slots above, drop a slot, or remove the other caches before GitHub starts evicting the caches jobs restore');
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
