// Claude's thinking setting has no "off": every current Claude model rejects
// disabled thinking and the CLI runs adaptive thinking in its place.
//
// Coverage: the Settings picker offers only Claude's default and a fixed
// budget; a user-tier "off" an earlier version stored loads as Claude's
// default on the next boot, in the page and in the settings read, and a
// later settings write still succeeds beside the stored row. The spec writes
// the old row into the stopped backend's database, which is the state an
// upgrade from that version leaves behind.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Page } from '@playwright/test';
import { launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';

interface ClaudeThinking {
  mode?: string;
  budgetTokens?: number;
  display?: string;
}

async function openClaudeSettings(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
}

async function storedThinking(harness: HarnessApp): Promise<ClaudeThinking> {
  const settings = await harness.rpc<{ claudeThinking?: ClaudeThinking }>('GetSettings');
  return settings.claudeThinking ?? {};
}

test('the thinking picker has no off, and a stored off loads as the default', async ({ page }) => {
  test.setTimeout(120_000);
  const root = await mkdtemp(join(tmpdir(), 'ao-thinking-off-'));
  let harness: HarnessApp | undefined;
  try {
    harness = await launchHarness({ dataDir: root });
    await harness.rpc('HarnessSeed', { projects: [{ name: 'thinking-settings', repo: {} }] });
    await openClaudeSettings(harness, page);
    const mode = page.getByTestId('settings-claude-thinking-mode');
    await expect(mode).toHaveValue('');
    expect(await mode.locator('option').evaluateAll((options) => options.map((o) => (o as HTMLOptionElement).value))).toEqual([
      '',
      'budget',
    ]);
    const database = join(harness.bootstrap.dataDir, 'agent-overflow.db');
    expect(await harness.stop()).toBe(true);
    harness = undefined;

    // What an earlier version wrote for "Off" with hidden thinking text.
    const writing = new DatabaseSync(database);
    try {
      writing
        .prepare(
          `INSERT INTO ui_state (scope, key, value, updated_at) VALUES ('user:default', 'claudeThinking', ?, ?)
           ON CONFLICT (scope, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
        )
        .run(JSON.stringify({ mode: 'off', display: 'omitted' }), Date.now());
    } finally {
      writing.close();
    }

    harness = await launchHarness({ dataDir: root });
    expect(await storedThinking(harness)).toEqual({});
    await openClaudeSettings(harness, page);
    await expect(page.getByTestId('settings-claude-thinking-mode')).toHaveValue('');
    await expect(page.getByTestId('settings-claude-thinking-display')).toHaveValue('');
    await expect(page.getByTestId('settings-claude-thinking-budget')).toHaveCount(0);

    // The stored row does not block a later write, and the picker keeps working.
    await page.getByTestId('settings-claude-thinking-mode').selectOption('budget');
    await expect(page.getByTestId('settings-claude-thinking-budget')).toBeVisible();
    await expect.poll(() => storedThinking(harness!)).toEqual({ mode: 'budget', budgetTokens: 8000 });
  } finally {
    try {
      await page.close();
      await harness?.close();
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }
});
