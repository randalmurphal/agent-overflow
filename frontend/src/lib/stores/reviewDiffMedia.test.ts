import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { createReviewDiffMedia, type ImageHandle } from './reviewDiffMedia.svelte';
import { DiffFilesHandle, DIFF_FILES_CHANGED, DIFF_FILES_GONE, DIFF_FILES_UNREADABLE, DIFF_FILE_REFUSED, readDiffFileSizes, type DiffFilesTarget, type DiffImageAnswer } from './reviewDiffFiles';
import { TransportError } from '../transport/wsClient';
import { frontendErrorCaptureStateForTest, resetFrontendErrorCaptureForTest } from '../utils/frontendErrorCapture';
import { __mediaBlobCacheKeysForTest, __resetMediaBlobCacheForTest, __setMediaBlobCacheLimitsForTest } from '../utils/mediaBlobCache';
import { __setBackendStatusForTest } from './transportStatus.svelte';
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
// A git diff's target: the handle its open answered, and its reopen
// (null: the open read the working tree).
const GONE = 'This diff\'s files are no longer held; reload the diff.';
const gone = () => new TransportError(DIFF_FILES_GONE, GONE);
const CHANGED = 'This file changed since the diff was read; reload the diff.';

function gitTarget(handle = 'h1', reopen: (() => Promise<string>) | null = async () => 'h2', workspace = WORKSPACE): DiffFilesTarget {
  return { kind: 'git', backend: 'home', workspace, files: new DiffFilesHandle(handle, reopen) };
}

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
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    expect(handle.state.kind).toBe('loading');
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    // The handle and the file's paths: no object id leaves the client.
    expect(read.mock.calls[0]).toEqual([WORKSPACE, 'h1', { path: 'img/logo.png' }]);
    expect(fetched.sort()).toEqual(['/t/logo/new', '/t/logo/old']);
    expect(shownUrls(handle).map(([name, url]) => [name, url !== null])).toEqual([['old', true], ['new', true]]);
    handle.release();
  });

  it('shows an added image by its new side alone', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => ({ ...answer('icon'), old: side(false, 0) }));
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(read.mock.calls[0][2]).toEqual({ path: 'img/icon.png' });
    expect(fetched).toEqual(['/t/icon/new']);
    expect(shownUrls(handle).map(([name]) => name)).toEqual(['new']);
    handle.release();
  });

  it('shows an over-cap side as its size and note, fetching nothing for it', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo', { new: { url: undefined, overCap: true, size: 30 * 1024 * 1024 } }));
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
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
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toEqual({ kind: 'binary', reason: null, action: null, sizes: { old: { exists: true, size: 1200 }, new: { exists: true, size: 3400 } } });
    expect(fetched).toEqual([]);
  });

  it('shows an unavailable side\'s sentence on the binary row, with no Retry', async () => {
    setBindingMock('GetReviewDiffImage', async () => answer('logo', { new: { url: undefined, unavailable: 'The file is a link outside the repository.' } }));
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'The file is a link outside the repository.', action: null });
  });

  it('offers reloading the diff for a file changed since it was read, never the same read again', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw new TransportError(DIFF_FILES_CHANGED, CHANGED);
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: CHANGED, action: 'reload' });
    expect(reopen).not.toHaveBeenCalled();
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('offers the changed refusal a reload on its code whatever its sentence says (a redacted one included)', async () => {
    setBindingMock('GetReviewDiffImage', async () => {
      throw new TransportError(DIFF_FILES_CHANGED, 'method failed (id: abc)');
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: 'method failed (id: abc)', action: 'reload' });
  });

  it('offers a retry, never a reload, for a sentence that reads like the changed refusal but has no code', async () => {
    setBindingMock('GetReviewDiffImage', async () => {
      throw new Error(CHANGED);
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: CHANGED, action: 'retry' });
  });

  it('offers nothing for a file the diff refuses: every read answers the same', async () => {
    const SENTENCE = 'img/logo.png is not a file this diff changes';
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw new TransportError(DIFF_FILE_REFUSED, SENTENCE);
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: SENTENCE, action: null });
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('reopens a handle the server no longer holds once, shared by every read, and reads again on the new one', async () => {
    const read = setBindingMock('GetReviewDiffImage', async (_ws: unknown, handle: string, file: { path: string }) => {
      if (handle === 'h1') throw gone();
      return answer(file.path === 'img/logo.png' ? 'logo' : 'icon');
    });
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const logo = media.image(files()['img/logo.png'], ['old', 'new']);
    const icon = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(reopen).toHaveBeenCalledTimes(1);
    expect(read.mock.calls.map((call) => call[1])).toEqual(['h1', 'h1', 'h2', 'h2']);
    expect(logo.state.kind).toBe('image');
    expect(icon.state.kind).toBe('image');
    logo.release();
    icon.release();
  });

  it('shows the sentence, offering a reload, when the reopened handle is refused too: never a loop', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw gone();
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(reopen).toHaveBeenCalledTimes(1);
    expect(read).toHaveBeenCalledTimes(2);
    expect(handle.state).toMatchObject({ kind: 'binary', reason: GONE, action: 'reload' });
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('shows a diff whose files the server could not pin as the sentence, reopening nothing and offering no retry', async () => {
    const SENTENCE = 'This diff\'s files cannot be read: the snapshot failed.';
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw new TransportError(DIFF_FILES_UNREADABLE, SENTENCE);
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(handle.state).toMatchObject({ kind: 'binary', reason: SENTENCE, action: null });
    expect(reopen).not.toHaveBeenCalled();
    handle.retry();
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('never reopens the handle of a diff that left the screen (a reconnect answering for it late)', async () => {
    let refuse: (err: Error) => void = () => {};
    setBindingMock('GetReviewDiffImage', () => new Promise((_resolve, reject) => { refuse = reject; }));
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    media.setTarget(gitTarget('h9'));
    refuse(gone());
    await settle();
    expect(reopen).not.toHaveBeenCalled();
    expect(handle.state.kind).toBe('loading');
    handle.release();
  });

  it('reopens on the gone code alone, never on a sentence that reads like it', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw new Error(GONE);
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    const handle = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(reopen).not.toHaveBeenCalled();
    expect(read).toHaveBeenCalledTimes(1);
    expect(handle.state).toMatchObject({ kind: 'binary', action: 'retry' });
  });

  it('reads sizes on a reopened handle too, once', async () => {
    const sizes = setBindingMock('GetReviewDiffFileSizes', async (_ws: unknown, handle: string, list: Array<{ path: string }>) => {
      if (handle === 'h1') throw gone();
      return list.map((file) => ({ path: file.path, old: { exists: false, size: 0 }, new: { exists: true, size: 8 } }));
    });
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    media.requestSizes(files()['blob.bin']);
    await settle();
    expect(reopen).toHaveBeenCalledTimes(1);
    expect(sizes.mock.calls.map((call) => call[1])).toEqual(['h1', 'h2']);
    expect(media.sizes('blob.bin')).toMatchObject({ kind: 'ready' });
  });

  // Each handle's answer names its own bytes, as two opens' do.
  function byHandle() {
    return setBindingMock('GetReviewDiffImage', async (_ws: unknown, handle: string, file: { path: string }) => answer(`${handle}${file.path}`));
  }

  it('reads one path again under another open: the bytes are keyed by the open, never reused across diffs', async () => {
    const read = byHandle();
    const media = createReviewDiffMedia();
    // All commits shows logo version B; then an older commit's open.
    media.setTarget(gitTarget('all'));
    const first = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(shownUrls(first).map(([, url]) => url !== null)).toEqual([true, true]);
    first.release();
    media.setTarget(gitTarget('older'));
    const second = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(read.mock.calls.map((call) => call[1])).toEqual(['all', 'older']);
    expect(fetched).toEqual(['/t/allimg/logo.png/old', '/t/allimg/logo.png/new', '/t/olderimg/logo.png/old', '/t/olderimg/logo.png/new']);
    expect(shownUrls(second)).not.toEqual(shownUrls(first));
    second.release();
  });

  it('keeps two panes\' same path apart', async () => {
    const read = byHandle();
    const left = createReviewDiffMedia();
    const right = createReviewDiffMedia();
    left.setTarget(gitTarget('pane-a', null, { projectId: 'p1', workspacePath: '' }));
    right.setTarget(gitTarget('pane-b', null, { projectId: 'p2', workspacePath: '' }));
    const a = left.image(files()['img/logo.png'], ['new']);
    const b = right.image(files()['img/logo.png'], ['new']);
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    expect(fetched.sort()).toEqual(['/t/pane-aimg/logo.png/new', '/t/pane-bimg/logo.png/new']);
    expect(shownUrls(a)).not.toEqual(shownUrls(b));
    a.release();
    b.release();
  });

  it('drops a diff\'s bytes from the cache when it is closed or replaced, once nothing shows them', async () => {
    byHandle();
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1'));
    const row = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(__mediaBlobCacheKeysForTest().filter((key) => key.includes('"h1"'))).toHaveLength(2);
    media.setTarget(null);
    // Still shown: it goes when the row lets go.
    expect(__mediaBlobCacheKeysForTest().filter((key) => key.includes('"h1"'))).toHaveLength(2);
    row.release();
    expect(__mediaBlobCacheKeysForTest()).toEqual([]);
    expect(media.identity).toBeNull();
  });

  it('gives a reopen a new identity, reads under it, and drops the forgotten handle\'s bytes', async () => {
    let refuse = false;
    setBindingMock('GetReviewDiffImage', async (_ws: unknown, handle: string, file: { path: string }) => {
      if (refuse && handle === 'h1') throw gone();
      return answer(`${handle}${file.path}`);
    });
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1'));
    const before = media.identity;
    const logo = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    logo.release();
    refuse = true;
    const icon = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(media.identity).not.toBe(before);
    expect(media.identity).toContain('"h2"');
    expect(__mediaBlobCacheKeysForTest().some((key) => key.includes('"h1"'))).toBe(false);
    expect(shownUrls(icon)[0][1]).not.toBeNull();
    icon.release();
  });

  it('reads nothing before an open is attached', async () => {
    const read = byHandle();
    const media = createReviewDiffMedia();
    expect(media.identity).toBeNull();
    const row = media.image(files()['img/logo.png'], ['old', 'new']);
    media.requestSizes(files()['blob.bin']);
    await settle();
    expect(read).not.toHaveBeenCalled();
    expect(row.state.kind).toBe('loading');
    row.release();
  });

  it('offers reloading a working-tree diff whose handle is gone, and never reopens it or reads on it again', async () => {
    const read = setBindingMock('GetReviewDiffImage', async () => {
      throw gone();
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', null));
    const logo = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(logo.state).toMatchObject({ kind: 'binary', reason: GONE, action: 'reload' });
    const icon = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(icon.state).toMatchObject({ kind: 'binary', reason: GONE, action: 'reload' });
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('after its connection ends, a working-tree diff\'s unread row offers reloading with no read made', async () => {
    const read = byHandle();
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', null));
    __setBackendStatusForTest('home', { status: 'connected', nextAttemptAt: null });
    __setBackendStatusForTest('home', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('home', { status: 'connected', nextAttemptAt: null });
    const row = media.image(files()['img/logo.png'], ['old', 'new']);
    await settle();
    expect(row.state).toMatchObject({ kind: 'binary', reason: GONE, action: 'reload' });
    expect(read).not.toHaveBeenCalled();
    row.release();
  });

  it('after its connection ends, a revision diff reopens before its next read rather than after a refusal', async () => {
    const read = byHandle();
    const reopen = vi.fn(async () => 'h2');
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget('h1', reopen));
    __setBackendStatusForTest('home', { status: 'connected', nextAttemptAt: null });
    __setBackendStatusForTest('home', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('home', { status: 'connected', nextAttemptAt: null });
    const row = media.image(files()['img/logo.png'], ['old', 'new']);
    // A second row reading while the reopen is in flight waits for it.
    const icon = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(reopen).toHaveBeenCalledTimes(1);
    expect(read.mock.calls.map((call) => call[1])).toEqual(['h2', 'h2']);
    icon.release();
    expect(media.identity).toContain('"h2"');
    row.release();
  });

  it('shows a busy refusal on the row with Retry, and a Retry is one fresh read', async () => {
    let refuse = true;
    const read = setBindingMock('GetReviewDiffImage', async () => {
      if (refuse) throw new Error('Too many images are loading. Try again in a moment.');
      return answer('logo');
    });
    setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
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
    media.setTarget(gitTarget());
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
    media.setTarget(gitTarget());
    const released = media.image(files()['img/logo.png'], ['old', 'new']);
    released.release();
    resolve(answer('logo'));
    await settle();
    expect(released.state.kind).toBe('loading');
    expect(fetched).toEqual([]);

    const replaced = media.image(files()['img/logo.png'], ['old', 'new']);
    media.setTarget(gitTarget('h9'));
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
    media.setTarget(gitTarget());
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

  it('carries a file\'s own refusal code with its sentence', async () => {
    setBindingMock('GetReviewDiffFileSizes', async (_ws: unknown, _handle: unknown, refs: Array<{ path: string }>) => refs.map((ref) => ({
      path: ref.path, old: side(false, 0), new: side(false, 0), error: `${ref.path} is not a file`, code: DIFF_FILE_REFUSED,
    })));
    const sizes = await readDiffFileSizes(gitTarget(), [files()['img/logo.png']]);
    expect(sizes.get('img/logo.png')).toEqual({ old: side(false, 0), new: side(false, 0), error: 'img/logo.png is not a file', code: DIFF_FILE_REFUSED });
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
    media.setTarget(gitTarget());
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
    media.setTarget(gitTarget());
    expect(media.sizes('f0.bin')).toBeNull();
  });

  it('drops a sizes answer for a replaced diff', async () => {
    let resolve: (value: unknown) => void = () => {};
    setBindingMock('GetReviewDiffFileSizes', () => new Promise((done) => { resolve = done; }));
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    media.requestSizes(files()['blob.bin']);
    await settle();
    media.setTarget(gitTarget());
    resolve([{ path: 'blob.bin', old: side(true, 1), new: side(true, 2) }]);
    await settle();
    expect(media.sizes('blob.bin')).toBeNull();
  });

  it('reads an edit\'s new side by its edit, and reads no sizes there', async () => {
    const read = setBindingMock('GetEditDiffImage', async () => ({ old: side(false, 0), new: side(true, 99, { url: '/t/edit/new', mimeType: 'image/png' }), maxBytes: 100 }));
    const sizes = setBindingMock('GetReviewDiffFileSizes', async () => []);
    const media = createReviewDiffMedia();
    media.setTarget({ kind: 'edits', backend: 'home', threadId: 't1', editPayloadId: 'pay', editTurnIndex: -1 });
    expect(media.readsSizes).toBe(false);
    media.requestSizes(files()['blob.bin']);
    const handle = media.image(files()['img/icon.png'], ['new']);
    await settle();
    expect(sizes).not.toHaveBeenCalled();
    expect(read.mock.calls[0]).toEqual(['t1', { path: 'img/icon.png', editPayloadId: 'pay', editTurnIndex: -1 }]);
    expect(fetched).toEqual(['/t/edit/new']);
    handle.release();
  });

  it('remembers a side\'s decoded pixel size for its caption until the diff changes', () => {
    const media = createReviewDiffMedia();
    media.setTarget(gitTarget());
    media.rememberPixels('img/logo.png', 'new', 64, 32);
    media.rememberPixels('img/logo.png', 'old', 0, 0);
    expect(media.pixels('img/logo.png', 'new')).toEqual({ width: 64, height: 32 });
    expect(media.pixels('img/logo.png', 'old')).toBeNull();
    media.setTarget(gitTarget());
    expect(media.pixels('img/logo.png', 'new')).toBeNull();
  });
});
