// Where a tracker upload failed decides what the person is told: before
// its PUT left (the mint, or a paired computer's route), nothing was
// attached; after, it may have been. The route's own phase is
// computerRoutes.test.ts's; this holds the upload to it.

import { describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { HOME_BACKEND } from './backendKey';
import { RequestNotSentError } from './computerRoutes';

const paired = vi.hoisted(() => ({ fetch: vi.fn<(...args: unknown[]) => Promise<Response>>() }));
vi.mock('./deviceSession', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./deviceSession')>()),
  fetchPairedComputer: (...args: unknown[]) => paired.fetch(...args),
}));

const { TransferInterruptedError, UploadNotSentError, uploadTrackerAttachmentBytes } = await import('./attachmentTransfer');

describe('a tracker upload that fails', () => {
  it('was not sent when its mint failed, or its route failed before the PUT left, and may have been once it left', async () => {
    const file = new File(['bytes'], 'report.pdf');
    setBindingMock('MintTrackerAttachmentUploadTicket', async () => {
      throw new Error('the connection is turned off');
    });
    const minted = await uploadTrackerAttachmentBytes(HOME_BACKEND, 'c1', '10101', file).catch((e: unknown) => e);
    expect(minted).toBeInstanceOf(UploadNotSentError);
    expect((minted as Error).message).toBe('Not sent: the connection is turned off');
    expect(paired.fetch).not.toHaveBeenCalled();

    setBindingMock('MintTrackerAttachmentUploadTicket', async () => '/attachments/tracker/upload?ticket=t');
    paired.fetch.mockRejectedValueOnce(new RequestNotSentError(new Error('Computer pairing changed. Reconnect to continue.')));
    const unrouted = await uploadTrackerAttachmentBytes(HOME_BACKEND, 'c1', '10101', file).catch((e: unknown) => e);
    expect(unrouted).toBeInstanceOf(UploadNotSentError);
    expect((unrouted as Error).message).toBe('Not sent: Computer pairing changed. Reconnect to continue.');

    paired.fetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    const cut = await uploadTrackerAttachmentBytes(HOME_BACKEND, 'c1', '10101', file).catch((e: unknown) => e);
    expect(cut).toBeInstanceOf(TransferInterruptedError);
  });
});
