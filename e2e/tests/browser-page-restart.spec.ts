// Browser pages survive a restart (docs/architecture/browser-tools.md,
// lifecycle). A graceful shutdown saves each open page, the next boot lists
// it as a suspended tab at its address, and presenting it reloads it: a
// mounted pane whose active tab is suspended restores it with no click.
import { test, expect, type SeedResult } from './fixtures.js';
import type { BrowserContext } from '@playwright/test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import * as path from 'node:path';
import { launchHarness, type HarnessApp } from '../src/harness.js';

interface BrowserState {
  activePageId?: string;
  visible?: boolean;
  pages?: Array<{ id: string; url: string; suspended?: boolean }>;
}

function tabs(state: BrowserState): string[] {
  return (state.pages ?? []).map((p) => `${path.basename(p.url)}${p.suspended ? ':suspended' : ''}`);
}

test('browser pages come back after a restart and restore when presented', async ({ browser }) => {
  const dataDir = await mkdtemp(path.join(tmpdir(), 'ao-browser-restart-'));
  let host: HarnessApp | undefined;
  let context: BrowserContext | undefined;
  try {
    host = await launchHarness({ dataDir });
    const seed = await host.rpc<SeedResult>('HarnessSeed', {
      projects: [{
        name: 'browser-restart',
        repo: {},
        threads: [{ title: 'Browser restart', turns: [{ userText: 'Open pages', items: [{ kind: 'assistant_text', summary: 'Opened.' }] }] }],
      }],
    });
    const projectId = seed.projects[0].projectId;
    const threadId = seed.projects[0].threadIds[0];
    await host.rpc('StartSession', threadId);
    const files: string[] = [];
    for (const name of ['first.html', 'second.html']) {
      files.push(await host.rpc<string>(
        'WriteWorkspaceFile',
        { projectId, workspacePath: '' },
        name,
        `<!doctype html><title>${name}</title>`,
      ));
    }
    const ids: string[] = [];
    for (const file of files) {
      const opened = await host.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
      const pageId = opened.activePageId!;
      ids.push(pageId);
      await host.rpc('BrowserCompanionDo', threadId, { kind: 'navigate', pageId, address: file });
    }
    const live = host;
    await expect.poll(async () => tabs(await live.rpc<BrowserState>('BrowserCompanionThreadState', threadId)))
      .toEqual(['first.html', 'second.html']);

    expect(await host.stop()).toBe(true);
    host = await launchHarness({ dataDir });
    const restarted = host;
    const saved = await restarted.rpc<BrowserState>('BrowserCompanionThreadState', threadId);
    expect(tabs(saved)).toEqual(['first.html:suspended', 'second.html:suspended']);
    expect(saved.pages!.map((p) => p.id)).toEqual(ids);

    context = await browser.newContext();
    const page = await context.newPage();
    await restarted.open(page);
    await page.getByText('Browser restart').click();
    // Showing the companion opens the pane on the page; the pane presents
    // it, which reloads it.
    await restarted.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId: ids[0] });
    const pane = page.getByTestId('browser-pane');
    await expect(pane).toBeVisible();
    await expect.poll(async () => tabs(await restarted.rpc<BrowserState>('BrowserCompanionThreadState', threadId)))
      .toEqual(['first.html', 'second.html:suspended']);

    // Closing the active tab makes the suspended one active; the pane
    // presents it, which restores it at its address.
    await pane.getByRole('button', { name: 'Close tab' }).first().click();
    await expect.poll(async () => tabs(await restarted.rpc<BrowserState>('BrowserCompanionThreadState', threadId)))
      .toEqual(['second.html']);
    await expect(pane.getByRole('textbox', { name: 'Address' })).toHaveValue(/second\.html$/);
    await expect(pane.getByRole('alert')).toHaveCount(0);
  } finally {
    await context?.close();
    await host?.close();
    await rm(dataDir, { recursive: true, force: true });
  }
});
