// Draft project selection preserves editor content, uploads, settings and
// durable attachments. Source cleanup uses the normal empty/worktree policy.
import { test, expect, type SeedResult } from './fixtures.js';
import { PNG_BASE64, PNG_BYTES } from './attachment-fixture.js';

interface ThreadRow {
  id: string; title: string; projectId: string; workspacePath: string; worktreePath?: string;
  provider: string; model: string; mode: string; reasoningEffort: string; runtimeMode: string;
}
interface DraftRow { content: string; attachmentIds: string[]; terminalChips: Array<{ content: string }> }

export function draftProjectMoveFlow(): void {
  for (const worktree of [false, true]) {
    test(`project switch carries an uploading image and file; source ${worktree ? 'keeps its worktree' : 'cleans up'}`, async ({ harness, page }, info) => {
      const seed = await harness.rpc<SeedResult>('HarnessSeed', {
        projects: [
          { name: 'Draft source', repo: { commits: [{ message: 'initial', files: { 'README.md': 'source' } }] } },
          { name: 'Draft destination', repo: { commits: [{ message: 'initial', files: { 'README.md': 'destination' } }] } },
        ],
      });
      let source = await harness.rpc<ThreadRow>('CreateThread', {
        projectId: seed.projects[0].projectId, title: 'Portable draft', provider: 'claude', model: 'claude-sonnet-4-6', mode: 'plan', reasoningEffort: 'high', runtimeMode: 'read-only',
      });
      if (worktree) source = await harness.rpc<ThreadRow>('PrepareThreadWorktree', source.id, '', 'draft-feature', false);
      await harness.rpc('SetThreadMcpServerEnabled', source.id, 'ao-thread-tools', false);
      await harness.rpc('SaveDraft', source.id, 'Move this prompt', [], [{ id: 'capture', label: 'shell', preview: 'captured', content: 'captured terminal output', createdAt: 1 }], null);
      await harness.open(page);
      await page.getByTestId('thread-row-title').filter({ hasText: /^Portable draft$/ }).click();
      const input = page.getByRole('textbox', { name: 'Message Input', exact: true });
      await expect(input).toHaveValue('Move this prompt');
      await input.fill('Latest unsaved prompt');

      let release!: () => void;
      let reached!: () => void;
      const uploading = new Promise<void>((resolve) => { reached = resolve; });
      const held = new Promise<void>((resolve) => { release = resolve; });
      await page.route('**/attachments/upload?*', async (route) => { reached(); await held; await route.continue(); });
      await page.getByTestId('composer-root').evaluate((root, base64) => {
        const transfer = new DataTransfer();
        transfer.items.add(new File([Uint8Array.from(atob(base64), (c) => c.charCodeAt(0))], 'draft.png', { type: 'image/png' }));
        transfer.items.add(new File(['attached file contents'], 'draft.txt', { type: 'text/plain' }));
        root.dispatchEvent(new DragEvent('drop', { dataTransfer: transfer, bubbles: true, cancelable: true }));
      }, PNG_BASE64);
      await uploading;
      await page.getByTestId('chat-header-project').click();
      await page.getByRole('menuitem', { name: /Draft destination/ }).click();
      await expect(input).toBeDisabled();
      release();
      await expect(page.getByTestId('chat-header-project')).toHaveText('Draft destination');
      await expect(input).toHaveValue(/Latest unsaved prompt.*\[Image #1\]/s);
      await expect(page.getByTestId('attachment-thumb')).toHaveCount(1);
      await expect(page.getByTestId('composer-root')).toContainText('draft.txt');

      const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
      const target = rows.find((row) => row.projectId === seed.projects[1].projectId)!;
      expect(target).toBeDefined();
      expect(target).toMatchObject({ provider: source.provider, model: source.model, mode: source.mode, reasoningEffort: source.reasoningEffort, runtimeMode: source.runtimeMode, workspacePath: seed.projects[1].path });
      expect(target.worktreePath || '').toBe('');
      expect(await harness.rpc<Array<{ name: string; disabled: boolean }>>('ListThreadMcpServers', target.id)).toContainEqual(expect.objectContaining({ name: 'ao-thread-tools', disabled: true }));
      const draft = await harness.rpc<DraftRow>('GetDraft', target.id);
      expect(draft.attachmentIds).toHaveLength(2);
      expect(draft.terminalChips).toEqual([expect.objectContaining({ content: 'captured terminal output' })]);
      const ticket = await harness.rpc<string>('MintAttachmentDownloadTicket', target.id, draft.attachmentIds[0]);
      const bytes = await fetch(new URL(ticket, harness.url));
      expect(bytes.status).toBe(200);
      expect(Buffer.from(await bytes.arrayBuffer())).toEqual(PNG_BYTES);
      if (worktree) {
        const remaining = rows.find((row) => row.id === source.id);
        expect(remaining?.worktreePath).toBe(source.worktreePath);
        expect(await harness.rpc<DraftRow>('GetDraft', source.id)).toMatchObject({ content: '', attachmentIds: [], terminalChips: [] });
      } else {
        await expect.poll(async () => (await harness.rpc<ThreadRow[]>('HarnessListThreadRows')).some((row) => row.id === source.id)).toBe(false);
      }
      await page.reload();
      if (info.project.name === 'compact') await page.getByRole('group', { name: 'Project: Draft destination', exact: true }).getByTestId('thread-row-title').click();
      await expect(input).toHaveValue(draft.content);
      await expect(page.getByTestId('attachment-thumb')).toHaveCount(1);
      await page.getByTestId('chat-header-project').click();
      await page.getByRole('menuitem', { name: /Draft source/ }).click();
      await expect(page.getByTestId('chat-header-project')).toHaveText('Draft source');
      await expect(input).toHaveValue(draft.content);
      await expect(page.getByTestId('attachment-thumb')).toHaveCount(1);
      await expect(page.locator('[role="alert"].text-error')).toHaveCount(0);
    });
  }

  test('an empty worktree draft carries its conversation settings before the first prompt', async ({ harness, page }) => {
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [
        { name: 'Empty draft source', repo: { commits: [{ files: { 'README.md': 'source' } }] } },
        { name: 'Empty draft destination', repo: { commits: [{ files: { 'README.md': 'target' } }] } },
      ],
    });
    const source = await harness.rpc<ThreadRow>('CreateThread', {
      projectId: seed.projects[0].projectId, title: 'Empty portable draft', provider: 'claude', model: 'claude-sonnet-4-6', mode: 'chat',
    });
    await harness.rpc('PrepareThreadWorktree', source.id, '', 'empty-draft', false);
    await harness.rpc('SetThreadMcpServerEnabled', source.id, 'ao-thread-tools', false);
    await harness.rpc('SaveDraft', source.id, 'Temporary prompt', [], [], null);
    await harness.open(page);
    await page.getByTestId('thread-row-title').filter({ hasText: /^Empty portable draft$/ }).click();
    const input = page.getByRole('textbox', { name: 'Message Input', exact: true });
    await expect(input).toHaveValue('Temporary prompt');
    await input.fill('');
    await expect.poll(() => harness.rpc<DraftRow>('GetDraft', source.id)).toMatchObject({ content: '' });
    await page.getByTestId('chat-header-project').click();
    await page.getByRole('menuitem', { name: /Empty draft destination/ }).click();
    await expect(page.getByTestId('chat-header-project')).toHaveText('Empty draft destination');
    await input.fill('The first prompt keeps the selected tools');
    await expect.poll(async () => {
      const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
      const target = rows.find((row) => row.projectId === seed.projects[1].projectId);
      if (!target) return null;
      return harness.rpc<DraftRow>('GetDraft', target.id);
    }).toMatchObject({ content: 'The first prompt keeps the selected tools' });
    const rows = await harness.rpc<ThreadRow[]>('HarnessListThreadRows');
    const target = rows.find((row) => row.projectId === seed.projects[1].projectId)!;
    expect(await harness.rpc<Array<{ name: string; disabled: boolean }>>('ListThreadMcpServers', target.id))
      .toContainEqual(expect.objectContaining({ name: 'ao-thread-tools', disabled: true }));
    expect(await harness.rpc<DraftRow>('GetDraft', source.id)).toMatchObject({ content: '', attachmentIds: [] });
  });

}
