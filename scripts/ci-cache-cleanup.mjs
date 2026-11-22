// Keeps one Actions cache entry per cache name and fails when the entries
// left pass the budget, so the repository stays under GitHub's 10 GB cap
// and no cache a job restores is evicted.
//
//   node scripts/ci-cache-cleanup.mjs   (CI, a push to main; needs GH_TOKEN
//                                        with actions: write)
//
// A cache key is NAME-VERSION, VERSION being its last dash-separated part:
// the commit for a Go build cache, a lockfile hash otherwise. Only pushes to
// main save caches (.github/actions/setup); an entry of any other ref is
// deleted and named. Of a name's main entries the newest stays: by the
// commit's place on main when VERSION is a commit, by creation time
// otherwise. An entry created after this run started also stays, so this
// run's own saves, or those of a newer run finishing beside it, are never
// deleted; a later cleanup removes them once superseded.
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

// budgetBytes leaves 2 GB of GitHub's 10 GB for the entries a run saves
// before the cleanup after it deletes the ones they supersede.
export const budgetBytes = 8e9;

const versionOf = key => key.slice(key.lastIndexOf('-') + 1);
const nameOf = key => key.slice(0, key.lastIndexOf('-'));

export function plan(caches, mainCommits, runStartedAt, budget = budgetBytes) {
  const started = Date.parse(runStartedAt);
  if (Number.isNaN(started)) throw new Error(`run start time is not a time: ${runStartedAt}`);
  const order = new Map(mainCommits.map((sha, index) => [sha, index]));
  const remove = [];
  const warnings = [];
  const groups = new Map();
  for (const cache of caches) {
    if (cache.ref !== 'refs/heads/main') {
      remove.push(cache);
      warnings.push(`${cache.ref} saved ${cache.key}; only pushes to main save caches`);
      continue;
    }
    const name = nameOf(cache.key);
    if (!groups.has(name)) groups.set(name, []);
    groups.get(name).push(cache);
  }
  // newer sorts the newest first: a commit nearer main's head, then the
  // later creation. A commit main no longer lists sorts after every listed one.
  const rank = cache => order.get(versionOf(cache.key)) ?? (/^[0-9a-f]{40}$/.test(versionOf(cache.key)) ? Infinity : -1);
  const newer = (a, b) => (rank(a) - rank(b)) || (Date.parse(b.created_at) - Date.parse(a.created_at));
  const keep = [];
  for (const entries of groups.values()) {
    entries.sort(newer);
    entries.forEach((cache, index) => {
      if (index === 0 || Date.parse(cache.created_at) >= started) keep.push(cache);
      else remove.push(cache);
    });
  }
  const total = keep.reduce((sum, cache) => sum + cache.size_in_bytes, 0);
  return { keep, remove, warnings, total, over: total > budget };
}

const gb = bytes => `${(bytes / 1e9).toFixed(2)} GB`;

function gh(args) {
  return execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 26, stdio: ['ignore', 'pipe', 'pipe'] });
}

function main() {
  const repository = process.env.GITHUB_REPOSITORY;
  const run = process.env.GITHUB_RUN_ID;
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '') || !/^\d+$/.test(run ?? '')) {
    throw new Error('needs GITHUB_REPOSITORY and GITHUB_RUN_ID');
  }
  const caches = gh(['api', '--paginate', `repos/${repository}/actions/caches?per_page=100`, '--jq', '.actions_caches[]'])
    .split('\n').filter(Boolean).map(line => JSON.parse(line));
  const commits = gh(['api', `repos/${repository}/commits?sha=main&per_page=100`, '--jq', '.[].sha']).split('\n').filter(Boolean);
  const startedAt = gh(['api', `repos/${repository}/actions/runs/${run}`, '--jq', '.run_started_at']).trim();
  const result = plan(caches, commits, startedAt);
  for (const warning of result.warnings) console.log(`::warning title=Cache saved outside main::${warning}`);
  for (const cache of result.remove) {
    try {
      gh(['api', '-X', 'DELETE', `repos/${repository}/actions/caches/${cache.id}`]);
      console.log(`deleted ${cache.key} (${gb(cache.size_in_bytes)}, ${cache.ref})`);
    } catch (error) {
      // A cleanup finishing beside this one may have deleted it first.
      if (!String(error.stderr).includes('HTTP 404')) throw new Error(`delete ${cache.key}: ${error.stderr || error.message}`);
      console.log(`already gone: ${cache.key}`);
    }
  }
  for (const cache of result.keep.sort((a, b) => b.size_in_bytes - a.size_in_bytes)) {
    console.log(`kept ${cache.key} (${gb(cache.size_in_bytes)})`);
  }
  console.log(`ci-cache-cleanup: ${result.keep.length} entries, ${gb(result.total)} of the ${gb(budgetBytes)} budget`);
  if (result.over) {
    throw new Error(`the caches total ${gb(result.total)}, over the ${gb(budgetBytes)} budget under GitHub's 10 GB cap; ` +
      'shrink the largest slots above or drop a slot before GitHub starts evicting the caches jobs restore');
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
