// The Actions caches this pipeline owns: the one list of its slots, their
// keys and paths, and the retirement of a slot's superseded entries.
//
//   node scripts/ci-cache.mjs name [--go-build SLOT] [--go-mod] [--pnpm] [--frontend-dist]
//       (.github/actions/setup) resolves each named cache's key and path
//       once and writes them to $GITHUB_ENV, so the restore and the save
//       pass the same strings: Actions hashes the path into a cache's
//       version, and a save under another spelling never restores.
//   node scripts/ci-cache.mjs retire KEY
//       (.github/actions/cache-save, after saving KEY on a push) deletes
//       the entries of KEY's slot that were made before this run started,
//       so a slot holds two entries only between a save and its retire.
//       Needs GH_TOKEN with actions: write.
//
// Every key starts with prefix, which nothing else in the repository uses;
// no entry outside it is ever deleted but the legacy shapes named below. A key is SLOT-VERSION: VERSION is its last dash-separated
// part, the commit for a Go build cache and a git object id otherwise, so
// a key changes exactly when its content's source does.
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

export const prefix = 'ao-ci-';

// goBuildSlots are the Go build cache slots, one per job that sets up Go,
// named by its cache-name. A job with other build tags or another package
// set keeps a slot of its own, so one job's save never evicts another's.
export const goBuildSlots = [
  'checks', 'build-macos', 'lint', 'test-store', 'test-app-triage', 'test-rest', 'test-noremote', 'netns', 'harness', 'codeql',
];

// legacyShapes are the keys this pipeline saved before its caches moved
// under prefix: Go build slots keyed by go.sum and commit, among them the
// retired noremote slots, the module cache, setup-node's pnpm cache and
// the Playwright browser cache. scripts/ci-cache-cleanup.mjs retires them;
// once a cleanup has run on main none is left, and this list can go.
export const legacyShapes = [
  /^go-build-[\w-]+-(Linux|macOS)-[0-9a-f]{64}-[0-9a-f]{40}$/,
  /^go-mod-(Linux|macOS)-[0-9a-f]{64}$/,
  /^node-cache-(Linux|macOS)-\w+-pnpm-[0-9a-f]{64}$/,
  /^playwright-(Linux|macOS)-/,
];

// owned is whether this pipeline may delete an entry: its namespace, or a
// legacy shape.
export const owned = key => key.startsWith(prefix) || legacyShapes.some(shape => shape.test(key));

export const slotOf = key => key.slice(0, key.lastIndexOf('-'));

function git(...args) {
  return execFileSync('git', args, { encoding: 'utf8' }).trim();
}

const digest = parts => createHash('sha256').update(parts.join('\n')).digest('hex');

// caches names the caches a job asked for: each one's key, restore prefix
// (absent for an exact-only cache) and path. objectId reads a git object
// id from the checkout; tool runs a toolchain command for a path.
export function caches({ os, sha, goBuild, goMod, pnpm, frontendDist }, { objectId, tool }) {
  if (!/^[A-Za-z]+$/.test(os ?? '')) throw new Error(`runner OS is not a name: ${os}`);
  const named = {};
  if (goBuild !== undefined) {
    if (!goBuildSlots.includes(goBuild)) {
      throw new Error(`Go build cache slot "${goBuild}" is not in goBuildSlots (scripts/ci-cache.mjs); add it there`);
    }
    if (!/^[0-9a-f]{40}$/.test(sha ?? '')) throw new Error(`commit is not a commit id: ${sha}`);
    const slot = `${prefix}go-build-${goBuild}-${os}`;
    named.GO_BUILD = { key: `${slot}-${sha}`, restore: `${slot}-`, path: tool('go', 'env', 'GOCACHE') };
  }
  if (goMod) {
    const slot = `${prefix}go-mod-${os}`;
    named.GO_MOD = { key: `${slot}-${objectId('HEAD:go.sum')}`, restore: `${slot}-`, path: tool('go', 'env', 'GOMODCACHE') };
  }
  if (pnpm) {
    const slot = `${prefix}pnpm-store-${os}`;
    const lockfiles = digest([objectId('HEAD:frontend/pnpm-lock.yaml'), objectId('HEAD:e2e/pnpm-lock.yaml')]);
    named.PNPM = { key: `${slot}-${lockfiles}`, restore: `${slot}-`, path: tool('pnpm', '--dir', 'frontend', 'store', 'path') };
  }
  if (frontendDist) {
    // The tree id names frontend/'s every path and byte; the bundle is
    // restored only for that exact tree.
    named.FRONTEND_DIST = { key: `${prefix}frontend-dist-${os}-${objectId('HEAD:frontend')}`, path: 'frontend/dist' };
  }
  return named;
}

// superseded picks, of the entries listed for a slot, those to retire: made
// before this run started, so never this run's own save nor that of a newer
// run finishing beside it.
export function superseded(entries, slot, runStartedAt) {
  const started = Date.parse(runStartedAt);
  if (Number.isNaN(started)) throw new Error(`run start time is not a time: ${runStartedAt}`);
  return entries.filter(entry => slotOf(entry.key) === slot && Date.parse(entry.created_at) < started);
}

export function gh(args) {
  return execFileSync('gh', args, { encoding: 'utf8', maxBuffer: 1 << 26, stdio: ['ignore', 'pipe', 'pipe'] });
}

export function repositoryAndRun() {
  const repository = process.env.GITHUB_REPOSITORY;
  const run = process.env.GITHUB_RUN_ID;
  if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '') || !/^\d+$/.test(run ?? '')) {
    throw new Error('needs GITHUB_REPOSITORY and GITHUB_RUN_ID');
  }
  const startedAt = gh(['api', `repos/${repository}/actions/runs/${run}`, '--jq', '.run_started_at']).trim();
  return { repository, startedAt };
}

export function listCaches(repository, key) {
  const query = key === undefined ? '' : `&key=${encodeURIComponent(key)}`;
  return gh(['api', '--paginate', `repos/${repository}/actions/caches?per_page=100${query}`, '--jq', '.actions_caches[]'])
    .split('\n').filter(Boolean).map(line => JSON.parse(line));
}

// deleteCache deletes one entry; one a run finishing beside this one
// deleted first is already gone.
export function deleteCache(repository, cache) {
  if (!owned(cache.key)) throw new Error(`refusing to delete ${cache.key}: not a key this pipeline owns`);
  try {
    gh(['api', '-X', 'DELETE', `repos/${repository}/actions/caches/${cache.id}`]);
    console.log(`deleted ${cache.key} (${(cache.size_in_bytes / 1e9).toFixed(2)} GB, ${cache.created_at})`);
  } catch (error) {
    if (!String(error.stderr).includes('HTTP 404')) throw new Error(`delete ${cache.key}: ${error.stderr || error.message}`);
    console.log(`already gone: ${cache.key}`);
  }
}

function name(flags) {
  const options = { os: process.env.RUNNER_OS, sha: process.env.GITHUB_SHA };
  for (let i = 0; i < flags.length; i++) {
    switch (flags[i]) {
      case '--go-build': options.goBuild = flags[++i]; break;
      case '--go-mod': options.goMod = true; break;
      case '--pnpm': options.pnpm = true; break;
      case '--frontend-dist': options.frontendDist = true; break;
      default: throw new Error(`unknown flag ${flags[i]}`);
    }
  }
  const named = caches(options, {
    objectId: spec => git('rev-parse', spec),
    tool: (command, ...args) => execFileSync(command, args, { encoding: 'utf8' }).trim(),
  });
  const env = process.env.GITHUB_ENV;
  if (!env) throw new Error('needs GITHUB_ENV');
  for (const [cache, { key, restore, path }] of Object.entries(named)) {
    if (!path) throw new Error(`${cache} resolved no path`);
    appendFileSync(env, `AO_CACHE_${cache}_KEY=${key}\nAO_CACHE_${cache}_RESTORE=${restore ?? ''}\nAO_CACHE_${cache}_PATH=${path}\n`);
    console.log(`${cache}: ${key} at ${path}`);
  }
}

function retire(key) {
  if (!key?.startsWith(prefix)) throw new Error(`retire needs a key this pipeline owns, got ${key}`);
  const { repository, startedAt } = repositoryAndRun();
  const slot = slotOf(key);
  for (const cache of superseded(listCaches(repository, `${slot}-`), slot, startedAt)) deleteCache(repository, cache);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const [command, ...rest] = process.argv.slice(2);
    if (command === 'name') name(rest);
    else if (command === 'retire' && rest.length === 1) retire(rest[0]);
    else throw new Error('usage: ci-cache.mjs name [--go-build SLOT] [--go-mod] [--pnpm] [--frontend-dist] | retire KEY');
  } catch (error) {
    console.error(`::error title=Actions caches::${error.message}`);
    process.exitCode = 1;
  }
}
