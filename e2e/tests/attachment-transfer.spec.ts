// Attachment bytes on their own HTTP routes, through the shipped UI and
// the real backend: an image dropped on the composer, and the same image
// read back at full size in the lightbox.
//
// WHY THIS FILE EXISTS. Wave 6b moved the payload off the WebSocket. The
// pieces are covered below this level — the routes and their tickets in
// `internal/transport`, the mint rules in `internal/app`, the request
// shape in `attachmentTransfer.test.ts` — but every one of those tests
// answers its own question with a stub on the other side of the seam. The
// thing that only a real boot can answer is whether the two hops COMPOSE:
// a ticket minted over the WebSocket, presented on a separate HTTP
// request, by a page that resolved a relative URL against whatever origin
// it happens to be served from, under the shipped CSP, with the bytes
// arriving unchanged at both ends.
//
// WHAT IS REAL HERE. The drop is the shipped `handleDrop` on the shipped
// `Composer.svelte`, so the upload is `uploadAttachmentBytes` as it
// ships; the expand is the shipped `loadAttachmentFullSize`, so the
// download is `fetchAttachmentBytes` and the `<img>` is painted from the
// object URL it made. Nothing is stubbed on the frontend side of the
// seam.
//
// The fixture's shape is load-bearing twice over. It is 494 bytes, far
// under `shouldCompressImage`'s threshold, so the composer hands the
// upload the ORIGINAL file and byte-identity is a fair assertion. And it
// is 300x200 — WIDER than the 256px thumbnail cap — so the dimensions the
// lightbox paints could only have come from the full-size body on the
// download route. A 40x40 fixture would have thumbnailed to 40x40 and the
// same assertion would have passed with the byte routes never called.

import type { Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import { expect, test, type SeedResult } from './fixtures.js';

import { PNG_BASE64, PNG_BYTES, PNG_WIDTH, PNG_HEIGHT } from './attachment-fixture.js';

const FILENAME = 'round-trip.png';

interface AttachmentRow {
  id: string;
  threadId: string;
  filename: string;
  mimeType: string;
  size: number;
}

async function seedThread(harness: HarnessApp, title: string): Promise<string> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'attachment-transfer-app',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Seeded\n' } }] },
        threads: [
          {
            title,
            turns: [
              {
                userText: 'Here is the screenshot.',
                items: [{ kind: 'assistant_text', summary: 'Got it.' }],
              },
            ],
          },
        ],
      },
    ],
  });
  return seed.projects[0].threadIds[0];
}

/**
 * Drops one image on the composer the way a browser does: a real
 * `DataTransfer` carrying a real `File`, dispatched at the element that
 * owns `ondrop`. Everything after this line is the app's own code.
 */
async function dropImage(page: Page, filename: string, base64: string): Promise<void> {
  await page.getByTestId('composer-root').evaluate(
    (root, payload) => {
      const binary = atob(payload.base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
      const transfer = new DataTransfer();
      transfer.items.add(new File([bytes], payload.filename, { type: 'image/png' }));
      root.dispatchEvent(
        new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }),
      );
    },
    { filename, base64 },
  );
}

test('an image dropped on the composer crosses on HTTP and comes back byte-identical', async ({
  harness,
  page,
}) => {
  const threadId = await seedThread(harness, 'Attachment round trip');
  await harness.open(page);
  await page.getByText('Attachment round trip').click();
  await expect(page.getByTestId('composer-root')).toBeVisible();

  await dropImage(page, FILENAME, PNG_BASE64);

  // The chip appears only after the PUT resolved AND its response body
  // was parsed into an attachment record: the draft is stamped with what
  // the route returned, not with what the composer sent.
  const thumb = page.getByTestId('attachment-thumb');
  await expect(thumb).toBeVisible();

  // What the backend actually stored. The metadata came from the ticket
  // SUBJECT, so this is also the check that the mint's arguments survived
  // the trip to the store without the body being consulted for any of it.
  const stored = await harness.rpc<AttachmentRow[]>('ListAttachments', threadId);
  expect(stored).toHaveLength(1);
  const attachment = stored[0];
  expect(attachment.filename).toBe(FILENAME);
  expect(attachment.mimeType).toBe('image/png');
  expect(attachment.size).toBe(PNG_BYTES.length);

  // Byte-identity, read back over a ticket of this test's own. Fetched
  // from Node, which has no cookie jar at all — the ticket IS the
  // admission, and a route that had quietly grown a session requirement
  // would fail here rather than in six months on a phone.
  const minted = await harness.rpc<string>(
    'MintAttachmentDownloadTicket',
    threadId,
    attachment.id,
  );
  expect(minted.startsWith('/attachments/')).toBe(true);
  const download = await fetch(new URL(minted, harness.url));
  expect(download.status).toBe(200);
  expect(download.headers.get('content-type')).toBe('image/png');
  // A ticketed body must not be reusable from a cache by whatever holds
  // the next session on this origin.
  expect(download.headers.get('cache-control')).toBe('no-store');
  expect(Buffer.from(await download.arrayBuffer())).toEqual(PNG_BYTES);

  // Now the DOWNLOAD path as the user drives it: the lightbox asks for
  // the full-size image, which mints its own ticket and fetches over the
  // same routes from inside the page.
  await page.getByLabel(`Preview ${FILENAME}`).click();
  const expanded = page
    .getByRole('dialog', { name: FILENAME })
    .getByRole('img', { name: FILENAME });
  await expect(expanded).toBeVisible();

  // The src is an object URL over the fetched Blob, and the element
  // decoded it at the fixture's FULL dimensions — 300x200 is past the
  // 256px thumbnail cap, so this is the download route's body and not the
  // cached thumbnail the composer chip is painted from.
  await expect
    .poll(async () =>
      await expanded.evaluate((img: HTMLImageElement) => ({
        blobURL: img.src.startsWith('blob:'),
        width: img.naturalWidth,
        height: img.naturalHeight,
      })),
    )
    .toEqual({ blobURL: true, width: PNG_WIDTH, height: PNG_HEIGHT });
});

test('a transfer ticket admits one request and says nothing about the rest', async ({
  harness,
  page,
}) => {
  const threadId = await seedThread(harness, 'Ticket admission');
  await harness.open(page);
  await page.getByText('Ticket admission').click();
  await expect(page.getByTestId('composer-root')).toBeVisible();
  await dropImage(page, FILENAME, PNG_BASE64);
  await expect(page.getByTestId('attachment-thumb')).toBeVisible();

  const [attachment] = await harness.rpc<AttachmentRow[]>('ListAttachments', threadId);
  const url = async () =>
    new URL(
      await harness.rpc<string>('MintAttachmentDownloadTicket', threadId, attachment.id),
      harness.url,
    );

  // Spent once, and the second presentation is indistinguishable from a
  // path that was never a route.
  const first = await url();
  expect((await fetch(first)).status).toBe(200);
  expect((await fetch(first)).status).toBe(404);

  // A ticket names ONE attachment on ONE thread. Presented against a
  // different id it is refused, and it is spent doing so — the token is
  // consumed before the subject is compared, so a request cannot probe
  // for a path by keeping its ticket.
  const crossed = await url();
  crossed.pathname = `/attachments/${threadId}/some-other-attachment`;
  expect((await fetch(crossed)).status).toBe(404);

  // No ticket at all is the same 404, not a 403: the route does not
  // confirm that this attachment exists to a request that cannot read it.
  const bare = await url();
  bare.search = '';
  expect((await fetch(bare)).status).toBe(404);
});
