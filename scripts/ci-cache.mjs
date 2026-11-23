// The Actions caches this pipeline owns: the one list of its slots, their
// keys and paths, and which entries of a slot are superseded.
//
//   node scripts/ci-cache.mjs name [--go-build SLOT] [--go-mod] [--pnpm] [--frontend-dist]
//       (.github/actions/setup) resolves each named cache's key and path
//       once and writes them to $GITHUB_ENV, so the restore and the save
//       pass the same strings: Actions hashes the path into a cache's
//       version, and a save under another spelling never restores.
//
// scripts/ci-cache-cleanup.mjs, a push's last job, is the one place that
// deletes entries. Every key starts with prefix, which nothing else in the
// repository uses; no entry outside it is ever deleted but the legacy
// shapes named below. A key is SLOT-VERSION: VERSION is its last dash-separated
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

// knownSlot is whether slot is one caches() can name, so an entry of a slot
// dropped from the lists above is retired rather than kept forever.
export const knownSlot = slot => new RegExp(
  `^${prefix}(go-build-(${goBuildSlots.join('|')})|go-mod|pnpm-store|frontend-dist)-[A-Za-z]+$`).test(slot);

function git(...args) {
  return execFileSync('git', args, { encoding: 'utf8' }).trim();
}

const digest = parts => createHash('sha256').update(parts.join('\n')).digest('hex');

// bundleEnv are the variables that change what `pnpm run build` writes
// (frontend/vite.config.ts, frontend/scripts/bundleId.ts, the UI trace and
// oracle flags). The cached bundle is built without them, so a job that
// sets one cannot take it.
export const bundleEnv = ['AO_RELEASE_BUILD', 'AO_SOURCEMAP', 'VITE_AGENT_OVERFLOW_UI_TRACE', 'VITE_AGENT_OVERFLOW_UI_ORACLES'];

// bundleSource names what the frontend build reads: every file of
// frontend/ but its guide files, which it never opens. The build reads
// nothing outside frontend/ (vite.config.ts, scripts/bundleId.ts). tree
// is `git ls-tree -r HEAD -- frontend`: mode, type, object id and path per
// line, so the digest changes with any path or byte the build reads.
export function bundleSource(tree) {
  const read = tree.split('\n').filter(line => line && !/\/(AGENTS|CLAUDE)\.md$/.test(line));
  if (read.length === 0) throw new Error('git ls-tree listed no files under frontend/');
  return digest(read);
}

// caches names the caches a job asked for: each one's key, restore prefix
// (absent for an exact-only cache) and path. objectId reads a git object
// id from the checkout, tree lists frontend/'s files, env is the job's
// environment, and tool runs a toolchain command for a path.
export function caches({ os, sha, goBuild, goMod, pnpm, frontendDist }, { objectId, tree, env, tool }) {
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
    const set = bundleEnv.filter(name => env[name]);
    if (set.length > 0) throw new Error(`${set.join(', ')} change the bundle, which the frontend-dist cache holds as built without them; build it in this job instead`);
    // Restored only for exactly the files the build reads. Two builds of
    // one tree still differ: a build that is not a release stamps its
    // bundle's version with the build time (bundleId.ts).
    named.FRONTEND_DIST = { key: `${prefix}frontend-dist-${os}-${bundleSource(tree())}`, path: 'frontend/dist' };
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

export function listCaches(repository) {
  return gh(['api', '--paginate', `repos/${repository}/actions/caches?per_page=100`, '--jq', '.actions_caches[]'])
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
    tree: () => git('ls-tree', '-r', 'HEAD', '--', 'frontend'),
    env: process.env,
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

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const [command, ...rest] = process.argv.slice(2);
    if (command === 'name') name(rest);
    else throw new Error('usage: ci-cache.mjs name [--go-build SLOT] [--go-mod] [--pnpm] [--frontend-dist]');
  } catch (error) {
    console.error(`::error title=Actions caches::${error.message}`);
    process.exitCode = 1;
  }
}
