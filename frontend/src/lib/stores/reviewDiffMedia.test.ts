import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { createReviewDiffMedia, type ImageHandle } from './reviewDiffMedia.svelte';
import type { DiffFilesTarget, DiffImageAnswer } from './reviewDiffFiles';
import { frontendErrorCaptureStateForTest, resetFrontendErrorCaptureForTest } from '../utils/frontendErrorCapture';
import { __resetMediaBlobCacheForTest, __setMediaBlobCacheLimitsForTest } from '../utils/mediaBlobCache';
import { parseReviewFiles, type ReviewFile } from '../utils/patchStore';

const fetched: string[] = [];
let failFetch: string | null = null;

vi.mock('../transport/attachmentTransfer', () => ({
  fetchTicketedBytes: async (_backend: string, url: string) => {
    fetched.push(url);
    if (failFetch) throw new Error(failFetch);
    return new Blob([url], { type: 'image/png' });
  },
}));

vi.mock('../transport/backends', () => ({
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));

const WORKSPACE = { projectId: 'p1', workspacePath: '' };
const REVISION = { baseRef: 'main', baseSHA: 'b'.repeat(40), headSHA: 'h'.repeat(40) };
const PR_TARGET: DiffFilesTarget = { kind: 'git', backend: 'home', workspace: WORKSPACE, opened: { source: { scope: 'pr', revision: REVISION }, unavailable: null } };
const prTarget = (source: Extract<DiffFilesTarget, { kind: 'git' }>['opened']['source']): DiffFilesTarget => ({ ...PR_TARGET, opened: { source, unavailable: null } });

const PATCH = [
  'diff --git a/img/logo.png b/img/logo.png',
  'index 1111111..2222222 100644',
  'Binary files a/img/logo.png and b/img/logo.png differ',
  'diff --git a/img/icon.png b/img/icon.png',
  'new file mode 100644',
  'index 0000000..3333333',
  'Binary files /dev/null and b/img/icon.png differ',
  'diff --git a/blob.bin b/blob.bin',
  'index 4444444..5555555 100644',
  'Binary files a/blob.bin and b/blob.bin differ',
  '',
].join('\n');

function files(): Record<string, ReviewFile> {
  return Object.fromEntries(parseReviewFiles(PATCH).map((file) => [file.path, file]));
}

function side(exists: boolean, size: number, extra: Record<string, unknown> = {}) {
  return { exists, size, ...extra };
}

function answer(path: string, extra: Partial<Record<'old' | 'new', Record<string, unknown>>> = {}): DiffImageAnswer {
  return {
    old: side(true, 1200, { url: `/t/${path}/old`, mimeType: 'image/png', ...extra.old }),
    new: side(true, 3400, { url: `/t/${path}/new`, mimeType: 'image/png', ...extra.new }),
    maxBytes: 25 * 1024 * 1024,
  };
}

// Settles the RPC, the fetches and the state writes they make.
async function settle(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function shownUrls(handle: ImageHandle): Array<[string, string | null]> {
  const state = handle.state;
  return state.kind === 'image' ? state.sides.map((each) => [each.side, each.url]) : [];
}

describe('a review diff\'s images', () => {
  beforeEach(() => {
    fetched.length = 0;
    resetFrontendErrorCaptureForTest();
    failFetch = null;
    __resetMediaBlobCacheForTest();
  });
  afterEach(() => {
    resetBindingMocks();
    __resetMediaBlobCacheForTest();
  });

  it('reads a changed image once, naming the file as the patch does, and shows both sides', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => answer('logo'));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    expect(handle.state.kind).toBe('loading');
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    expect(read.mock.calls[0]).toEqual([WORKSPACE, { scope: 'pr', revision: REVISION }, { path: 'img/logo.png', oldPath: undefined, oldOid: '1111111', newOid: '2222222' }]);
    expect(fetched.sort()).toEqual(['/t/logo/new', '/t/logo/old']);
    expect(shownUrls(handle).map(([name, url]) => [name, url !== null])).toEqual([['old', true], ['new', true]]);
    handle.release();
  });

  it('names an added side\'s absent old blob as zeros, and a run\'s ends with the source', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => ({ ...answer('icon'), old: side(false, 0) }));
    const media = createReviewDiffMedia();
    media.setTarget(prTarget({ scope: 'pr', revision: REVISION, firstSha: 'c'.repeat(40), lastSha: 'e'.repeat(40) }), '1');
    const handle = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(read.mock.calls[0][1]).toEqual({ scope: 'pr', revision: REVISION, firstSha: 'c'.repeat(40), lastSha: 'e'.repeat(40) });
    expect(read.mock.calls[0][2]).toMatchObject({ oldOid: '0000000', newOid: '3333333' });
    expect(fetched).toEqual(['/t/icon/new']);
    expect(shownUrls(handle).map(([name]) => name)).toEqual(['new']);
    handle.release();
  });

  it('shows an over-cap side as its size and note, fetching nothing for it', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo', { new: { url: undefined, overCap: true, size: 30 * 1024 * 1024 } }));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(fetched).toEqual(['/t/logo/old']);
    const state = handle.state;
    expect(state.kind).toBe('image');
    if (state.kind !== 'image') return;
    expect(state.sides[1]).toEqual({ side: 'new', size: 30 * 1024 * 1024, overCap: true, url: null });
    expect(state.maxBytes).toBe(25 * 1024 * 1024);
    handle.release();
  });

  it('turns a file that is not an image into the binary row with the answer\'s sizes', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo', { old: { notImage: true, url: undefined }, new: { notImage: true, url: undefined } }));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toEqual({ kind: 'binary', reason: null, action: null, sizes: { old: { exists: true, size: 1200 }, new: { exists: true, size: 3400 } } });
    expect(fetched).toEqual([]);
  });

  it('shows an unavailable side\'s sentence on the binary row, with no Retry', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo', { new: { url: undefined, unavailable: 'The file is a link outside the repository.' } }));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'The file is a link outside the repository.', action: null });
  });

  it('offers reloading the diff for a file changed since it was read, never the same read again', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw new Error('img/logo.png changed since the diff was read.');
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'img/logo.png changed since the diff was read.', action: 'reload' });
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('shows a diff whose sides could not be pinned as binary rows with its sentence, reading nothing', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => answer('logo'));
    const sizes = setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget({ ...PR_TARGET, opened: { source: { scope: 'workspace' }, unavailable: 'The diff\'s sides could not be read.' } }, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    media.requestSizes(files()['blob.bin']);
    await settle();
    expect(handle.state).toEqual({ kind: 'binary', reason: 'The diff\'s sides could not be read.', action: null, sizes: null });
    expect(read).not.toHaveBeenCalled();
    expect(sizes).not.toHaveBeenCalled();
  });

  it('shows a busy refusal on the row with Retry, and a Retry is one fresh read', async () => {
    let refuse = true;
    const read = setBindingMock('GetReviewDiffImage', async () => {
      if (refuse) throw new Error('Too many images are loading. Try again in a moment.');
      return answer('logo');
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'Too many images are loading. Try again in a moment.', action: 'retry' });
    expect(read).toHaveBeenCalledTimes(1);
    // Nothing reads again on its own.
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    refuse = false;
    handle.retry();
    expect(handle.state.kind).toBe('loading');
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    expect(handle.state.kind).toBe('image');
    // A Retry outside a failure does nothing.
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    handle.release();
  });

  it('turns a failed byte fetch into the binary row with Retry, and reports it', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo'));
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    failFetch = 'Too many images are loading. Try again in a moment.';
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'Too many images are loading. Try again in a moment.', action: 'retry' });
    // Reported once, though both sides' fetches failed.
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(1);
    failFetch = null;
    handle.retry();
    await settle();
    expect(handle.state.kind).toBe('image');
    handle.release();
  });

  it('drops an answer that lands after its row was released or its diff replaced', async () => {
    let resolve: (value: DiffImageAnswer) => void = () => {};
    setBindingMock('GetReviewDiffImage', () => new Promise<DiffImageAnswer>((done) => { resolve = done; }));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const released = media.image(files()['img/logo.png'], ['old', 'new']);
    released.release();
    resolve(answer('logo'));
    await settle();
    expect(released.state.kind).toBe('loading');
    expect(fetched).toEqual([]);

    const replaced = media.image(files()['img/logo.png'], ['old', 'new']);
    media.setTarget(prTarget({ scope: 'pr', revision: { ...REVISION, headSHA: 'n'.repeat(40) } }), '2');
    resolve(answer('logo'));
    await settle();
    expect(replaced.state.kind).toBe('loading');
    expect(fetched).toEqual([]);
    replaced.release();
  });

  it('releases a row\'s bytes when it unmounts, so the cache can evict them, and keeps them while held', async () => {
    const read = setBindingMock('GetReviewDiffImage', async (_ws: unknown, _source: unknown, file: { path: string }) => answer(file.path === 'img/logo.png' ? 'logo' : 'icon'));
    __setMediaBlobCacheLimitsForTest({ maxEntries: 2 });
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    const all = files();
    const logo = media.image(all['img/logo.png'], ['old', 'new']);
    await settle();
    expect(fetched).toHaveLength(2);
    // Held: the icon cannot push the logo's bytes out.
    const iconRow = media.image(all['img/icon.png'], ['new']);
    await settle();
    expect(fetched).toHaveLength(3);
    const again = media.image(all['img/logo.png'], ['old', 'new']);
    // Painted at once from the cache, with no read at all.
    expect(shownUrls(again).every(([, url]) => url !== null)).toBe(true);
    expect(read).toHaveBeenCalledTimes(2);
    again.release();
    // Released (scrolled far): the next file's bytes evict them, and a
    // remount reads them again by a fresh read (tickets are single-use).
    logo.release();
    const icon = all['img/icon.png'];
    const other = { ...icon, path: 'img/other.png', header: { ...icon.header!, newBlob: '6666666' } } as ReviewFile;
    const next = media.image(other, ['new']);
    await settle();
    const reads = read.mock.calls.length;
    fetched.length = 0;
    const back = media.image(all['img/logo.png'], ['old', 'new']);
    // Its facts are kept: the row paints its captions while the bytes load.
    expect(back.state).toMatchObject({ kind: 'image' });
    await settle();
    expect(fetched.sort()).toEqual(['/t/logo/new', '/t/logo/old']);
    expect(read).toHaveBeenCalledTimes(reads + 1);
    back.release();
    next.release();
    iconRow.release();
  });

  it('reads binary files\' sizes in batches of 100, and a file\'s own failure is its own', async () => {
    const calls: number[] = [];
    setBindingMock('GetReviewDiffFileSizes', async (_ws: unknown, _source: unknown, refs: Array<{ path: string }>) => {
      calls.push(refs.length);
      return refs.map((ref) => (ref.path === 'f7.bin'
        ? { path: ref.path, old: side(false, 0), new: side(false, 0), error: 'The blob is missing.' }
        : { path: ref.path, old: side(true, 10), new: side(true, 20) }));
    });
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    expect(media.readsSizes).toBe(true);
    const template = files()['blob.bin'];
    const many = Array.from({ length: 150 }, (_, index) => ({ ...template, path: `f${index}.bin` }) as ReviewFile);
    for (const file of many) media.requestSizes(file);
    // Asked twice: nothing more.
    media.requestSizes(many[0]);
    expect(media.sizes('f0.bin')).toEqual({ kind: 'loading' });
    await settle();
    expect(calls).toEqual([100, 50]);
    expect(media.sizes('f0.bin')).toEqual({ kind: 'ready', sizes: { old: { exists: true, size: 10 }, new: { exists: true, size: 20 } } });
    expect(media.sizes('f7.bin')).toEqual({ kind: 'failed', error: 'The blob is missing.' });
    // A new diff forgets them.
    media.setTarget(PR_TARGET, '2');
    expect(media.sizes('f0.bin')).toBeNull();
  });

  it('drops a sizes answer for a replaced diff', async () => {
    let resolve: (value: unknown) => void = () => {};
    setBindingMock('GetReviewDiffFileSizes', () => new Promise((done) => { resolve = done; }));
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    media.requestSizes(files()['blob.bin']);
    await settle();
    media.setTarget(PR_TARGET, '2');
    resolve([{ path: 'blob.bin', old: side(true, 1), new: side(true, 2) }]);
    await settle();
    expect(media.sizes('blob.bin')).toBeNull();
  });

  it('reads an edit\'s new side by its edit and patch text, and reads no sizes there', async () => {
    const read = setBindingMock('GetEditDiffImage', async () => ({ old: side(false, 0), new: side(true, 99, { url: '/t/edit/new', mimeType: 'image/png' }), maxBytes: 100 }));
    const sizes = setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget({ kind: 'edits', backend: 'home', threadId: 't1', editPayloadId: 'pay', editTurnIndex: -1, verifyPatch: async () => 'PATCH TEXT' }, '1');
    expect(media.readsSizes).toBe(false);
    media.requestSizes(files()['blob.bin']);
    const handle = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(sizes).not.toHaveBeenCalled();
    expect(read.mock.calls[0]).toEqual(['t1', expect.objectContaining({ scope: 'edits', path: 'img/icon.png', verifyPatch: 'PATCH TEXT', editPayloadId: 'pay', editTurnIndex: -1 })]);
    expect(fetched).toEqual(['/t/edit/new']);
    handle.release();
  });

  it('remembers a side\'s decoded pixel size for its caption until the diff changes', () => {
    const media = createReviewDiffMedia();
    media.setTarget(PR_TARGET, '1');
    media.rememberPixels('img/logo.png', 'new', 64, 32);
    media.rememberPixels('img/logo.png', 'old', 0, 0);
    expect(media.pixels('img/logo.png', 'new')).toEqual({ width: 64, height: 32 });
    expect(media.pixels('img/logo.png', 'old')).toBeNull();
    media.setTarget(PR_TARGET, '2');
    expect(media.pixels('img/logo.png', 'new')).toBeNull();
  });
});
