// System prompt overrides stored as files under <configDir>/prompts, and the
// per-thread "system prompt changed" banner.
//
// Coverage: adding an override in Settings creates its empty file and shows
// the blank-file warning; text written to the file by hand is what the next
// settings read reports (not blank, no text on the wire), what the open
// Settings page shows once the prompts watcher reloads it, and what the next
// Claude spawn receives with the prompt snapshot off; a file over the size
// limit is reported on the entry; removing the entry in Settings deletes its
// file. A live thread whose next prompt differs shows the banner after a
// file edit; dismiss hides it, also across a page reload; a second edit
// shows it again; Restart spawns with the new text and the banner is gone; a
// stopped session withdraws the banner and an edit while stopped raises none.
import { access, readFile, writeFile } from 'node:fs/promises';
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import { openThread } from './worktree-removal-helpers.js';
import { mockSessions, type MockInfo, type MockSessionConfig } from './workflows-helpers.js';
import type { HarnessApp } from '../src/harness.js';

// The harness seeds Claude threads on this model (internal/harnessrpc/seed.go).
const SEEDED_CLAUDE_MODEL = 'claude-opus-4-7';
// The sidebar lists a thread once it has a turn.
const seededTurn = { userText: 'hello', items: [{ kind: 'assistant_text', summary: 'Ready.' }] };
const PROMPT_FILE_PATTERN = /\/prompts\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.md$/;

interface PromptOverrideRow {
  id: string;
  enabled: boolean;
  models: string[];
  prompt?: string;
  blank?: boolean;
  path?: string;
  problem?: string;
}

interface ProviderStatusEvent {
  kind?: string;
  threadId?: string;
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

async function openClaudeSettings(harness: HarnessApp, page: Page): Promise<void> {
  await harness.open(page);
  await page.getByTestId('sidebar-settings-button').click();
  await page.getByRole('tab', { name: 'Claude Code', exact: true }).click();
}

// The launch config of the first Claude spawn not in `known`, polled because
// the config is latched on the mock's registration rather than evented.
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

test('an override is a file: added blank in Settings, written by hand, spawned from, limited and removed', async ({
  harness,
  page,
}) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: 'prompt-files', repo: {}, threads: [{ title: 'Claude with an override', provider: 'claude', turns: [seededTurn] }] },
    ],
  });
  const [threadId] = seed.projects[0].threadIds;
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));

  let promptPath = '';
  try {
    await openClaudeSettings(harness, page);
    await page.getByTestId('settings-prompt-add-claude').click();
    const pathCell = page.getByTestId('settings-prompt-path-claude-0');
    await expect(pathCell).toHaveText(PROMPT_FILE_PATTERN);

    const [added] = await claudeOverrides(harness);
    expect(added.id).not.toBe('');
    expect(added.blank).toBe(true);
    expect(added).not.toHaveProperty('prompt');
    expect(added.path?.endsWith(`/${added.id}.md`)).toBe(true);
    promptPath = added.path ?? '';
    await expect(pathCell).toHaveText(promptPath);
    expect(await readFile(promptPath, 'utf8')).toBe('');

    // Turned on for the seeded model through the page, it warns that the
    // file is empty. Those writes carry no text and leave the file alone.
    await page.getByRole('switch', { name: 'Enable claude override 1' }).click();
    await page.getByTestId(`settings-prompt-model-claude-0-${SEEDED_CLAUDE_MODEL}`).click();
    await expect
      .poll(async () => {
        const [row] = await claudeOverrides(harness);
        return { enabled: row.enabled, models: row.models };
      })
      .toEqual({ enabled: true, models: [SEEDED_CLAUDE_MODEL] });
    await expect(page.getByTestId('settings-prompt-blank-claude-0')).toBeVisible();
    expect(await readFile(promptPath, 'utf8')).toBe('');

    // Written by hand: the open page drops the warning when the prompts
    // watcher's reload reaches it, and the next read is not blank.
    await writeFile(promptPath, 'Written in the prompt file.\n');
    await expect(page.getByTestId('settings-prompt-blank-claude-0')).toHaveCount(0);
    await expect(page.getByTestId('settings-prompt-problem-claude-0')).toHaveCount(0);
    const [written] = await claudeOverrides(harness);
    expect(written.blank ?? false).toBe(false);
    expect(written).not.toHaveProperty('prompt');
    expect(written.problem ?? '').toBe('');

    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', threadId);
    const claudeConfig = await nextClaudeSpawn(harness, []);
    expect(claudeConfig.systemPrompt).toBe('Written in the prompt file.');
    expect(claudeConfig.systemPromptSnapshot).toBe('off');

    // Over the one-prompt limit: reported on the entry, not applied, and the
    // entry stays in the list.
    await writeFile(promptPath, 'x'.repeat(70_000));
    const problem = `${promptPath} is 70000 bytes, over the 64000-byte limit for one prompt; the entry is not applied until the file is shortened`;
    await expect(page.getByTestId('settings-prompt-problem-claude-0')).toHaveText(problem);
    await expect(page.getByTestId('settings-prompt-blank-claude-0')).toHaveCount(0);
    const [oversized] = await claudeOverrides(harness);
    expect(oversized.blank).toBe(true);
    expect(oversized.problem).toBe(problem);

    await page.getByTestId('settings-prompt-remove-claude-0').click();
    await expect(page.getByTestId('settings-prompt-entry-claude-0')).toHaveCount(0);
    expect(await claudeOverrides(harness)).toEqual([]);
    expect(await exists(promptPath)).toBe(false);
  } finally {
    // Soft, in the finally: a browser error is reported beside whatever
    // assertion it caused to time out, not hidden behind it.
    expect.soft(pageErrors).toEqual([]);
    await harness.rpc('UpdateSettings', { claudePromptOverrides: [] });
  }
});

test('a live thread whose next prompt differs shows a dismissible banner that Restart or a stop withdraws', async ({
  harness,
  page,
}) => {
  const title = 'Claude told its prompt changed';
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: 'prompt-banner',
        repo: {},
        threads: [
          { title, provider: 'claude', turns: [seededTurn] },
          { title: 'Claude started later', provider: 'claude', turns: [seededTurn] },
        ],
      },
    ],
  });
  const [threadId, otherThreadId] = seed.projects[0].threadIds;
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));

  try {
    await harness.rpc('UpdateSettings', {
      claudePromptOverrides: [{ id: '', enabled: true, models: [SEEDED_CLAUDE_MODEL] }],
    });
    const [entry] = await claudeOverrides(harness);
    const promptPath = entry.path ?? '';
    expect(promptPath).toMatch(PROMPT_FILE_PATTERN);
    await writeFile(promptPath, 'First prompt.\n');
    await expect.poll(async () => (await claudeOverrides(harness))[0].blank ?? false).toBe(false);

    await harness.rpc('GetProviderStatuses');
    await harness.rpc('StartSession', threadId);
    const first = await nextClaudeSpawn(harness, []);
    expect(first.systemPrompt).toBe('First prompt.');

    await harness.open(page);
    await openThread(page, title);
    const banner = page.locator('[data-testid="provider-status-banner"][data-kind="prompt_stale"]');
    await expect(banner).toHaveCount(0);

    // An edit flags the live thread; the running session keeps its prompt.
    await writeFile(promptPath, 'Second prompt.\n');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('System prompt changed');
    expect(await mockSessions(harness, 'claude', 1)).toHaveLength(1);

    // Dismissed, it stays hidden for this change, including after a reload
    // hydrates the pane from GetThreadLiveState.
    await banner.getByTestId('provider-status-dismiss').click();
    await expect(banner).toHaveCount(0);
    await harness.open(page);
    await openThread(page, title);
    await expect(banner).toHaveCount(0);

    // A new change after the dismiss shows it again.
    await writeFile(promptPath, 'Third prompt.\n');
    await expect(banner).toBeVisible();

    // Restart spawns with the new text and the banner is gone.
    const beforeRestart = await mockSessions(harness, 'claude', 1);
    await banner.getByTestId('provider-status-restart').click();
    const restarted = await nextClaudeSpawn(harness, beforeRestart);
    expect(restarted.systemPrompt).toBe('Third prompt.');
    await expect(banner).toHaveCount(0);

    // A stopped session withdraws a shown banner.
    await writeFile(promptPath, 'Fourth prompt.\n');
    await expect(banner).toBeVisible();
    await harness.rpc('StopSession', threadId);
    await expect(banner).toHaveCount(0);

    // An edit while stopped raises none. The other thread, live on the
    // fourth prompt, is the barrier: its flag comes from the same reconcile
    // pass, and GetThreadLiveState waits for that pass to finish.
    const beforeOther = await mockSessions(harness, 'claude', 0);
    await harness.rpc('StartSession', otherThreadId);
    const other = await nextClaudeSpawn(harness, beforeOther);
    expect(other.systemPrompt).toBe('Fourth prompt.');
    const isPromptStale = (id: string) => (event: ProviderStatusEvent) =>
      event.kind === 'prompt_stale' && event.threadId === id;
    const flagsBefore = harness.countEvents('provider:status', isPromptStale(threadId));
    await writeFile(promptPath, 'Fifth prompt.\n');
    await harness.waitForEvent('provider:status', isPromptStale(otherThreadId));
    // promptStale is omitted when false.
    const live = await harness.rpc<{ promptStale?: boolean }>('GetThreadLiveState', threadId);
    expect(live.promptStale ?? false).toBe(false);
    expect(harness.countEvents('provider:status', isPromptStale(threadId))).toBe(flagsBefore);
    await harness.open(page);
    await openThread(page, title);
    await expect(banner).toHaveCount(0);
  } finally {
    expect.soft(pageErrors).toEqual([]);
    await harness.rpc('UpdateSettings', { claudePromptOverrides: [] });
  }
});
