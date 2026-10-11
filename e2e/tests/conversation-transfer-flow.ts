// Real paired hosts and the production SPA. Native session bytes are an inert
// fixture in the harness home; no real provider or developer home is reached.
import { expect, test } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { startTogether } from './launch-helpers.js';
import { headlessPairing } from './headless-pairing-helpers.js';
import { PNG_BYTES } from './attachment-fixture.js';
import { codexTransferScenario } from './conversation-transfer-helpers.js';
import { RESULT_LINE, claudeTurnsScenario, emit, listItems, textLines, toolResultLine, toolUseLine } from './agent-visibility-helpers.js';

interface Seed { projects: Array<{ projectId: string; path: string; threadIds: string[] }> }
interface Thread { id: string; title: string; sessionRef: string; workspacePath: string; ownershipEpoch?: number }
interface Transfer { id: string; threadId: string; kind: string; phase: string; error?: string; targetThreadId: string }

// One paired pair of hosts serves every provider's case. Each case seeds and
// names its own source thread, so the other case's transfers cannot satisfy
// its assertions. The pairing itself is covered by connected-computers-flow.
export function conversationTransferFlow(providers: Array<'claude' | 'codex'> = ['claude', 'codex']): void {
  test.describe('conversation transfer between paired computers', () => {
    let source: HarnessApp;
    let destination: HarnessApp;
    let remoteId = '';
    let fixtureDir = '';
    let scenarioFile = '';

    test.beforeAll(async ({ browser }) => {
      fixtureDir = await mkdtemp(join(tmpdir(), 'ao-transfer-fixture-'));
      scenarioFile = join(fixtureDir, 'codex.json');
      await writeFile(scenarioFile, JSON.stringify(codexTransferScenario()));
      // The Codex scenario names its provider; Claude mocks ignore it.
      [source, destination] = await startTogether(
        launchHarness({ env: { AO_MOCK_SCENARIO_FILE: scenarioFile } }),
        launchHarness(),
      );
      const context = await browser.newContext();
      const errors: string[] = [];
      let pairing: Awaited<ReturnType<typeof headlessPairing>> | undefined;
      try {
        const page = await context.newPage();
        page.on('pageerror', (error) => errors.push(error.message));
        await source.open(page);
        await page.getByRole('button', { name: 'Settings', exact: true }).click();
        await page.getByRole('tab', { name: 'Connect to a computer', exact: true }).click();
        pairing = await headlessPairing(destination);
        await page.getByRole('textbox', { name: /^(Computer address or pairing link|Pairing link)$/ }).fill(pairing.invite.url);
        await page.getByRole('button', { name: 'Connect', exact: true }).click();
        const verification = page.getByLabel('Verification number');
        await expect(verification).toBeVisible();
        await pairing.confirm((await verification.textContent())!.trim());
        await expect(page.getByTestId('attached-system')).toContainText('Connected');
        remoteId = (await source.rpc<Array<{ id: string }>>('ListBackends'))[0].id;
      } finally {
        pairing?.close();
        await context.close();
      }
      expect(errors).toEqual([]);
    });

    test.afterAll(async () => {
      try { await destination?.close(); await source?.close(); }
      finally { if (fixtureDir) await rm(fixtureDir, { recursive: true, force: true }); }
    });

    for (const provider of providers) {
      test(`${provider}: copies independently, then moves the original with its image and workspace`, async ({ page }, info) => {
        test.setTimeout(120_000);
        const portable = `Portable ${provider} conversation`;
        const independent = `Independent ${provider} copy`;
        const errors: string[] = [];
        page.on('pageerror', (error) => errors.push(error.message));
        try {
          const nativeID = randomUUID();
          const original = await source.rpc<Seed>('HarnessSeed', {
            projects: [{ name: `Source ${provider} project`, repo: { commits: [{ files: { 'README.md': 'Base commit' } }] }, threads: [{ provider, title: portable, sessionRef: nativeID, turns: [{ userText: 'Remember the workspace', items: [{ kind: 'assistant_text', summary: 'I remember this conversation.' }] }] }] }],
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
          const target = await destination.rpc<Seed>('HarnessSeed', { projects: [{ name: `Destination ${provider} project`, repo: { commits: [{ files: { 'README.md': 'Other checkout' } }] } }] });
          const sourceThread = original.projects[0].threadIds[0];
          const uploadURL = await source.rpc<string>('MintAttachmentUploadTicket', sourceThread, 'portable.png', 'image/png', PNG_BYTES.length);
          const upload = await fetch(new URL(uploadURL, source.url), { method: 'PUT', body: PNG_BYTES });
          expect(upload.status, await upload.text()).toBe(200);
          await writeFile(join(original.projects[0].path, 'untracked.txt'), 'Unsaved workspace change');
          await source.open(page);

          for (const kind of ['copy', 'move']) {
            const title = page.getByTestId('thread-row-title').filter({ hasText: new RegExp(`^${portable}$`) }).first();
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
              const rows = await source.rpc<Transfer[]>('GetThreadTransfers');
              result = rows.find((row) => row.threadId === sourceThread && row.kind === kind);
              if (result?.phase === 'complete') return 'complete';
              const recipient = (await destination.rpc<Transfer[]>('GetThreadTransfers')).find((row) => row.id === result?.id);
              return result?.error || recipient?.error || result?.phase;
            }, { timeout: 40_000, intervals: [250] }).toBe('complete');
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
            if (kind === 'copy') await destination.rpc('RenameThread', received.id, independent);
            await source.open(page);
            await expect(page.getByTestId('thread-row-title').filter({ hasText: new RegExp(`^${independent}$`) })).toBeVisible();
          }
          await expect(page.getByTestId('thread-row-title').filter({ hasText: new RegExp(`^${portable}$`) })).toHaveCount(1);
          expect(errors).toEqual([]);
        } finally {
          await page.close();
        }
      });
    }

    // A transfer stops the source session, and a recurring CronCreate job
    // (claude-wire.md §E11) ends with it instead of holding the transfer.
    // The dialog names the job before the user commits. A copy and then a
    // move each go through, and each leaves the source with no job: the
    // stop ended the process that held it.
    test('names the recurring job a transfer ends, then copies and moves the conversation', async ({ page }, info) => {
      test.setTimeout(150_000);
      const title = 'Looping Claude conversation';
      const reply = 'Checking the deploy every five minutes.';
      const errors: string[] = [];
      page.on('pageerror', (error) => errors.push(error.message));
      const nativeID = randomUUID();
      try {
        const original = await source.rpc<Seed>('HarnessSeed', {
          projects: [{ name: 'Looping source project', repo: { commits: [{ files: { 'README.md': 'Base commit' } }] }, threads: [{ provider: 'claude', title, sessionRef: nativeID, turns: [{ userText: 'Watch the deploy', items: [{ kind: 'assistant_text', summary: 'Watching.' }] }] }] }],
        });
        // The mock resumes this session and appends its rows to the same file.
        await source.rpc('HarnessSeed', { providerHome: [{ path: `.claude/projects/mock/${nativeID}.jsonl`, content: JSON.stringify({ type: 'user', sessionId: nativeID, uuid: randomUUID(), parentUuid: null, message: { role: 'user', content: 'Watch the deploy' } }) + '\n' }] });
        // Every session resuming this conversation arms the same job.
        await source.rpc('HarnessSetScenario', {
          sessionRef: nativeID,
          scenario: claudeTurnsScenario('transfer-recurring-job', [[emit([
            toolUseLine('msg-loop-cron', 'toolu_loop_cron', 'CronCreate', { cron: '*/5 * * * *', prompt: 'Check the deploy log.' }),
            toolResultLine('toolu_loop_cron', 'Scheduled recurring job 5f0c1a2b (Every 5 minutes). Session-only (not written to disk, dies when Claude exits). Auto-expires after 7 days. Use CronDelete to cancel sooner.', {
              toolUseResult: { id: '5f0c1a2b', humanSchedule: 'Every 5 minutes', recurring: true, durable: false },
            }),
            ...textLines('msg-loop-armed', reply),
            RESULT_LINE,
          ])]]),
        });
        const target = await destination.rpc<Seed>('HarnessSeed', { projects: [{ name: 'Looping destination project', repo: { commits: [{ files: { 'README.md': 'Other checkout' } }] } }] });
        const sourceThread = original.projects[0].threadIds[0];
        const exits = (mockId: string) => source.countEvents<{ mockId: string; report: { kind: string } }>('harness:mock', (ev) => ev.mockId === mockId && ev.report.kind === 'exiting');
        const recurringJobs = () => source.rpc<Array<{ id: string; cron: string }>>('ListThreadRecurringJobs', sourceThread);
        // The screenshots below show the destination's name; keep the
        // host's own name out of them.
        await source.rpc('RenameBackend', remoteId, 'Laptop');

        for (const [round, kind] of (['copy', 'move'] as const).entries()) {
          const used = new Set((await source.rpc<Array<{ mockId: string }>>('HarnessListMocks')).map((mock) => mock.mockId));
          await source.rpc('SendMessage', sourceThread, 'Check the deploy every five minutes.', null);
          await expect.poll(async () => (await listItems(source, sourceThread)).filter((item) => item.summary === reply).length, { timeout: 20_000 }).toBe(round + 1);
          const mocks = await source.rpc<Array<{ mockId: string; registration: { resumeRef?: string } }>>('HarnessListMocks');
          const mockId = mocks.find((mock) => mock.registration.resumeRef === nativeID && !used.has(mock.mockId))?.mockId;
          expect(mockId, 'the session holding the job runs on a mock').toBeTruthy();
          expect(await recurringJobs()).toEqual([{ id: '5f0c1a2b', cron: '*/5 * * * *' }]);

          await source.open(page);
          const row = page.getByTestId('thread-row-title').filter({ hasText: new RegExp(`^${title}$`) }).first();
          await expect(row).toBeVisible();
          if (info.project.name === 'compact') await page.getByTestId('thread-row-shell').filter({ has: row }).getByTestId('thread-row-menu').click();
          else await row.click({ button: 'right' });
          await page.getByRole('menuitem', { name: 'Move or copy to computer…', exact: true }).click();
          const dialog = page.getByRole('dialog', { name: 'Move or copy conversation', exact: true });
          await dialog.getByRole('radio', { name: kind === 'copy' ? 'Copy / fork' : 'Move', exact: true }).check();
          await dialog.getByRole('combobox', { name: 'Computer', exact: true }).selectOption(remoteId);
          await dialog.getByRole('combobox', { name: 'Project on destination', exact: true }).selectOption(target.projects[0].projectId);
          // Soft, so a run without the note still records what the transfer does.
          await expect.soft(dialog.getByTestId('transfer-recurring-jobs')).toHaveText('The transfer stops this conversation’s running session, which ends its recurring job: */5 * * * * (5f0c1a2b).');
          await page.screenshot({ path: info.outputPath(`transfer-recurring-job-${kind}.png`) });

          await dialog.getByRole('button', { name: kind === 'copy' ? 'Copy conversation' : 'Move conversation', exact: true }).click();
          await expect(dialog.getByTestId('conversation-transfer-status').or(dialog.getByRole('alert'))).toBeVisible();
          await page.screenshot({ path: info.outputPath(`transfer-recurring-job-${kind}-started.png`) });
          await expect(dialog.getByRole('alert')).toHaveCount(0);
          let result: Transfer | undefined;
          await expect.poll(async () => {
            result = (await source.rpc<Transfer[]>('GetThreadTransfers')).find((value) => value.threadId === sourceThread && value.kind === kind);
            if (result?.phase === 'complete') return 'complete';
            const recipient = (await destination.rpc<Transfer[]>('GetThreadTransfers')).find((value) => value.id === result?.id);
            return result?.error || recipient?.error || result?.phase;
          }, { timeout: 40_000, intervals: [250] }).toBe('complete');
          expect(exits(mockId!), 'the source session holding the job still runs').toBe(1);
          // Soft, so a run that keeps the job still records the move.
          await expect.configure({ soft: true }).poll(recurringJobs, { message: `the source still lists the job its ${kind} ended` }).toEqual([]);
          if (kind === 'copy') {
            // Name the copy distinctly so the next menu still names the source.
            await destination.rpc('RenameThread', result!.targetThreadId, 'Looping Claude copy');
            await dialog.getByRole('button', { name: 'Close', exact: true }).click();
          }
        }
        // The conversation lives on at the destination.
        expect((await destination.rpc<Thread>('GetThread', sourceThread)).sessionRef).toBe(nativeID);
        expect(errors).toEqual([]);
      } finally {
        await source.rpc('HarnessClearScenarios');
        await page.close();
      }
    });
  });
}
