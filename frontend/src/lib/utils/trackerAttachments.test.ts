// A ticket's attachments on this page: Save on each surface fileSaveAction
// decides (the ticket's computer writes it, this browser downloads it, or
// above the cap the ticket opens in the tracker), the file a name in the
// ticket's text names, and the images of a ticket leaving the media cache
// with its last surface.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import type { AttachmentView, TicketSubscription } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { attachTrackerTicket, type TrackerTicketAttachment } from '../stores/trackerTicket.svelte';
import { getToasts } from '../stores/toast.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { __mediaBlobCacheKeysForTest } from './mediaBlobCache';
import {
  BROWSER_SAVE_MAX_BYTES,
  saveTrackerAttachment,
  trackerAttachmentSave,
  trackerMarkdownImageSource,
  trackerMediaReader,
  type TrackerAttachmentTicket,
} from './trackerAttachments';
import { INLINE_IMAGE_MAX_BYTES } from '../stores/trackerTicketModel';

const action = vi.hoisted(() => ({ value: 'download' as 'save-here' | 'save-there' | 'download' }));
const downloads = vi.hoisted(() => [] as Array<{ name: string; type: string; size: number }>);
const opened = vi.hoisted(() => [] as string[]);

vi.mock('./fileSaveAction', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./fileSaveAction')>()),
  fileSaveAction: () => action.value,
}));
vi.mock('./blobDownload', () => ({
  downloadBlob: (blob: Blob, name: string) => downloads.push({ name, type: blob.type, size: blob.size }),
}));
vi.mock('./externalLinks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./externalLinks')>()),
  handleExternalURL: async (url: string) => {
    opened.push(url);
    return true;
  },
}));

const REF = { backend: HOME_BACKEND, connection: 'c1', issue: '10101' };
// A string body: its size is its length in every realm the test runs in.
const PNG = 'png-bytes';

const file = (id: string, name: string, mime: string, size: number): AttachmentView =>
  ({ id, name, mime_type: mime, size, created: 1, author: { id: 'u1', name: 'User 1' } } as AttachmentView);

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

let realFetch: typeof globalThis.fetch;
let held: TrackerTicketAttachment | null = null;

async function openTicket(): Promise<TrackerAttachmentTicket> {
  setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }) as TicketSubscription);
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  held = attachTrackerTicket(REF, true);
  await flush();
  return { key: held.key, ref: REF, url: 'https://example.atlassian.net/browse/WEB-1' };
}

beforeEach(() => {
  action.value = 'download';
  downloads.length = 0;
  opened.length = 0;
  realFetch = globalThis.fetch;
  globalThis.fetch = (async () => new Response(PNG, { status: 200, headers: { 'Content-Type': 'application/octet-stream' } })) as typeof globalThis.fetch;
  setBindingMock('MintTrackerAttachmentDownloadTicket', async () => '/attachments/tracker/a1?ticket=t');
});

afterEach(() => {
  globalThis.fetch = realFetch;
  held?.release();
  held = null;
});

describe('saving an attachment', () => {
  it('has the ticket\'s computer write it into its Downloads, here or there, and says where', async () => {
    const ticket = await openTicket();
    const save = setBindingMock('SaveTrackerAttachment', async () => '/home/user1/Downloads/report.pdf');
    action.value = 'save-here';
    await saveTrackerAttachment(ticket, file('a1', 'report.pdf', 'application/pdf', 10));
    expect(save).toHaveBeenCalledWith('sub-1', 'a1');
    expect(getToasts().at(-1)?.message).toBe('Saved to /home/user1/Downloads/report.pdf');
    action.value = 'save-there';
    expect(trackerAttachmentSave(ticket, { size: BROWSER_SAVE_MAX_BYTES + 1 })).toBe('computer');
    await saveTrackerAttachment(ticket, file('a1', 'report.pdf', 'application/pdf', BROWSER_SAVE_MAX_BYTES + 1));
    expect(save).toHaveBeenCalledTimes(2);
    expect(downloads).toEqual([]);
  });

  it('downloads it in a connected browser, typed as the ticket names it, up to the cap', async () => {
    const ticket = await openTicket();
    await saveTrackerAttachment(ticket, file('a1', 'shot.png', 'image/png', BROWSER_SAVE_MAX_BYTES));
    expect(downloads).toEqual([{ name: 'shot.png', type: 'image/png', size: PNG.length }]);
    expect(opened).toEqual([]);
  });

  it('opens the ticket in the tracker for a file over the cap, holding none of it', async () => {
    const ticket = await openTicket();
    const fetches = vi.fn(globalThis.fetch);
    globalThis.fetch = fetches as typeof globalThis.fetch;
    expect(trackerAttachmentSave(ticket, { size: BROWSER_SAVE_MAX_BYTES + 1 })).toBe('tracker');
    await saveTrackerAttachment(ticket, file('a1', 'video.mp4', 'video/mp4', BROWSER_SAVE_MAX_BYTES + 1));
    expect(opened).toEqual(['https://example.atlassian.net/browse/WEB-1']);
    expect(fetches).not.toHaveBeenCalled();
  });

  it('says so when the ticket is not followed on its computer', async () => {
    const ticket = { key: 'not-open', ref: REF, url: '' };
    await saveTrackerAttachment(ticket, file('a1', 'report.pdf', 'application/pdf', 10));
    expect(getToasts().at(-1)?.message).toBe('Could not save report.pdf: The ticket is not followed on its computer right now; try again once it shows.');
  });
});

describe('a file the ticket\'s text names', () => {
  it('is the one attachment of that name, shown in place when it is an image under the cap', async () => {
    const ticket = await openTicket();
    const attachments = [
      file('a1', 'diagram.png', 'image/png', 10),
      file('a2', 'report.pdf', 'application/pdf', 10),
      file('a3', 'huge.png', 'image/png', INLINE_IMAGE_MAX_BYTES + 1),
      file('a4', 'twice.png', 'image/png', 10),
      file('a5', 'twice.png', 'image/png', 10),
    ];
    const read = trackerMediaReader(() => ({ ticket, attachments }));
    expect(read('diagram.png')?.image?.filename).toBe('diagram.png');
    expect(read('report.pdf')).toMatchObject({ name: 'report.pdf', size: 10, image: null, saveLabel: 'Save' });
    expect(read('huge.png')?.image).toBeNull();
    expect(read('twice.png')).toBeNull();
    expect(read('missing.png')).toBeNull();
    expect(trackerMediaReader(() => null)('diagram.png')).toBeNull();
  });

  it('leaves the media cache with the ticket\'s last surface', async () => {
    const ticket = await openTicket();
    const source = trackerMarkdownImageSource(ticket, file('a1', 'diagram.png', 'image/png', 10));
    const hold = source.acquire(0);
    const value = await hold.value;
    expect(value.mimeType).toBe('image/png');
    hold.release();
    expect(__mediaBlobCacheKeysForTest()).toContain(source.key);
    held!.release();
    held = null;
    expect(__mediaBlobCacheKeysForTest()).not.toContain(source.key);
  });
});
