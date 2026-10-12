// System prompt overrides stored as files under <configDir>/prompts.
//
// Coverage: a saved override's text lands in its own file and the settings
// read reports the file's path; an edit made to the file outside the app is
// what the next read returns, what the next Claude spawn receives through
// --system-prompt-file (rendered under <configDir>/cache/prompts) and what
// the Settings page shows beside the path and its open control; a file over
// the size limit is reported on the entry instead of applied; removing the
// entry removes its file.
//
// The watcher's push of an external edit to a live session is covered at the
// app level (TestAnEditedPromptFileReachesALiveClaudeSession). Every page
// load here happens after the edit, so no assertion depends on the watcher's
// timing.
import { access, readFile, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { test, expect, type SeedResult } from './fixtures.js';
import { sessionConfigs } from './workflows-helpers.js';
import type { HarnessApp } from '../src/harness.js';

// The harness seeds Claude threads on this model (internal/harnessrpc/seed.go).
const SEEDED_CLAUDE_MODEL = 'claude-opus-4-7';
const PROMPT_FILE_PATTERN = /\/prompts\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.md$/;

interface PromptOverrideRow {
  id: string;
  enabled: boolean;
  models: string[];
  prompt?: string;
  path?: string;
  problem?: string;
}

async function claudeOverrides(harness: HarnessApp): Promise<PromptOverrideRow[]> {
  const settings = await harness.rpc<{ claudePromptOverrides?: PromptOverrideRow[] }>('GetSettings');
  return settings.claudePromptOverrides ?? [];
}

async function exists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

test('a prompt override is a file: saved there, edited there, spawned from there and shown in Settings', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'prompt-files', repo: {}, threads: [{ title: 'Claude with an override', provider: 'claude' }] },
    ],
  });
  const [threadId] = seed.projects[0].threadIds;
  // A row the UI cannot render (the Settings chunk throwing mid-flush) leaves
  // the tab selected and the panel on the previous page; the error, not the
  // stale panel, names the defect.
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));

  let promptPath = '';
  try {
    await harness.rpc('UpdateSettings', {
      claudePromptOverrides: [
        { id: '', enabled: true, models: [SEEDED_CLAUDE_MODEL], prompt: 'Saved through settings.' },
      ],
    });
    const [saved] = await claudeOverrides(harness);
    expect(saved.id).not.toBe('');
    expect(saved.path).toMatch(PROMPT_FILE_PATTERN);
    expect(saved.path?.endsWith(`/${saved.id}.md`)).toBe(true);
    promptPath = saved.path ?? '';
    expect(await readFile(promptPath, 'utf8')).toBe('Saved through settings.\n');

    // An edit made to the file, not through the app.
    await writeFile(promptPath, 'Edited in the prompt file.\n');
    const [edited] = await claudeOverrides(harness);
    expect(edited.prompt).toBe('Edited in the prompt file.');
    expect(edited.problem ?? '').toBe('');

    // The next spawn renders the edited text into the thread's file under
    // <configDir>/cache/prompts and passes that file to the CLI.
    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', threadId);
    const [claudeConfig] = await sessionConfigs(harness, 'claude', 1);
    expect(claudeConfig.systemPrompt).toBe('Edited in the prompt file.');
    const configDir = dirname(dirname(promptPath));
    expect(await readFile(join(configDir, 'cache', 'prompts', `${threadId}.system.md`), 'utf8')).toBe(
      'Edited in the prompt file.',
    );

    await harness.open(page);
    await page.getByTestId('sidebar-settings-button').click();
    await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
    await expect(page.getByTestId('settings-prompt-text-claude-0')).toHaveValue('Edited in the prompt file.');
    await expect(page.getByTestId('settings-prompt-path-claude-0')).toHaveText(promptPath);
    await expect(page.getByRole('button', { name: 'Open prompt file in editor' })).toBeVisible();
    await expect(page.getByTestId('settings-prompt-problem-claude-0')).toHaveCount(0);

    // Over the one-prompt limit: reported on the entry, not applied, and the
    // entry stays in the list.
    await writeFile(promptPath, 'x'.repeat(70_000));
    const [oversized] = await claudeOverrides(harness);
    expect(oversized.prompt ?? '').toBe('');
    expect(oversized.problem).toBe(
      `${promptPath} is 70000 bytes, over the 64000-byte limit for one prompt; the entry is not applied until the file is shortened`,
    );
    await harness.open(page);
    await page.getByTestId('sidebar-settings-button').click();
    await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
    await expect(page.getByTestId('settings-prompt-problem-claude-0')).toHaveText(oversized.problem ?? '');
    await expect(page.getByTestId('settings-prompt-noprompt-claude-0')).toHaveCount(0);
    await expect(page.getByTestId('settings-prompt-text-claude-0')).toHaveValue('');
    expect(pageErrors).toEqual([]);
  } finally {
    await harness.rpc('UpdateSettings', { claudePromptOverrides: [] });
  }
  expect(await claudeOverrides(harness)).toEqual([]);
  expect(await exists(promptPath)).toBe(false);
});
