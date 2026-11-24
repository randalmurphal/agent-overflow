// An attachment's bytes carried between a page and a paired computer
// through this computer's relay (`/backend/<id>/attachments/`,
// backendproxy.CarryTransfer): a file uploaded from the page lands on the
// paired computer byte for byte and downloads back the same way, and a
// refusal the paired computer gives before it has read the body reaches
// the page as its own status and sentence. The relay's timing (stalls, a
// withheld answer, an early answer to a page that stopped sending) is
// proved over real sockets in internal/backendproxy/transfer_test.go.
import { test, expect } from './base-test.js';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { connectedAfterConfirm, headlessPairing } from './headless-pairing-helpers.js';
import { noisePng } from './attachment-fixture.js';

interface Seed { projects: Array<{ projectId: string }> }

// About 4 MiB, so each body crosses the relay in many reads. Only an image
// is served back (MintAttachmentDownloadTicket).
const IMAGE = noisePng(1200, 1200);

test('a paired computer\'s attachment crosses this computer\'s relay both ways', async ({ page }) => {
  let source: HarnessApp | undefined;
  let target: HarnessApp | undefined;
  let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
  try {
    source = await launchHarness();
    target = await launchHarness();
    const seeded = await target.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Relay target project', repo: { commits: [{ files: { 'README.md': 'Relay target' } }] } }] });
    const thread = await target.rpc<{ id: string }>('CreateThread', { projectId: seeded.projects[0].projectId, title: 'Relay transfer', provider: 'claude', model: 'claude-sonnet-4-6', mode: 'plan', runtimeMode: 'read-only' });
    await source.open(page);
    await page.getByRole('button', { name: 'Settings', exact: true }).click();
    await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
    pairing = await headlessPairing(target);
    await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
    await page.getByRole('button', { name: 'Connect', exact: true }).click();
    const verification = page.getByLabel('Verification number');
    await expect(verification).toBeVisible();
    await pairing.confirm((await verification.textContent())!.trim());
    await connectedAfterConfirm({ source, target }, (wait) => expect(page.getByTestId('attached-system')).toContainText('Connected', wait));
    const relay = `/backend/${encodeURIComponent((await source.rpc<Array<{ id: string }>>('ListBackends'))[0].id)}`;

    const encoded = IMAGE.toString('base64');
    const carry = (url: string, method: 'PUT' | 'GET') => page.evaluate(async ({ url, method, encoded }) => {
      const body = method === 'PUT' ? Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0)) : undefined;
      const response = await fetch(url, { method, body });
      const bytes = new Uint8Array(await response.arrayBuffer());
      let binary = '';
      for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
      return { status: response.status, body: btoa(binary) };
    }, { url, method, encoded });
    const text = (body: string) => Buffer.from(body, 'base64').toString();

    const upload = await target.rpc<string>('MintAttachmentUploadTicket', thread.id, 'relayed.png', 'image/png', IMAGE.length);
    const stored = await carry(relay + upload, 'PUT');
    expect(stored.status, text(stored.body)).toBe(200);
    const listed = await target.rpc<Array<{ id: string; filename: string; size: number }>>('ListAttachments', thread.id);
    expect(listed).toEqual([expect.objectContaining({ filename: 'relayed.png', size: IMAGE.length })]);

    const download = await target.rpc<string>('MintAttachmentDownloadTicket', thread.id, listed[0].id);
    const fetched = await carry(relay + download, 'GET');
    expect(fetched.status).toBe(200);
    expect(Buffer.from(fetched.body, 'base64').equals(IMAGE), 'the downloaded bytes are the uploaded ones').toBe(true);

    // A ticket for one byte with a 4 MiB body: the paired computer refuses
    // the declared length before reading any of it.
    const small = await target.rpc<string>('MintAttachmentUploadTicket', thread.id, 'refused.png', 'image/png', 1);
    const refused = await carry(relay + small, 'PUT');
    expect({ status: refused.status, text: text(refused.body) }).toEqual({ status: 400, text: 'size mismatch\n' });
    expect(await target.rpc<unknown[]>('ListAttachments', thread.id)).toHaveLength(1);
  } finally {
    try {
      if (!page.isClosed()) await page.goto('about:blank');
    } finally {
      try { await pairing?.close(); } finally {
        try { await target?.stop(); } finally { await source?.stop(); }
      }
    }
  }
});
