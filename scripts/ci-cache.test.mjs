import { test } from 'node:test';
import assert from 'node:assert/strict';
import { bundleSource, caches, deleteCache, goBuildSlots, knownSlot, owned, superseded } from './ci-cache.mjs';

const sha = 'a'.repeat(40);
const frontendTree = [
  `100644 blob ${'4'.repeat(40)}\tfrontend/AGENTS.md`,
  `100644 blob ${'5'.repeat(40)}\tfrontend/src/App.svelte`,
  `100644 blob ${'6'.repeat(40)}\tfrontend/src/lib/components/chat/AGENTS.md`,
  `100644 blob ${'7'.repeat(40)}\tfrontend/src/lib/components/chat/CLAUDE.md`,
].join('\n');
const resolvers = {
  objectId: spec => ({ 'HEAD:go.sum': '1'.repeat(40) }[spec] ?? '3'.repeat(40)),
  tree: () => frontendTree,
  env: {},
  tool: (command, ...args) => `/resolved/${command}/${args.at(-1)}`,
};

test('every cache a job asks for gets one key and one resolved path', () => {
  const named = caches({ os: 'Linux', sha, goBuild: 'test-store', goMod: true, pnpm: true, frontendDist: true }, resolvers);
  assert.deepEqual(named.GO_BUILD, { key: `ao-ci-go-build-test-store-Linux-${sha}`, restore: 'ao-ci-go-build-test-store-Linux-', path: '/resolved/go/GOCACHE' });
  assert.deepEqual(named.GO_MOD, { key: `ao-ci-go-mod-Linux-${'1'.repeat(40)}`, restore: 'ao-ci-go-mod-Linux-', path: '/resolved/go/GOMODCACHE' });
  assert.match(named.PNPM.key, /^ao-ci-pnpm-store-Linux-[0-9a-f]{64}$/);
  assert.equal(named.PNPM.path, '/resolved/pnpm/path');
  // The bundle restores only for exactly the files the build reads.
  assert.deepEqual(named.FRONTEND_DIST, { key: `ao-ci-frontend-dist-Linux-${bundleSource(frontendTree)}`, path: 'frontend/dist' });
  for (const { key } of Object.values(named)) assert.ok(owned(key), key);
});

test('the bundle key follows every file the build reads and no guide file', () => {
  const base = bundleSource(frontendTree);
  assert.equal(bundleSource(frontendTree.replaceAll('4'.repeat(40), '8'.repeat(40)).replaceAll('6'.repeat(40), '9'.repeat(40))), base);
  assert.notEqual(bundleSource(frontendTree.replace('5'.repeat(40), '8'.repeat(40))), base);
  assert.notEqual(bundleSource(frontendTree.replace('frontend/src/App.svelte', 'frontend/src/Main.svelte')), base);
  assert.notEqual(bundleSource(`${frontendTree}\n100644 blob ${'8'.repeat(40)}\tfrontend/src/AGENTS.ts`), base);
});

test('a job whose environment changes the bundle cannot take the cached one', () => {
  for (const name of ['AO_RELEASE_BUILD', 'VITE_AGENT_OVERFLOW_UI_TRACE']) {
    assert.throws(() => caches({ os: 'Linux', frontendDist: true }, { ...resolvers, env: { [name]: '1' } }), new RegExp(`${name} change the bundle`));
  }
});

test('a Go build slot outside the list fails', () => {
  assert.throws(() => caches({ os: 'Linux', sha, goBuild: 'noremote-rest' }, resolvers), /not in goBuildSlots/);
  assert.ok(goBuildSlots.includes('codeql'));
});

test('superseded picks only the slot entries made before the run started', () => {
  const entries = [
    { key: `ao-ci-go-build-lint-Linux-${'1'.repeat(40)}`, created_at: '2026-11-21T10:00:00Z' },
    { key: `ao-ci-go-build-lint-Linux-${'2'.repeat(40)}`, created_at: '2026-11-22T10:05:00Z' },
    // A slot whose name extends this one's stays.
    { key: `ao-ci-go-build-lint-extra-Linux-${'3'.repeat(40)}`, created_at: '2026-11-21T10:00:00Z' },
  ];
  assert.deepEqual(superseded(entries, 'ao-ci-go-build-lint-Linux', '2026-11-22T10:00:00Z').map(e => e.key), [entries[0].key]);
});

test('the known slots are exactly those caches() names', () => {
  for (const slot of ['ao-ci-go-build-lint-Linux', 'ao-ci-go-build-test-store-macOS', 'ao-ci-go-mod-macOS', 'ao-ci-pnpm-store-Linux', 'ao-ci-frontend-dist-Linux']) {
    assert.ok(knownSlot(slot), slot);
  }
  for (const slot of ['ao-ci-go-build-noremote-rest-Linux', 'ao-ci-go-build-lint', 'ao-ci-playwright-Linux']) {
    assert.ok(!knownSlot(slot), slot);
  }
});

test('nothing outside the namespace or the legacy shapes is deleted', () => {
  assert.throws(() => deleteCache('o/r', { id: 1, key: 'setup-go-Linux-x64-abc', size_in_bytes: 1, created_at: '' }), /not a key this pipeline owns/);
});
