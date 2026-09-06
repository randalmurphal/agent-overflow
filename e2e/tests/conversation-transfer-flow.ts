// Real paired hosts and the production SPA. Native session bytes are an inert
// fixture in the harness home; no real provider or developer home is reached.
import { expect, test } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { PNG_BYTES } from './attachment-fixture.js';
import { codexTransferScenario } from './conversation-transfer-helpers.js';

interface Seed { projects: Array<{ projectId: string; path: string; threadIds: string[] }> }
interface Thread { id: string; title: string; sessionRef: string; workspacePath: string; ownershipEpoch?: number }
interface Transfer { id: string; phase: string; error?: string; targetThreadId: string }

export function conversationTransferFlow(): void {
  conversationTransferForProvider('claude');
  conversationTransferForProvider('codex');
}

function conversationTransferForProvider(provider: 'claude' | 'codex'): void {
  test(`${provider}: copies independently, then moves the original with its image and workspace`, async ({ page }, info) => {
    test.setTimeout(120_000);
    let source: HarnessApp | undefined;
    let destination: HarnessApp | undefined;
    let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
    const fixtureDir = await mkdtemp(join(tmpdir(), 'ao-transfer-fixture-'));
    const errors: string[] = [];
    page.on('pageerror', (error) => errors.push(error.message));
    try {
      const scenarioFile = join(fixtureDir, 'codex.json');
      await writeFile(scenarioFile, JSON.stringify(codexTransferScenario()));
      source = await launchHarness({ env: provider === 'codex' ? { AO_MOCK_SCENARIO_FILE: scenarioFile } : {} });
      destination = await launchHarness();
      const nativeID = randomUUID();
      const original = await source.rpc<Seed>('HarnessSeed', {
        projects: [{ name: 'Source project', repo: { commits: [{ files: { 'README.md': 'Base commit' } }] }, threads: [{ provider, title: 'Portable conversation', sessionRef: nativeID, turns: [{ userText: 'Remember the workspace', items: [{ kind: 'assistant_text', summary: 'I remember this conversation.' }] }] }] }],
      });
      const nativePath = provider === 'claude'
        ? `.claude/projects/transfer-fixture/${nativeID}.jsonl`
        : `.codex/sessions/2026/09/05/rollout-2026-09-05T12-00-00-${nativeID}.jsonl`;
      const transcript = provider === 'claude'
        ? [{ type: 'user', sessionId: nativeID, uuid: randomUUID(), parentUuid: null, message: { role: 'user', content: 'Remember the workspace' } }]
        : [
          { type: 'session_meta', payload: { id: nativeID, cwd: original.projects[0].path, cli_version: '0.153.4', history_mode: 'legacy' } },
          { type: 'response_item', payload: { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Remember the workspace' }] } },
        ];
      await source.rpc('HarnessSeed', { providerHome: [{ path: nativePath, content: transcript.map((row) => JSON.stringify(row)).join('\n') + '\n' }] });
      if (provider === 'codex') {
        if (!source.bootstrap.homeDir) throw new Error('The transfer fixture requires an isolated provider home');
        await writeFile(scenarioFile, JSON.stringify(codexTransferScenario(nativeID, join(source.bootstrap.homeDir, nativePath))));
      }
      const target = await destination.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Destination project', repo: { commits: [{ files: { 'README.md': 'Other checkout' } }] } }] });
      const sourceThread = original.projects[0].threadIds[0];
      const uploadURL = await source.rpc<string>('MintAttachmentUploadTicket', sourceThread, 'portable.png', 'image/png', PNG_BYTES.length);
      const upload = await fetch(new URL(uploadURL, source.url), { method: 'PUT', body: PNG_BYTES });
      expect(upload.status, await upload.text()).toBe(200);
      await writeFile(join(original.projects[0].path, 'untracked.txt'), 'Unsaved workspace change');
      await source.open(page);
      await page.getByRole('button', { name: 'Settings', exact: true }).click();
      await page.getByRole('tab', { name: 'Connections', exact: true }).click();
      pairing = await headlessPairing(destination);
      await page.getByRole('textbox', { name: 'Pairing link' }).fill(pairing.invite.url);
      await page.getByRole('button', { name: 'Connect', exact: true }).click();
      const verification = page.getByLabel('Verification number');
      await expect(verification).toBeVisible();
      await pairing.confirm((await verification.textContent())!.trim());
      await expect(page.getByTestId('attached-system')).toContainText('Connected');
      const remoteId = (await source.rpc<Array<{ id: string }>>('ListBackends'))[0].id;
      await page.getByRole('button', { name: 'Close Settings', exact: true }).click();

      for (const kind of ['copy', 'move']) {
        const title = page.getByTestId('thread-row-title').filter({ hasText: /^Portable conversation$/ }).first();
        await expect(title).toBeVisible();
        const shell = page.getByTestId('thread-row-shell').filter({ has: title });
        if (info.project.name === 'compact') await shell.getByTestId('thread-row-menu').click();
        else await title.click({ button: 'right' });
        await page.getByRole('menuitem', { name: 'Move or copy to computer…', exact: true }).click();
        const dialog = page.getByRole('dialog', { name: 'Move or copy conversation', exact: true });
        await dialog.getByRole('radio', { name: kind === 'copy' ? 'Copy / fork' : 'Move', exact: true }).check();
        await dialog.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteId);
        await dialog.getByRole('combobox', { name: 'Project on destination', exact: true }).selectOption(target.projects[0].projectId);
        await dialog.getByRole('button', { name: kind === 'copy' ? 'Copy conversation' : 'Move conversation', exact: true }).click();
        await expect(dialog.getByTestId('conversation-transfer-status')).toBeVisible();
        // Disconnect the initiating frontend while the hosts own the transfer.
        await dialog.getByRole('button', { name: 'Close', exact: true }).click();
        await page.goto('about:blank');
        let result: Transfer | undefined;
        await expect.poll(async () => {
          const rows = await source!.rpc<Array<Transfer & { kind: string }>>('GetThreadTransfers');
          result = rows.find((row) => row.kind === kind);
          if (result?.phase === 'complete') return 'complete';
          const recipient = (await destination!.rpc<Transfer[]>('GetThreadTransfers')).find((row) => row.id === result?.id);
          return result?.error || recipient?.error || result?.phase;
        }, { timeout: 40_000 }).toBe('complete');
        const received = await destination.rpc<Thread>('GetThread', result!.targetThreadId);
        const attachments = await destination.rpc<Array<{ id: string; filename: string; size: number }>>('ListAttachments', received.id);
        expect(attachments).toHaveLength(1);
        expect(attachments[0]).toMatchObject({ filename: 'portable.png', size: PNG_BYTES.length });
        const downloadURL = await destination.rpc<string>('MintAttachmentDownloadTicket', received.id, attachments[0].id);
        const download = await fetch(new URL(downloadURL, destination.url));
        expect(download.status).toBe(200);
        expect(Buffer.from(await download.arrayBuffer())).toEqual(PNG_BYTES);
        expect(await readFile(join(received.workspacePath, 'untracked.txt'), 'utf8')).toBe('Unsaved workspace change');
        expect(received.id === sourceThread).toBe(kind === 'move');
        expect(received.sessionRef === nativeID).toBe(kind === 'move');
        const remaining = await source.rpc<Thread[] | null>('ListThreads');
        expect((remaining ?? []).some((row) => row.id === sourceThread)).toBe(kind === 'copy');
        // Name the first copy distinctly so the next menu still names the source.
        if (kind === 'copy') await destination.rpc('RenameThread', received.id, 'Independent copy');
        await source.open(page);
        await expect(page.getByTestId('thread-row-title').filter({ hasText: /^Independent copy$/ })).toBeVisible();
      }
      await expect(page.getByTestId('thread-row-title').filter({ hasText: /^Portable conversation$/ })).toHaveCount(1);
      expect(errors).toEqual([]);
    } finally {
      try { pairing?.close(); await page.close(); await destination?.close(); await source?.close(); }
      finally { await rm(fixtureDir, { recursive: true, force: true }); }
    }
  });
}
