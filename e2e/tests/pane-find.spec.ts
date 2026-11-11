// Per-pane find (mod+f) and the cross-thread search dialog (mod+shift+f).
//
// Coverage: mod+f opens a find bar inside the focused thread pane, under its
// header, and not the message-search dialog. A query matching a row far
// above the loaded window reports its count, moves the timeline to that row
// with the landing flash, and paints the match through the pane's own CSS
// highlight. With a second pane focused, mod+f opens that pane's own bar
// with its own query and highlight; Escape closes it and returns focus to
// the composer it came from. An agent pane's find reads only that agent's
// transcript and the thread pane's only its top-level rows. mod+shift+f
// still opens the dialog, which has no thread mode. Unit and component coverage: paneFind.svelte.test.ts,
// threadFind.svelte.test.ts, PaneFindBar.test.ts, findHighlights.browser.test.ts.
import type { Page } from '@playwright/test';
import { test, expect, type SeedResult } from './fixtures.js';
import {
  RESULT_LINE,
  claudeScenario,
  emitBurst,
  seedAgentThread,
  startMock,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';

const TURNS = 160;
const FILLER = 'A detailed answer. '.repeat(40);

function turns(needleAt: number, needle: string) {
  return Array.from({ length: TURNS }, (_, i) => ({
    userText: `Question ${i}`,
    items: [{ kind: 'assistant_text', summary: `${i === needleAt ? `${needle} answer` : `Answer ${i}`}\n\n${FILLER}` }],
  }));
}

/** The match texts painted under a highlight name. */
function painted(page: Page, name: string): Promise<string[]> {
  return page.evaluate((n) => {
    const highlight = CSS.highlights.get(n);
    return highlight ? [...highlight].map((range) => range.toString()) : [];
  }, name);
}

test('mod+f finds inside the focused pane and steps to a row outside the loaded window', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'pane-find', repo: {}, threads: [
      { title: 'Long history', provider: 'claude', turns: turns(20, 'needle-cold') },
      { title: 'Other thread', provider: 'claude', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'needle-other lives here.' }] }] },
    ] }],
  });
  const [longId, otherId] = seed.projects[0].threadIds;
  await harness.open(page);
  await page.locator(`[data-sidebar-thread-id="${longId}"]`).click();
  await page.locator(`[data-sidebar-thread-id="${otherId}"]`).click({ modifiers: ['ControlOrMeta'] });
  const panes = page.locator('section[data-pane-kind="thread"]');
  await expect(panes).toHaveCount(2);
  const longPane = panes.filter({ has: page.locator(`[data-thread-id="${longId}"]`) });
  const otherPane = panes.filter({ has: page.locator(`[data-thread-id="${otherId}"]`) });
  await expect(longPane.getByText('Answer 159', { exact: false })).toBeVisible();

  // From the composer, mod+f opens this pane's bar, under its header.
  await longPane.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+f');
  const longBar = longPane.getByTestId('pane-find-bar');
  await expect(longBar).toBeVisible();
  await expect(longBar).toHaveAttribute('role', 'search');
  await expect(page.getByTestId('message-search')).toHaveCount(0);
  await expect(otherPane.getByTestId('pane-find-bar')).toHaveCount(0);
  const headerBox = (await longPane.getByTestId('chat-header-title').boundingBox())!;
  const barBox = (await longBar.boundingBox())!;
  const scrollerBox = (await longPane.getByTestId('message-timeline-scroll').boundingBox())!;
  expect(barBox.y).toBeGreaterThan(headerBox.y);
  expect(scrollerBox.y).toBeGreaterThanOrEqual(barBox.y + barBox.height - 1);
  await expect(longPane.getByTestId('pane-find-input')).toBeFocused();

  // The hit is far above the loaded window: the bar counts it and the pane
  // pages it in and lands on it.
  await page.keyboard.type('needle-cold');
  await expect(longBar.getByTestId('pane-find-count')).toHaveText('1/1');
  const row = longPane.getByText('needle-cold answer', { exact: false });
  await expect(row).toBeInViewport();
  await expect(longPane.getByTestId('load-newer-messages')).toHaveCount(1);
  const longName = await longPane.getAttribute('data-pane-id').then((id) => `ao-find-${id!.replace(/[^A-Za-z0-9_-]/g, '_')}`);
  await expect.poll(() => painted(page, `${longName}-current`)).toEqual(['needle-cold']);

  // Stepping wraps on one hit and lands again, with the landing flash.
  await page.keyboard.press('Enter');
  await expect(longPane.getByTestId('timeline-jump-flash')).toHaveCount(1);
  await expect(row).toBeInViewport();

  // The other pane gets its own bar, query and highlight.
  const otherComposer = otherPane.getByLabel('Message Input');
  await otherComposer.click();
  await page.keyboard.press('ControlOrMeta+f');
  const otherBar = otherPane.getByTestId('pane-find-bar');
  await expect(otherBar).toBeVisible();
  await expect(otherPane.getByTestId('pane-find-input')).toBeFocused();
  await expect(otherPane.getByTestId('pane-find-input')).toHaveValue('');
  await page.keyboard.type('needle-other');
  await expect(otherBar.getByTestId('pane-find-count')).toHaveText('1/1');
  const otherName = await otherPane.getAttribute('data-pane-id').then((id) => `ao-find-${id!.replace(/[^A-Za-z0-9_-]/g, '_')}`);
  expect(otherName).not.toBe(longName);
  await expect.poll(() => painted(page, `${otherName}-current`)).toEqual(['needle-other']);
  await expect(longPane.getByTestId('pane-find-input')).toHaveValue('needle-cold');
  await expect.poll(() => painted(page, `${longName}-current`)).toEqual(['needle-cold']);

  // Escape closes this bar only, clears its paint and returns focus.
  await page.keyboard.press('Escape');
  await expect(otherBar).toHaveCount(0);
  await expect(otherComposer).toBeFocused();
  await expect.poll(() => painted(page, `${otherName}-current`)).toEqual([]);
  await expect(longBar).toBeVisible();

  // Reopening shows the pane's last query, selected.
  await page.keyboard.press('ControlOrMeta+f');
  await expect(otherPane.getByTestId('pane-find-input')).toHaveValue('needle-other');
  await expect.poll(() => otherPane.getByTestId('pane-find-input').evaluate((el: HTMLInputElement) => [el.selectionStart, el.selectionEnd])).toEqual([0, 12]);
});

test('an agent pane finds only its own transcript, and the thread pane only its top-level rows', async ({ harness, page }) => {
  const agentLines = (name: string, toolUseId: string, needle: string): string[] => [
    toolUseLine(`${name}-launch`, toolUseId, 'Agent', { description: `${name} survey`, subagent_type: name }),
    taskStartedLine(`${name}-task`, toolUseId, `${name} survey`),
    ...textLines(`${name}-child`, `${needle} from ${name}`, toolUseId),
    taskUpdatedLine(`${name}-task`, { status: 'completed', end_time: 1787415964725 }),
    toolResultLine(toolUseId, `${name} done.`),
  ];
  await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('pane-find-agents', [emitBurst([
    ...textLines('lead', 'Delegating two surveys.'),
    ...agentLines('alpha', 'tu-alpha', 'needle-alpha'),
    ...agentLines('beta', 'tu-beta', 'needle-beta'),
    ...textLines('wrap', 'Both surveys are done.'),
    RESULT_LINE,
  ])]) });
  const threadId = await seedAgentThread(harness, 'pane-find-agents', 'Agent find');
  await harness.open(page);
  await page.getByText('Agent find', { exact: true }).click();
  await startMock(harness, threadId);
  const settled = harness.waitForEvent('provider:turn_completed');
  await harness.rpc('SendMessage', threadId, 'survey', null);
  await settled;
  const threadPane = page.locator('section[data-pane-kind="thread"]');
  await expect(threadPane.getByText('Both surveys are done.', { exact: true })).toBeVisible();

  await threadPane.getByTestId('subagent-group').first().getByTestId('subagent-group-open-pane').first().click();
  const agentSection = page.locator('section[data-pane-kind="agent"]');
  await expect(agentSection.getByTestId('agent-pane-breadcrumb-current')).toContainText(/alpha/i);
  await agentSection.getByTestId('agent-pane-header').click();
  await page.keyboard.press('ControlOrMeta+f');
  const agentBar = agentSection.getByTestId('pane-find-bar');
  await expect(agentBar).toBeVisible();
  await expect(agentSection.getByTestId('pane-find-input')).toHaveAttribute('placeholder', 'Find in transcript');
  await page.keyboard.type('needle-alpha');
  await expect(agentBar.getByTestId('pane-find-count')).toHaveText('1/1');
  await agentSection.getByTestId('pane-find-input').fill('needle-beta');
  await expect(agentBar.getByTestId('pane-find-count')).toHaveText('No results');
  await expect(threadPane.getByTestId('pane-find-bar')).toHaveCount(0);

  await threadPane.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+f');
  await page.keyboard.type('needle-alpha');
  await expect(threadPane.getByTestId('pane-find-count')).toHaveText('No results');
  await threadPane.getByTestId('pane-find-input').fill('surveys are done');
  await expect(threadPane.getByTestId('pane-find-count')).toHaveText('1/1');
});

test('mod+shift+f still opens the cross-thread search dialog, with no thread mode', async ({ harness, page }) => {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'pane-find-dialog', repo: {}, threads: [
      { title: 'Dialog thread', provider: 'claude', turns: [{ userText: 'Hello', items: [{ kind: 'assistant_text', summary: 'dialog-needle answer' }] }] },
    ] }],
  });
  await harness.open(page);
  await page.getByText('Dialog thread', { exact: true }).click();
  await page.getByLabel('Message Input').click();
  await page.keyboard.press('ControlOrMeta+Shift+f');
  const input = page.getByTestId('message-search-input');
  await expect(input).toBeFocused();
  await expect(input).toHaveAttribute('aria-label', 'Search messages');
  await expect(page.getByTestId('pane-find-bar')).toHaveCount(0);
  await page.keyboard.type('dialog-needle');
  await expect(page.getByTestId('message-search-results').getByRole('button')).toHaveCount(1);
  await expect(page.getByText('Find in thread')).toHaveCount(0);
});
