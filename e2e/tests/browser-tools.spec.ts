import { test, expect, type SeedResult } from './fixtures.js';
import { sessionConfigs } from './workflows-helpers.js';

const browserServer = 'ao-browser-tools';

interface BrowserState {
  kind: string;
  threadId: string;
  activePageId?: string;
  visible?: boolean;
  pages?: Array<{ id: string; url: string; title: string }>;
}

test('built-in browser MCP reaches both provider launch boundaries and composer toggle', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'browser-mcp-wiring',
        repo: {},
        threads: [
          {
            title: 'Claude browser',
            provider: 'claude',
            turns: [{ userText: 'Ready?', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
          {
            title: 'Codex browser',
            provider: 'codex',
            turns: [{ userText: 'Ready?', items: [{ kind: 'assistant_text', summary: 'Ready.' }] }],
          },
        ],
      },
    ],
  });
  const [claudeThread, codexThread] = seed.projects[0].threadIds;

  await harness.rpc('StartSession', claudeThread);
  await harness.rpc('StartSession', codexThread);

  const [claude] = await sessionConfigs(harness, 'claude', 1);
  const [codex] = await sessionConfigs(harness, 'codex', 1);
  expect(claude.mcpServers).toContain(browserServer);
  expect(codex.mcpServers).toContain(browserServer);

  const rows = await harness.rpc<Array<{ name: string; disabled: boolean }>>(
    'ListThreadMcpServers',
    claudeThread,
  );
  expect(rows).toContainEqual(expect.objectContaining({ name: browserServer, disabled: false }));

  await page.goto(harness.url);
  await page.getByText('Claude browser').click();
  const trigger = page.getByTestId('composer-mcp-trigger');
  await trigger.click();
  let browserRow = page.getByRole('menu', { name: 'MCP servers' })
    .getByRole('menuitem')
    .filter({ hasText: browserServer });
  await expect(browserRow).toBeVisible();
  await browserRow.click();
  await expect.poll(async () => {
    const current = await harness.rpc<Array<{ name: string; disabled: boolean }>>(
      'ListThreadMcpServers',
      claudeThread,
    );
    return current.find((row) => row.name === browserServer)?.disabled;
  }).toBe(true);

  browserRow = page.getByRole('menu', { name: 'MCP servers' })
    .getByRole('menuitem')
    .filter({ hasText: browserServer });
  await expect(browserRow).toContainText('Disabled');
  await browserRow.click();
  await expect.poll(async () => {
    const current = await harness.rpc<Array<{ name: string; disabled: boolean }>>(
      'ListThreadMcpServers',
      claudeThread,
    );
    return current.find((row) => row.name === browserServer)?.disabled;
  }).toBe(false);
});

test('agent browser page stays headless until explicitly presented as an interactive companion', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'browser-companion',
      repo: {},
      threads: [{
        title: 'Browser companion',
        turns: [{ userText: 'Open the companion', items: [{ kind: 'assistant_text', summary: 'Opening it.' }] }],
      }],
    }],
  });
  const threadId = seed.projects[0].threadIds[0];
  await harness.rpc('StartSession', threadId);
  await page.goto(harness.url);
  await page.getByText('Browser companion').click();
  const htmlPath = await harness.rpc<string>(
    'WriteThreadWorkspaceFile',
    threadId,
    'browser-companion.html',
    '<!doctype html><title>Companion fixture</title><h1>Inside Agent Overflow</h1>',
  );
  const opened = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
  const pageId = opened.activePageId!;

  const pane = page.getByTestId('browser-pane');
  await expect(pane).toHaveCount(0);
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId });
  await expect(pane).toBeVisible();
  const address = pane.getByRole('textbox', { name: 'Address' });
  await address.fill(htmlPath);
  await address.press('Enter');

  // The harness boots the fake engine (spec §10): pages navigate and carry a
  // URL, but nothing renders them, so the title is derived from the address
  // rather than read from a document.
  await expect.poll(async () => {
    const state = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, {
      kind: 'activate',
      pageId,
    });
    return state.pages?.find((candidate) => candidate.id === pageId)?.url;
  }).toContain('browser-companion.html');
  // The pane reserves a host rect a real engine would present its view over.
  // Pixels are never in the DOM any more, on any engine.
  await expect(pane.getByTestId('browser-pane-host-rect')).toBeVisible();
  await expect(pane.locator('img')).toHaveCount(0);

  await pane.getByRole('button', { name: 'Close browser' }).click();
  await expect(pane).toHaveCount(0);
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'reload', pageId });
  await expect(page.getByTestId('browser-pane')).toHaveCount(0);
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId });
  await expect(page.getByTestId('browser-pane')).toBeVisible();

  const second = await harness.rpc<BrowserState>('BrowserCompanionDo', threadId, { kind: 'new' });
  await harness.rpc('BrowserCompanionDo', threadId, { kind: 'show', pageId: second.activePageId });
  await expect(pane.getByRole('button', { name: 'Close tab' })).toHaveCount(2);
  await pane.getByRole('textbox', { name: 'Address' }).focus();
  await pane.getByRole('textbox', { name: 'Address' }).press('ControlOrMeta+w');
  await expect(pane.getByRole('button', { name: 'Close tab' })).toHaveCount(1);
  await pane.getByRole('textbox', { name: 'Address' }).focus();
  await pane.getByRole('textbox', { name: 'Address' }).press('ControlOrMeta+w');
  await expect(pane).toHaveCount(0);
});

test('browser settings toggle and clear through the real SPA and backend', async ({
  harness,
  page,
}) => {
  await page.goto(harness.url);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Browser' }).click();

  const enabled = page.getByRole('switch', { name: 'Toggle Built-in Browser Tools' });
  await expect(enabled).toHaveAttribute('aria-checked', 'true');
  await expect(page.getByRole('switch', { name: 'Toggle Browser Window' })).toHaveCount(0);

  try {
    await enabled.click();
    await expect(enabled).toHaveAttribute('aria-checked', 'false');
    await expect
      .poll(async () => {
        const settings = await harness.rpc<{ browserEnabled: boolean }>('GetSettings');
        return settings.browserEnabled;
      })
      .toBe(false);
  } finally {
    const settings = await harness.rpc<{ browserEnabled: boolean }>('GetSettings');
    if (!settings.browserEnabled) await harness.rpc('UpdateSettings', { browserEnabled: true });
  }

  await page.getByRole('button', { name: 'Clear site data' }).click();
  await expect(page.getByRole('button', { name: 'Clear now' })).toBeVisible();
  await page.getByRole('button', { name: 'Clear now' }).click();
  await expect(page.getByText('Browser site data cleared')).toBeVisible();
});
