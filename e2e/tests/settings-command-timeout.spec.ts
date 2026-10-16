// The Claude command timeout ceiling (Settings > Claude Code > Tools >
// Command timeout), delivered to Claude Code as BASH_MAX_TIMEOUT_MS.
//
// Coverage: the row shows the 5 hour default and the first Claude spawn
// receives 18000000; a change made in the row leaves the running session
// alone and reaches the next spawn (3 h = 10800000); a BASH_MAX_TIMEOUT_MS
// in the provider's custom environment is named on the row as overriding it
// and is what the next spawn receives. Screenshots of the row in light and
// dark theme, default and overridden, are written to the test output.
import type { Page, TestInfo } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { mockSessions, type MockInfo, type MockSessionConfig } from './workflows-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const seededTurn = { userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Ready.' }] };

async function openClaudeSettings(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
}

// The launch config of the first Claude spawn not in `known`.
async function nextClaudeSpawn(harness: HarnessApp, known: readonly MockInfo[]): Promise<MockSessionConfig> {
  const knownIds = new Set(known.map((mock) => mock.mockId));
  let fresh: MockInfo | undefined;
  await expect
    .poll(async () => {
      const mocks = await mockSessions(harness, 'claude', 0);
      fresh = mocks.find((mock) => !knownIds.has(mock.mockId));
      return fresh !== undefined;
    })
    .toBe(true);
  return fresh?.sessionConfig as MockSessionConfig;
}

async function restart(harness: HarnessApp, threadId: string): Promise<MockSessionConfig> {
  const before = await mockSessions(harness, 'claude', 0);
  await harness.rpc('StopSession', threadId);
  await harness.rpc('StartSession', threadId);
  return nextClaudeSpawn(harness, before);
}

async function shootRow(page: Page, testInfo: TestInfo, what: string): Promise<void> {
  const row = page.getByTestId('settings-claude-command-timeout-row');
  for (const theme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: theme });
    await row.screenshot({ path: testInfo.outputPath(`timeout-${what}-${theme}.png`) });
  }
  await page.emulateMedia({ colorScheme: 'light' });
}

test('the command timeout reaches Claude as BASH_MAX_TIMEOUT_MS, at the next spawn, unless the custom environment sets it', async ({
  harness,
  page,
}, testInfo) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'command-timeout', repo: {}, threads: [{ title: 'Claude with a long command', provider: 'claude', turns: [seededTurn] }] },
    ],
  });
  const [threadId] = seed.projects[0].threadIds;
  const field = page.getByTestId('settings-claude-command-timeout');
  const overridden = page.getByTestId('settings-claude-command-timeout-overridden');

  try {
    await openClaudeSettings(harness, page);
    await expect(field).toHaveValue('5');
    await expect(overridden).toHaveCount(0);
    await shootRow(page, testInfo, 'default');

    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', threadId);
    const first = await nextClaudeSpawn(harness, []);
    expect(first.bashMaxTimeoutMs).toBe('18000000');

    // Changed in the row while the session runs: saved, the running
    // session is not restarted, and the next spawn carries the new value.
    await field.fill('3');
    await field.press('Enter');
    await expect
      .poll(async () => (await harness.rpc<{ claudeMaxCommandTimeoutHours: number }>('GetSettings')).claudeMaxCommandTimeoutHours)
      .toBe(3);
    expect(await mockSessions(harness, 'claude', 1)).toHaveLength(1);
    const changed = await restart(harness, threadId);
    expect(changed.bashMaxTimeoutMs).toBe('10800000');

    // A custom value wins, and the row says so.
    await harness.rpc('SetProviderCustomEnvVar', 'claude', 'BASH_MAX_TIMEOUT_MS', '99999999', false);
    await openClaudeSettings(harness, page);
    await expect(overridden).toHaveText('BASH_MAX_TIMEOUT_MS in Environment overrides this.');
    await shootRow(page, testInfo, 'overridden');
    const custom = await restart(harness, threadId);
    expect(custom.bashMaxTimeoutMs).toBe('99999999');
  } finally {
    await harness.rpc('StopSession', threadId);
    await harness.rpc('DeleteProviderCustomEnvVar', 'claude', 'BASH_MAX_TIMEOUT_MS');
    await harness.rpc('UpdateSettings', { claudeMaxCommandTimeoutHours: 5 });
  }
});
