// The ticket's attachments and poker sections and a file in its text, as
// rendered: the attachments newest first with Attach and the upload line,
// no Attach where the tracker stores no files; the poker votes as the read
// gives them, Clear votes behind a second press, the estimate offers after
// a reveal, and the poker part telling its surface it is on screen; and a
// name in the text that one attachment has showing that file.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AttachmentView, PokerView, WriteView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TicketAttachments from './TicketAttachments.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import TicketPoker from './TicketPoker.svelte';
import TicketMarkdown from './TicketMarkdown.svelte';
import { TRACKER_MEDIA_CONTEXT, type TrackerMediaReader } from '../chat/markdown/trackerMediaContext';
import { attachTrackerTicket, type TrackerTicketAttachment } from '../../stores/trackerTicket.svelte';
import { setTicketPoints, type TicketWriteContext } from '../../stores/trackerTicketWrites.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../../transport/backendKey';
import { rememberImageTier } from '../../utils/imageTiers';
import { DisconnectedError, TransportError } from '../../transport/wsClient';
import { getToasts } from '../../stores/toast.svelte';
import { BROWSER_SAVE_MAX_BYTES } from '../../utils/trackerAttachments';
import { pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';

const REF = { backend: HOME_BACKEND, connection: 'c1', issue: '10101' };
const CTX: TicketWriteContext = { key: 'ticket-1', ref: REF, label: 'WEB-1' };
const TICKET = { key: CTX.key, ref: REF, url: 'https://example.atlassian.net/browse/WEB-1' };

const file = (id: string, name: string, created: number, size = 10, mime = 'application/pdf'): AttachmentView =>
  ({ id, name, mime_type: mime, size, created, author: { id: 'u1', name: 'User 1' } } as AttachmentView);

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('the attachments section', () => {
  let realFetch: typeof globalThis.fetch;
  beforeEach(() => {
    realFetch = globalThis.fetch;
  });
  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  it('lists the attachments newest first, with their size and author', () => {
    const { getAllByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx: CTX, ticket: TICKET, attachments: [file('a1', 'old.pdf', 1), file('a2', 'new.pdf', 2, 4_100_000)], canAttach: true, open: true, onFold: () => {} },
    });
    const rows = getAllByTestId('ticket-attachment');
    expect(rows.map((r) => r.dataset.name)).toEqual(['new.pdf', 'old.pdf']);
    expect(rows[0].textContent).toContain('4.1 MB');
    expect(rows[0].textContent).toContain('User 1');
  });

  it('attaches the files picked, saying which one goes and its place', async () => {
    setBindingMock('MintTrackerAttachmentUploadTicket', async () => '/attachments/tracker/upload?ticket=t');
    const answers: Array<(r: Response) => void> = [];
    globalThis.fetch = (() => new Promise<Response>((resolve) => answers.push(resolve))) as typeof globalThis.fetch;
    // A surface holds the ticket, as the screen's does: an outcome is
    // kept beside the section only while one does.
    setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const held = attachTrackerTicket(REF, true);
    const ctx = { ...CTX, key: held.key };
    const { getByTestId, queryByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx, ticket: { ...TICKET, key: held.key }, attachments: [], canAttach: true, open: true, onFold: () => {} },
    });
    const input = getByTestId('ticket-attach-input') as HTMLInputElement;
    const picked = [new File(['x'.repeat(4_100_000)], 'report.pdf'), new File(['y'], 'notes.txt')];
    Object.defineProperty(input, 'files', { configurable: true, value: picked });
    await fireEvent.change(input);
    await flush();
    expect(getByTestId('ticket-uploading').textContent?.trim()).toBe('Uploading report.pdf (4.1 MB)… 1 of 2');
    answers.shift()!(new Response(JSON.stringify({ parts: [{ step: 'attach', state: 'applied' }] } as WriteView), { status: 200 }));
    await flush();
    expect(getByTestId('ticket-uploading').textContent?.trim()).toBe('Uploading notes.txt (1 B)… 2 of 2');
    answers.shift()!(new Response(JSON.stringify({ code: 'tracker_upload_size_changed', message: 'select the file again and attach it' }), { status: 400 }));
    await waitFor(() => expect(queryByTestId('ticket-uploading')).toBeNull());
    const outcome = getByTestId('ticket-attach-outcome');
    expect(outcome.dataset.name).toBe('notes.txt');
    expect(outcome.textContent).toContain('select the file again and attach it');
    held.release();
  });

  it('says of each file whether it was not sent, not attached, or may have been attached, from where it failed and the route\'s code', async () => {
    let mints = 0;
    setBindingMock('MintTrackerAttachmentUploadTicket', async () => {
      mints += 1;
      if (mints === 3) throw new DisconnectedError('transport disconnected');
      return '/attachments/tracker/upload?ticket=t';
    });
    const answers: Array<(r: Response) => void> = [];
    globalThis.fetch = (() => new Promise<Response>((resolve) => answers.push(resolve))) as typeof globalThis.fetch;
    setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const held = attachTrackerTicket(REF, true);
    const ctx = { ...CTX, key: held.key };
    const { getByTestId, queryByTestId, container } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx, ticket: { ...TICKET, key: held.key }, attachments: [], canAttach: true, open: true, onFold: () => {} },
    });
    const input = getByTestId('ticket-attach-input') as HTMLInputElement;
    Object.defineProperty(input, 'files', { configurable: true, value: [new File(['a'], 'unclear.txt'), new File(['b'], 'changed.txt'), new File(['c'], 'unsent.txt')] });
    await fireEvent.change(input);
    await flush();
    const unclear = 'the tracker\'s answer to this change could not be read, so it may or may not have been made; look at the ticket\'s attachments before you try again';
    answers.shift()!(new Response(JSON.stringify({ code: 'tracker_write_unclear', message: unclear }), { status: 400 }));
    await flush();
    answers.shift()!(new Response(JSON.stringify({ code: 'tracker_upload_size_changed', message: 'select the file again and attach it' }), { status: 400 }));
    await waitFor(() => expect(queryByTestId('ticket-uploading')).toBeNull());
    const note = (name: string) => container.querySelector<HTMLElement>(`[data-testid="ticket-attach-outcome"][data-name="${name}"] [data-testid="ticket-write-outcome"]`)!;
    expect(note('unclear.txt').dataset.kind).toBe('unknown');
    expect(note('unclear.txt').textContent).toContain(unclear);
    expect(note('changed.txt').dataset.kind).toBe('not_sent');
    expect(note('changed.txt').textContent).toContain('select the file again and attach it');
    expect(note('unsent.txt').textContent).toMatch(/^\s*Not sent: /);
    expect(note('unsent.txt').textContent).not.toContain('may or may not');
    held.release();
  });

  it('offers the tracker for a file the tracker cannot take here', async () => {
    setBindingMock('MintTrackerAttachmentUploadTicket', async () => {
      throw new TransportError('tracker_attachment_too_large', 'the tracker stores files up to 2.0 GB, and this one is 3.1 GB; share it another way and link to it');
    });
    setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const held = attachTrackerTicket(REF, true);
    const { getByTestId, findByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx: { ...CTX, key: held.key }, ticket: { ...TICKET, key: held.key }, attachments: [], canAttach: true, open: true, onFold: () => {} },
    });
    const input = getByTestId('ticket-attach-input') as HTMLInputElement;
    Object.defineProperty(input, 'files', { configurable: true, value: [new File(['a'], 'huge.zip')] });
    await fireEvent.change(input);
    expect(await findByTestId('ticket-attach-open-tracker')).toHaveTextContent('Open in the tracker');
    held.release();
  });

  it('names the file in the toast of an upload that settles after its ticket closed', async () => {
    setBindingMock('MintTrackerAttachmentUploadTicket', async () => '/attachments/tracker/upload?ticket=t');
    const answers: Array<(r: Response) => void> = [];
    globalThis.fetch = (() => new Promise<Response>((resolve) => answers.push(resolve))) as typeof globalThis.fetch;
    setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const held = attachTrackerTicket(REF, true);
    const ctx = { ...CTX, key: held.key };
    const { getByTestId, unmount } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx, ticket: { ...TICKET, key: held.key }, attachments: [], canAttach: true, open: true, onFold: () => {} },
    });
    const input = getByTestId('ticket-attach-input') as HTMLInputElement;
    Object.defineProperty(input, 'files', { configurable: true, value: [new File(['a'], 'report.pdf')] });
    await fireEvent.change(input);
    await flush();
    unmount();
    held.release();
    answers.shift()!(new Response(JSON.stringify({ code: 'tracker_upload_size_changed', message: 'select the file again and attach it' }), { status: 400 }));
    await waitFor(() => expect(getToasts().map((t) => t.message)).toContainEqual(expect.stringContaining('report.pdf')));
  });

  it('saves the attachment its row menu was opened on', async () => {
    setBindingMock('SubscribeTrackerTicket', async () => ({ id: 'sub-1', seq: 1, loaded: true, read_at: 1 }));
    setBindingMock('SetTrackerSubscriptionVisible', async () => {});
    setBindingMock('ReleaseTrackerSubscription', async () => {});
    const save = setBindingMock('SaveTrackerAttachment', async () => '/home/user/Downloads/new.pdf');
    const held = attachTrackerTicket(REF, true);
    await flush();
    const { getAllByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx: { ...CTX, key: held.key }, ticket: { ...TICKET, key: held.key }, attachments: [file('a1', 'old.pdf', 1), file('a2', 'new.pdf', 2)], canAttach: true, open: true, onFold: () => {} },
    });
    await fireEvent.click(getAllByTestId('ticket-attachment-menu')[0]);
    await fireEvent.click(await waitFor(() => document.body.querySelector<HTMLElement>('[data-testid="ticket-attachment-save"]')!));
    await waitFor(() => expect(save).toHaveBeenCalledWith('sub-1', 'a2'));
    held.release();
  });

  it('offers Open only for an image small enough to preview here, and Save or the tracker for a larger one', async () => {
    const { getAllByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: {
        ctx: CTX, ticket: TICKET, canAttach: true, open: true, onFold: () => {},
        attachments: [file('a1', 'small.png', 2, 1_000_000, 'image/png'), file('a2', 'huge.png', 1, 40 * 1024 * 1024, 'image/png')],
      },
    });
    const menu = async (row: number) => {
      await fireEvent.click(getAllByTestId('ticket-attachment-menu')[row]);
      await waitFor(() => expect(document.body.querySelector('[data-testid="ticket-attachment-save"]')).not.toBeNull());
      const offered = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitem"]')).map((m) => m.dataset.testid);
      await fireEvent.keyDown(document.body, { key: 'Escape' });
      await waitFor(() => expect(document.body.querySelector('[data-testid="ticket-attachment-save"]')).toBeNull());
      return offered;
    };
    expect(await menu(0)).toContain('ticket-attachment-open');
    const huge = await menu(1);
    expect(huge).not.toContain('ticket-attachment-open');
    expect(huge).toContain('ticket-attachment-save');
  });

  it('offers Open in the tracker once when a connected browser saves a large file there', async () => {
    await pairWithScopes(['threads:read', 'tracker:read']);
    try {
      const { getAllByTestId } = render(TicketAttachments, {
        context: ticketWriteSurfaceContext(() => true), props: { ctx: CTX, ticket: TICKET, canAttach: true, open: true, onFold: () => {}, attachments: [file('a1', 'huge.zip', 1, BROWSER_SAVE_MAX_BYTES + 1, 'application/zip')] },
      });
      await fireEvent.click(getAllByTestId('ticket-attachment-menu')[0]);
      const save = await waitFor(() => document.body.querySelector<HTMLElement>('[data-testid="ticket-attachment-save"]')!);
      expect(save.textContent?.trim()).toBe('Open in the tracker');
      const labels = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitem"]')).map((m) => m.textContent?.trim());
      expect(labels.filter((l) => l === 'Open in the tracker')).toHaveLength(1);
    } finally {
      resetToLocalPage();
    }
  });

  it('offers no Attach where the tracker stores no files', () => {
    const { queryByTestId } = render(TicketAttachments, {
      context: ticketWriteSurfaceContext(() => true), props: { ctx: CTX, ticket: TICKET, attachments: [], canAttach: false, open: true, onFold: () => {} },
    });
    expect(queryByTestId('ticket-attach')).toBeNull();
  });
});

describe('the poker section', () => {
  const attachment = (onScreen: boolean[]): TrackerTicketAttachment =>
    ({ key: CTX.key, ref: REF, current: null, error: null, tracker: null, setShown: () => {}, setPokerShown: (v: boolean) => onScreen.push(v), release: () => {} });
  const view = (revealed: boolean, votes: Array<[string, number | null, boolean?]>): PokerView => ({
    loaded: true, revealed, read_at: 0,
    votes: votes.map(([name, value, mine]) => ({ voter: { id: name, name }, value, mine })),
  } as PokerView);

  it('shows each vote as the read gives it, and tells its surface while it is on screen until it goes', async () => {
    // The scroller's viewport as the observer reports it, by the test.
    const observers: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
    vi.stubGlobal('IntersectionObserver', class {
      constructor(report: (entries: Array<{ isIntersecting: boolean }>) => void) {
        observers.push(report);
      }
      observe(): void {}
      disconnect(): void {}
    });
    const onScreen: boolean[] = [];
    const { getAllByTestId, unmount } = render(TicketPoker, { context: ticketWriteSurfaceContext(() => true),
      props: { attachment: attachment(onScreen), ctx: CTX, poker: { view: view(false, [['User 1', 5, true], ['User 2', null]]), error: null }, onSetEstimate: null, hours: false },
    });
    expect(getAllByTestId('ticket-poker-vote').map((v) => v.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['User 1 (you) 5', 'User 2 voted']);
    expect(onScreen).toEqual([]);
    observers[0]([{ isIntersecting: true }]);
    observers[0]([{ isIntersecting: false }]);
    observers[0]([{ isIntersecting: true }]);
    unmount();
    expect(onScreen).toEqual([true, false, true, false]);
    vi.unstubAllGlobals();
  });

  it('clears everyone\'s votes only on the second press', async () => {
    const clear = setBindingMock('ClearTrackerPokerVotes', async () => ({ parts: [{ step: 'poker', state: 'applied' }] }));
    const { getByTestId } = render(TicketPoker, { context: ticketWriteSurfaceContext(() => true),
      props: { attachment: attachment([]), ctx: CTX, poker: { view: view(false, []), error: null }, onSetEstimate: null, hours: false },
    });
    await fireEvent.click(getByTestId('ticket-poker-clear'));
    expect(clear).not.toHaveBeenCalled();
    expect(getByTestId('ticket-poker-clear').textContent).toBe('Clear everyone\'s votes');
    await fireEvent.click(getByTestId('ticket-poker-clear'));
    expect(clear).toHaveBeenCalledWith('c1', '10101');
  });

  it('offers the estimate from each value voted once revealed, most votes first, or says where to set it', async () => {
    const set = vi.fn();
    const poker = { view: view(true, [['User 1', 3], ['User 2', 5], ['User 3', 5]]), error: null };
    const { getAllByTestId, rerender, getByTestId } = render(TicketPoker, { context: ticketWriteSurfaceContext(() => true), props: { attachment: attachment([]), ctx: CTX, poker, onSetEstimate: set, hours: false } });
    const offers = getAllByTestId('ticket-poker-set-estimate');
    expect(offers.map((o) => o.textContent?.replace(/\s+/g, ' ').trim())).toEqual(['Set estimate to 5 (2 votes)', 'Set estimate to 3 (1 vote)']);
    await fireEvent.click(offers[0]);
    expect(set).toHaveBeenCalledWith(5);
    await rerender({ attachment: attachment([]), ctx: CTX, poker, onSetEstimate: set, hours: true });
    expect(getAllByTestId('ticket-poker-set-estimate')[0].textContent?.replace(/\s+/g, ' ').trim()).toBe('Set estimate to 5 hours (2 votes)');
    await rerender({ attachment: attachment([]), ctx: CTX, poker, onSetEstimate: null, hours: false });
    expect(getByTestId('ticket-poker-no-row').textContent).toBe('Open the ticket\'s board in Issues to set its estimate.');
  });

  it('takes no vote while a poker write is out, and waits for the estimate being set', async () => {
    const answers: Array<(view: WriteView) => void> = [];
    setBindingMock('VoteTrackerPoker', () => new Promise<WriteView>((resolve) => answers.push(resolve)));
    setBindingMock('SetTrackerEstimate', () => new Promise<WriteView>((resolve) => answers.push(resolve)));
    const poker = { view: view(true, [['User 1', 3, true]]), error: null };
    const { getAllByTestId, getByTestId } = render(TicketPoker, { context: ticketWriteSurfaceContext(() => true), props: { attachment: attachment([]), ctx: CTX, poker, onSetEstimate: (v: number) => { void setTicketPoints(CTX, '1', v); }, hours: false } });
    await fireEvent.click(getAllByTestId('ticket-poker-card')[1]);
    expect(getAllByTestId('ticket-poker-card').every((c) => (c as HTMLButtonElement).disabled)).toBe(true);
    expect((getByTestId('ticket-poker-typed') as HTMLInputElement).disabled).toBe(true);
    await fireEvent.click(getByTestId('ticket-poker-set-estimate'));
    expect((getByTestId('ticket-poker-set-estimate') as HTMLButtonElement).disabled).toBe(true);
    for (const answer of answers) answer({ parts: [{ step: 'poker', state: 'applied' }] } as WriteView);
    await waitFor(() => expect((getByTestId('ticket-poker-typed') as HTMLInputElement).disabled).toBe(false));
    await waitFor(() => expect((getByTestId('ticket-poker-set-estimate') as HTMLButtonElement).disabled).toBe(false));
  });

  it('refuses a typed vote outside 0 to 1000 and sends one inside', async () => {
    const vote = setBindingMock('VoteTrackerPoker', async () => ({ parts: [{ step: 'poker', state: 'applied' }] }));
    const { getByTestId } = render(TicketPoker, { context: ticketWriteSurfaceContext(() => true), props: { attachment: attachment([]), ctx: CTX, poker: { view: view(false, []), error: null }, onSetEstimate: null, hours: false } });
    const typed = getByTestId('ticket-poker-typed') as HTMLInputElement;
    await fireEvent.input(typed, { target: { value: '1001' } });
    expect(getByTestId('ticket-poker-typed-invalid').textContent).toBe('A vote is a number from 0 to 1000.');
    expect((getByTestId('ticket-poker-typed-vote') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.input(typed, { target: { value: '40' } });
    await fireEvent.click(getByTestId('ticket-poker-typed-vote'));
    expect(vote).toHaveBeenCalledWith('c1', { issue: '10101', vote: 40 });
  });
});

describe('a file the ticket\'s text names', () => {
  it('shows the file one attachment has with its size and Save, and a name alone otherwise', async () => {
    const save = vi.fn();
    const read: TrackerMediaReader = (name) => (name === 'report.pdf' ? { name, size: 4_100_000, image: null, save, saveLabel: 'Save' } : null);
    const { container } = render(TicketMarkdown, {
      props: { source: 'See [report.pdf](media:1) and [other.pdf](media:2).' },
      context: new Map([[TRACKER_MEDIA_CONTEXT, read]]),
    });
    const chips = await waitFor(() => {
      const found = Array.from(container.querySelectorAll<HTMLElement>('[data-tracker-media]'));
      expect(found).toHaveLength(2);
      return found;
    });
    expect(chips[0].textContent?.replace(/\s+/g, ' ').trim()).toBe('report.pdf 4.1 MB Save');
    await fireEvent.click(chips[0].querySelector('button')!);
    expect(save).toHaveBeenCalled();
    expect(chips[1].textContent?.trim()).toBe('other.pdf');
    expect(chips[1].querySelector('button')).toBeNull();
  });

  it('shows the file inside embedded HTML too, whose hosts are mounted apart from the text', async () => {
    const read: TrackerMediaReader = (name) => (name === 'spec.pdf' ? { name, size: 2_000, image: null, save: () => {}, saveLabel: 'Save' } : null);
    const { container } = render(TicketMarkdown, {
      props: { source: '<div><img src="media:9" alt="spec.pdf"></div>' },
      context: new Map([[TRACKER_MEDIA_CONTEXT, read]]),
    });
    await waitFor(() => expect(container.querySelector('[data-tracker-media]')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('spec.pdf 2 KB Save'));
  });

  it('fetches an image the text shows in place only once its box comes near the viewport', async () => {
    const observers: Array<{ report: (entries: Array<{ isIntersecting: boolean }>) => void; margin: string }> = [];
    vi.stubGlobal('IntersectionObserver', class {
      constructor(report: (entries: Array<{ isIntersecting: boolean }>) => void, options: { rootMargin: string }) {
        observers.push({ report, margin: options.rootMargin });
      }
      observe(): void {}
      disconnect(): void {}
    });
    const acquire = vi.fn(() => ({ settled: undefined, value: new Promise<never>(() => {}), sized: Promise.resolve(undefined), release: () => {} }));
    const image = { key: 'img-1', filename: 'diagram.png', acquire, original: async () => new Blob(), menuTag: {} };
    const read: TrackerMediaReader = (name) => (name === 'diagram.png' ? { name, size: 10, image, save: () => {}, saveLabel: 'Save' } : null);
    const { container } = render(TicketMarkdown, {
      props: { source: '![diagram.png](media:1)' },
      context: new Map([[TRACKER_MEDIA_CONTEXT, read]]),
    });
    await waitFor(() => expect(container.querySelector('[data-tracker-media]')?.textContent).toBe('diagram.png'));
    expect(acquire).not.toHaveBeenCalled();
    const near = observers.find((o) => o.margin === '200px');
    expect(near).toBeDefined();
    near!.report([{ isIntersecting: false }]);
    await flush();
    expect(acquire).not.toHaveBeenCalled();
    near!.report([{ isIntersecting: true }]);
    await flush();
    expect(container.querySelector('[data-tracker-media]')).toBeNull();
    vi.unstubAllGlobals();
  });

  it('lets an image scrolled away release its bytes while keeping its box, and paints it again when it comes back', async () => {
    const observers: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
    vi.stubGlobal('IntersectionObserver', class {
      constructor(report: (entries: Array<{ isIntersecting: boolean }>) => void, options: { rootMargin: string }) {
        if (options.rootMargin === '200px') observers.push(report);
      }
      observe(): void {}
      disconnect(): void {}
    });
    try {
      const releases: string[] = [];
      const acquire = vi.fn(() => ({
        settled: { url: 'blob:diagram', mimeType: 'image/png', width: 0, height: 0, originalWidth: 0, originalHeight: 0, originalBytes: 10, derived: false, blob: new Blob() },
        value: new Promise<never>(() => {}),
        sized: Promise.resolve(undefined),
        release: () => releases.push('diagram'),
      }));
      const image = { key: 'img-2', filename: 'diagram.png', acquire, original: async () => new Blob(), menuTag: {} };
      const read: TrackerMediaReader = (name) => (name === 'diagram.png' ? { name, size: 10, image, save: () => {}, saveLabel: 'Save' } : null);
      // Painted before, so a mount paints from the cache in its frame.
      rememberImageTier(image.key, 0);
      const { container } = render(TicketMarkdown, {
        props: { source: '![diagram.png](media:1)' },
        context: new Map([[TRACKER_MEDIA_CONTEXT, read]]),
      });
      await waitFor(() => expect(observers).toHaveLength(1));
      observers[0]!([{ isIntersecting: true }]);
      const img = await waitFor(() => {
        const found = container.querySelector<HTMLImageElement>('img[src="blob:diagram"]');
        expect(found).not.toBeNull();
        return found!;
      });
      // The picture as the browser decoded it.
      Object.defineProperty(img, 'naturalWidth', { configurable: true, value: 640 });
      Object.defineProperty(img, 'naturalHeight', { configurable: true, value: 480 });
      expect(releases).toEqual([]);

      observers[0]!([{ isIntersecting: false }]);
      await flush();
      expect(releases).toEqual(['diagram']);
      expect(container.querySelector('img[src="blob:diagram"]')).toBeNull();
      const box = container.querySelector<HTMLElement>('[data-streamdown-image-box] [role="img"]');
      expect(box).not.toBeNull();
      expect(box!.style.width).toBe('640px');
      expect(box!.style.aspectRatio).toBe('640 / 480');

      observers[0]!([{ isIntersecting: true }]);
      await waitFor(() => expect(container.querySelector('img[src="blob:diagram"]')).not.toBeNull());
      expect(acquire).toHaveBeenCalledTimes(2);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
