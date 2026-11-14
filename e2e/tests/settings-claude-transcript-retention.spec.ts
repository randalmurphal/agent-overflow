// The retention window (Settings > Storage > Retention) applied to Claude
// Code's own transcript cleanup, delivered as `cleanupPeriodDays` in the
// `--settings` block.
//
// Coverage: at the default window the page says nothing and a Claude spawn
// carries no period; a longer window shows the note naming the value and the
// Claude settings file, and the next spawn carries it; once that file sets a
// longer period the note goes and the next spawn carries none.
import { mkdir, rm, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { mockSessions, type MockInfo, type MockSessionConfig } from './workflows-helpers.js';
import type { HarnessApp } from '../src/harness.js';

const seededTurn = { userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Ready.' }] };

interface ClaudeTranscriptRetention {
  settingsPath: string;
  claudeDays: number;
  explicit: boolean;
  sessionDays: number;
  error?: string;
}

async function openStorageSettings(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Storage', exact: true }).click();
}

// Starts the thread's session, stopping a running one first, and returns the
// new spawn's launch config.
async function spawn(harness: HarnessApp, threadId: string, running: boolean): Promise<MockSessionConfig> {
  const known = new Set((await mockSessions(harness, 'claude', 0)).map((mock) => mock.mockId));
  if (running) await harness.rpc('StopSession', threadId);
  await harness.rpc('StartSession', threadId);
  let fresh: MockInfo | undefined;
  await expect
    .poll(async () => {
      fresh = (await mockSessions(harness, 'claude', 0)).find((mock) => !known.has(mock.mockId));
      return fresh !== undefined;
    })
    .toBe(true);
  return fresh?.sessionConfig as MockSessionConfig;
}

test('a longer retention window reaches Claude as cleanupPeriodDays unless the Claude settings file keeps longer', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'transcript-retention', repo: {}, threads: [{ title: 'Claude keeps transcripts', provider: 'claude', turns: [seededTurn] }] },
    ],
  });
  const [threadId] = seed.projects[0].threadIds;
  const field = page.getByTestId('settings-retention-days');
  const note = page.getByTestId('settings-claude-retention-note');
  const { settingsPath } = await harness.rpc<ClaudeTranscriptRetention>('GetClaudeTranscriptRetention', 30);

  try {
    await openStorageSettings(harness, page);
    await expect(field).toHaveValue('30');
    await expect(note).toHaveCount(0);
    await harness.rpc('GetProviderStatuses');
    expect((await spawn(harness, threadId, false)).cleanupPeriodDays).toBeUndefined();

    await field.fill('90');
    await field.blur();
    await expect(note).toContainText('"cleanupPeriodDays": 90');
    await expect(note).toContainText(settingsPath);
    await expect
      .poll(async () => (await harness.rpc<{ retention: { days: number } }>('GetSettings')).retention.days)
      .toBe(90);
    expect((await spawn(harness, threadId, true)).cleanupPeriodDays).toBe(90);

    // The user's own longer period stands: nothing to say, nothing sent.
    await mkdir(dirname(settingsPath), { recursive: true });
    await writeFile(settingsPath, JSON.stringify({ cleanupPeriodDays: 365 }));
    await openStorageSettings(harness, page);
    await expect(field).toHaveValue('90');
    await expect
      .poll(async () => (await harness.rpc<ClaudeTranscriptRetention>('GetClaudeTranscriptRetention', 90)).sessionDays)
      .toBe(0);
    await expect(note).toHaveCount(0);
    expect((await spawn(harness, threadId, true)).cleanupPeriodDays).toBeUndefined();
  } finally {
    await harness.rpc('StopSession', threadId);
    await rm(settingsPath, { force: true });
    await harness.rpc('UpdateSettings', { retention: { days: 30 } });
  }
});
