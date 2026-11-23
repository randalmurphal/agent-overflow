import { test } from 'node:test';
import assert from 'node:assert/strict';
import { caches, deleteCache, goBuildSlots, owned, superseded } from './ci-cache.mjs';

const sha = 'a'.repeat(40);
const resolvers = {
  objectId: spec => ({ 'HEAD:go.sum': '1'.repeat(40), 'HEAD:frontend': '2'.repeat(40) }[spec] ?? '3'.repeat(40)),
  tool: (command, ...args) => `/resolved/${command}/${args.at(-1)}`,
};

test('every cache a job asks for gets one key and one resolved path', () => {
  const named = caches({ os: 'Linux', sha, goBuild: 'test-store', goMod: true, pnpm: true, frontendDist: true }, resolvers);
  assert.deepEqual(named.GO_BUILD, { key: `ao-ci-go-build-test-store-Linux-${sha}`, restore: 'ao-ci-go-build-test-store-Linux-', path: '/resolved/go/GOCACHE' });
  assert.deepEqual(named.GO_MOD, { key: `ao-ci-go-mod-Linux-${'1'.repeat(40)}`, restore: 'ao-ci-go-mod-Linux-', path: '/resolved/go/GOMODCACHE' });
  assert.match(named.PNPM.key, /^ao-ci-pnpm-store-Linux-[0-9a-f]{64}$/);
  assert.equal(named.PNPM.path, '/resolved/pnpm/path');
  // The bundle restores only for the exact tree of frontend/.
  assert.deepEqual(named.FRONTEND_DIST, { key: `ao-ci-frontend-dist-Linux-${'2'.repeat(40)}`, path: 'frontend/dist' });
  for (const { key } of Object.values(named)) assert.ok(owned(key), key);
});

test('a Go build slot outside the list fails', () => {
  assert.throws(() => caches({ os: 'Linux', sha, goBuild: 'noremote-rest' }, resolvers), /not in goBuildSlots/);
  assert.ok(goBuildSlots.includes('codeql'));
});

test('retire picks only the slot entries made before the run started', () => {
  const entries = [
    { key: `ao-ci-go-build-lint-Linux-${'1'.repeat(40)}`, created_at: '2026-11-21T10:00:00Z' },
    { key: `ao-ci-go-build-lint-Linux-${'2'.repeat(40)}`, created_at: '2026-11-22T10:05:00Z' },
    // The key filter is a prefix: another slot sharing it stays.
    { key: `ao-ci-go-build-lint-extra-Linux-${'3'.repeat(40)}`, created_at: '2026-11-21T10:00:00Z' },
  ];
  assert.deepEqual(superseded(entries, 'ao-ci-go-build-lint-Linux', '2026-11-22T10:00:00Z').map(e => e.key), [entries[0].key]);
});

test('nothing outside the namespace or the legacy shapes is deleted', () => {
  assert.throws(() => deleteCache('o/r', { id: 1, key: 'setup-go-Linux-x64-abc', size_in_bytes: 1, created_at: '' }), /not a key this pipeline owns/);
});
