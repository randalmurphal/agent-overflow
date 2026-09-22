// A draft project change through the production picker between paired hosts.
// Both hosts use isolated homes and mock providers; the draft executes no turn.
import { test, expect } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { PNG_BYTES } from './attachment-fixture.js';

interface Seed { projects: Array<{ projectId: string; path: string }> }
interface Thread { id: string; projectId: string; workspacePath: string; mode: string; model: string; runtimeMode: string }
interface Draft { content: string; attachmentIds: string[]; terminalChips: Array<{ content: string }> }

export function draftProjectTransferFlow(): void {
  test('project picker moves the draft and its attachments to another computer', async ({ page }) => {
    let source: HarnessApp | undefined;
    let target: HarnessApp | undefined;
    let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
    try {
      source = await launchHarness();
      target = await launchHarness();
      const from = await source.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Draft home project', repo: { commits: [{ files: { 'README.md': 'Source repository' } }] } }] });
      const to = await target.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Draft remote project', repo: { commits: [{ files: { 'README.md': 'Destination repository' } }] } }] });
      const original = await source.rpc<Thread>('CreateThread', { projectId: from.projects[0].projectId, title: 'Moving draft', provider: 'claude', model: 'claude-sonnet-4-6', mode: 'plan', runtimeMode: 'read-only' });
      await source.rpc('SetThreadMcpServerEnabled', original.id, 'ao-thread-tools', false);
      const ids: string[] = [];
      for (const [name, mime, bytes] of [['image.png', 'image/png', PNG_BYTES], ['notes.txt', 'text/plain', Buffer.from('draft file')]] as const) {
        const ticket = await source.rpc<string>('MintAttachmentUploadTicket', original.id, name, mime, bytes.length);
        const response = await fetch(new URL(ticket, source.url), { method: 'PUT', body: bytes });
        expect(response.status).toBe(200);
        ids.push((await response.json() as { id: string }).id);
      }
      const prompt = 'A draft for the other project [Image #1]';
      await source.rpc('SaveDraft', original.id, prompt, ids, [{ id: 'capture', label: 'shell', preview: 'captured', content: 'captured output', createdAt: 1 }], null);
      await source.open(page);
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
      pairing = await headlessPairing(target);
      await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
      await page.getByRole('button', { name: 'Connect', exact: true }).click();
      const verification = page.getByLabel('Verification number');
      await expect(verification).toBeVisible();
      await pairing.confirm((await verification.textContent())!.trim());
      await expect(page.getByTestId('attached-system')).toContainText('Connected');
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();
      await page.getByTestId('thread-row-title').filter({ hasText: /^Moving draft$/ }).click();
      const input = page.getByRole('textbox', { name: 'Message Input', exact: true });
      await expect(input).toHaveValue(prompt);
      await page.getByTestId('chat-header-project').click();
      await page.getByRole('menuitem', { name: /Draft remote project/ }).click();
      await expect(page.getByTestId('chat-header-project')).toHaveText('Draft remote project');
      await expect(input).toHaveValue(prompt);
      await expect(page.getByTestId('attachment-thumb')).toHaveCount(1);
      await expect(page.getByTestId('composer-root')).toContainText('notes.txt');
      const rows = await target.rpc<Thread[]>('HarnessListThreadRows');
      expect(rows).toHaveLength(1);
      expect(rows[0]).toMatchObject({ projectId: to.projects[0].projectId, workspacePath: to.projects[0].path, mode: original.mode, model: original.model, runtimeMode: original.runtimeMode });
      expect(await target.rpc<Array<{ name: string; disabled: boolean }>>('ListThreadMcpServers', rows[0].id)).toContainEqual(expect.objectContaining({ name: 'ao-thread-tools', disabled: true }));
      const moved = await target.rpc<Draft>('GetDraft', rows[0].id);
      expect(moved.attachmentIds).toHaveLength(2);
      expect(moved.terminalChips).toEqual([expect.objectContaining({ content: 'captured output' })]);
      const ticket = await target.rpc<string>('MintAttachmentDownloadTicket', rows[0].id, moved.attachmentIds[0]);
      const image = await fetch(new URL(ticket, target.url));
      expect(image.status).toBe(200);
      expect(Buffer.from(await image.arrayBuffer())).toEqual(PNG_BYTES);
      await expect.poll(async () => (await source!.rpc<Thread[]>('HarnessListThreadRows')).length).toBe(0);
      await expect(page.locator('[role="alert"].text-error')).toHaveCount(0);
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
}
