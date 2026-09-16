// The saved v0.0.14 release creates local state, then the current binary opens
// that same data root. This checks installation upgrades, not old/new remote
// protocol compatibility. Both processes use isolated homes and mock providers.
// Run with AO_E2E_UPGRADE_BASELINE=/absolute/path/to/the/saved/binary.
import { expect, test } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { listItems, seedAgentThread } from './agent-visibility-helpers.js';

const baseline = process.env.AO_E2E_UPGRADE_BASELINE;

test('a saved release upgrades without losing conversations, pins, or drafts', async ({ page }) => {
  test.skip(!baseline, 'Requires an actual saved release binary');
  if (!baseline || !isAbsolute(baseline)) throw new Error('AO_E2E_UPGRADE_BASELINE must be an absolute binary path');
  test.setTimeout(120_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-release-upgrade-'));
  const dataDir = join(root, 'state');
  let app: HarnessApp | undefined;
  try {
    app = await launchHarness({
      binary: baseline,
      mockProvider: fileURLToPath(new URL('../../bin/ao-mockprovider', import.meta.url)),
      dataDir,
    });
    const previousVersion = app.bootstrap.version;
    expect(previousVersion).toBe('0.0.14');
    const threads = [];
    for (const provider of ['claude', 'codex'] as const) {
      const title = `Upgrade ${provider} conversation`;
      const id = await seedAgentThread(app, `upgrade-${provider}`, title, provider);
      const draft = `Unsent ${provider} draft before upgrade`;
      await app.rpc('SaveDraft', id, draft, [], [], null);
      await app.rpc('PinThread', id);
      // v0.0.14 predates ListItems' includePayloads parameter.
      const items = await app.rpc<Array<{ id: string; kind: string; summary: string }>>('ListItems', id);
      expect(items.length).toBeGreaterThan(0);
      threads.push({ id, title, draft, items: items.map(({ id, kind, summary }) => ({ id, kind, summary })) });
    }
    await app.close();
    app = undefined;

    for (const boot of ['upgrade', 'restart']) {
      app = await launchHarness({ dataDir });
      expect(app.bootstrap.version).not.toBe(previousVersion);
      test.info().annotations.push({ type: boot, description: `${previousVersion} -> ${app.bootstrap.version}` });
      for (const thread of threads) {
        expect(await app.rpc('GetDraft', thread.id)).toMatchObject({ content: thread.draft });
        expect(await app.rpc('GetThread', thread.id)).toMatchObject({ title: thread.title, pinnedAt: expect.any(Number) });
        expect((await listItems(app, thread.id)).map(({ id, kind, summary }) => ({ id, kind, summary }))).toEqual(thread.items);
      }
      await app.open(page);
      for (const thread of threads) {
        await page.getByTestId('thread-row').filter({ hasText: thread.title }).click();
        await expect(page.getByRole('textbox', { name: 'Message Input', exact: true })).toHaveValue(thread.draft);
        await expect(page.getByText('Ready.', { exact: true })).toBeVisible();
      }
      await page.goto('about:blank');
      await app.close();
      app = undefined;
    }
  } finally {
    try {
      await page.goto('about:blank');
    } finally {
      try { await app?.close(); }
      finally { await rm(root, { recursive: true, force: true }); }
    }
  }
});
