import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { mockGitBlobImage } from '../../test/mocks/attachmentTransfer';
import { __resetForgeFactsForTest, noteForgeFactsWorkspace } from '../stores/forgeFacts.svelte';
import type { WorkspaceRef } from '../types/git';
import { __resetMediaBlobCacheForTest, __setMediaBlobCacheLimitsForTest, mediaBlobCacheStats } from './mediaBlobCache';
import { prKey, type PRRef } from './prReference';
import type { RepositoryImageClaim } from './repositoryImageHref';
import { acquireRepositoryImage, repositoryImageCacheKey, repositoryMarkdownImageSource } from './repositoryImages';
import { composeWorkspaceKey } from './workspaceKey';

// A repository image's bytes: one mint and one fetch per (blob, tier),
// counted in the media blob cache's budget, and nothing left behind by a
// failure.

const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 };
const WORKSPACE = { backend: 'home', path: '/work/widget' } as unknown as WorkspaceRef;
const OID = 'c'.repeat(40);
const OTHER_OID = 'd'.repeat(40);

const claim = (overrides: Partial<RepositoryImageClaim> = {}): RepositoryImageClaim => ({
  state: 'found',
  backend: 'home',
  pr: PR,
  commit: 'a'.repeat(40),
  path: 'docs/a.png',
  oid: OID,
  width: 640,
  height: 480,
  fluid: false,
  browserUrl: 'https://github.com/acme/widget/blob/x/docs/a.png',
  ...overrides,
});

describe('the repository image cache', () => {
  let urls = 0;
  let revoked: string[] = [];
  beforeEach(() => {
    urls = 0;
    revoked = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation(() => `blob:repo-${++urls}`);
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation((url) => void revoked.push(url));
    noteForgeFactsWorkspace(composeWorkspaceKey('home', prKey(PR)), WORKSPACE);
  });

  afterEach(() => {
    __resetMediaBlobCacheForTest();
    __resetForgeFactsForTest();
    resetBindingMocks();
    vi.restoreAllMocks();
  });

  it('mints and fetches once per blob and tier, across two bodies, a new head and another PR of the repository', async () => {
    const mint = mockGitBlobImage(() => ({ width: 640, height: 480 }));
    const fetched = vi.spyOn(globalThis, 'fetch');
    const first = acquireRepositoryImage(claim(), 720);
    // Another body naming the same file, the same file after a push, and
    // a second PR of the repository.
    const again = acquireRepositoryImage(claim(), 720);
    const pushed = acquireRepositoryImage(claim({ commit: 'e'.repeat(40) }), 720);
    const sibling = acquireRepositoryImage(claim({ pr: { ...PR, number: 5 } }), 720);
    const image = await first.value;
    expect(await again.value).toBe(image);
    expect(await pushed.value).toBe(image);
    expect(await sibling.value).toBe(image);
    expect(mint).toHaveBeenCalledTimes(1);
    expect(mint).toHaveBeenCalledWith(WORKSPACE, OID, 720);
    expect(fetched).toHaveBeenCalledTimes(1);
    // Another tier is its own entry.
    const sharper = acquireRepositoryImage(claim(), 1440);
    await sharper.value;
    expect(mint).toHaveBeenCalledTimes(2);
    for (const handle of [first, again, pushed, sibling, sharper]) handle.release();
    expect(repositoryMarkdownImageSource(claim(), {}).key).toBe(repositoryImageCacheKey('home', PR, OID));
    expect(repositoryMarkdownImageSource(claim({ commit: 'e'.repeat(40) }), {}).key).toBe(repositoryImageCacheKey('home', PR, OID));
  });

  it('answers the painted size from the mint, before the bytes land', async () => {
    let land: () => void = () => {};
    const held = new Promise<void>((resolve) => (land = resolve));
    mockGitBlobImage(() => ({ width: 720, height: 360, originalWidth: 2000, originalHeight: 1000, blob: new Blob(['png'], { type: 'image/png' }) }));
    const real = globalThis.fetch;
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (...args) => {
      await held;
      return real(...args);
    });
    const handle = acquireRepositoryImage(claim(), 720);
    expect(await handle.sized).toEqual({ width: 2000, height: 1000 });
    expect(handle.settled).toBeUndefined();
    land();
    await handle.value;
    handle.release();
  });

  it('answers a fluid image as its ratio, marked fluid, and an older computer\'s answer as before', async () => {
    mockGitBlobImage(() => ({ width: 16, height: 9, fluid: true, blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }) }));
    const fluid = acquireRepositoryImage(claim({ oid: OTHER_OID }), 0);
    expect(await fluid.sized).toEqual({ width: 16, height: 9, fluid: true });
    expect(await fluid.value).toMatchObject({ width: 16, height: 9, originalWidth: 16, originalHeight: 9, fluid: true });
    fluid.release();
    // No fluid field: a 0x0 svg, with no size to reserve.
    mockGitBlobImage(() => ({ blob: new Blob(['<svg/>'], { type: 'image/svg+xml' }) }));
    const older = acquireRepositoryImage(claim(), 0);
    expect(await older.sized).toBeUndefined();
    expect((await older.value).fluid).toBeUndefined();
    older.release();
  });

  it('counts its bytes and URL in the shared budget, keeps them while held and revokes them on eviction', async () => {
    __setMediaBlobCacheLimitsForTest({ maxEntries: 1 });
    mockGitBlobImage(() => ({ blob: new Blob(['12345'], { type: 'image/png' }) }));
    const first = acquireRepositoryImage(claim(), 720);
    const painted = await first.value;
    expect(mediaBlobCacheStats()).toEqual({ entries: 1, bytes: 5, retained: 1, loading: 0, objectURLs: 1 });
    // Held, it outlives the budget.
    const second = acquireRepositoryImage(claim({ oid: OTHER_OID }), 720);
    await second.value;
    expect(mediaBlobCacheStats()).toMatchObject({ entries: 2, retained: 2, objectURLs: 2 });
    expect(revoked).toEqual([]);
    first.release();
    second.release();
    expect(mediaBlobCacheStats().retained).toBe(0);
    // The next settle evicts the least recently used released entries.
    const third = acquireRepositoryImage(claim({ oid: 'f'.repeat(40) }), 720);
    await third.value;
    expect(revoked).toContain(painted.url);
    expect(mediaBlobCacheStats()).toMatchObject({ entries: 1, bytes: 5, objectURLs: 1, retained: 1 });
    third.release();
  });

  it('leaves nothing behind when the mint is refused, and asks again on the next acquire', async () => {
    let refuse = true;
    const mint = mockGitBlobImage(() => {
      if (refuse) throw new Error('not an image');
      return {};
    });
    const handle = acquireRepositoryImage(claim(), 720);
    await expect(handle.value).rejects.toThrow('not an image');
    expect(await handle.sized).toBeUndefined();
    handle.release();
    expect(mediaBlobCacheStats()).toEqual({ entries: 0, bytes: 0, retained: 0, loading: 0, objectURLs: 0 });
    refuse = false;
    const retry = acquireRepositoryImage(claim(), 720);
    await retry.value;
    expect(mint).toHaveBeenCalledTimes(2);
    retry.release();
  });

  it('refuses without a checkout of the repository, holding nothing', async () => {
    const mint = mockGitBlobImage();
    __resetForgeFactsForTest();
    const handle = acquireRepositoryImage(claim(), 720);
    await expect(handle.value).rejects.toThrow('No checkout of this repository');
    handle.release();
    expect(mint).not.toHaveBeenCalled();
    expect(mediaBlobCacheStats().entries).toBe(0);
  });

  it('mints once more for a spent ticket', async () => {
    let calls = 0;
    const mint = mockGitBlobImage(() => ({}));
    const real = globalThis.fetch;
    vi.spyOn(globalThis, 'fetch').mockImplementation(async (...args) => {
      if (++calls === 1) return new Response('gone', { status: 404 });
      return real(...args);
    });
    const handle = acquireRepositoryImage(claim(), 720);
    await handle.value;
    expect(mint).toHaveBeenCalledTimes(2);
    handle.release();
  });
});
