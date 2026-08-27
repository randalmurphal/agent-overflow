import { test, expect, type SeedResult } from './fixtures.js';
import { sessionConfigs } from './workflows-helpers.js';

const browserServer = 'agent-overflow-browser';

test('built-in browser MCP reaches both provider launch boundaries', async ({ harness }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'browser-mcp-wiring',
        repo: {},
        threads: [
          { title: 'Claude browser', provider: 'claude' },
          { title: 'Codex browser', provider: 'codex' },
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
