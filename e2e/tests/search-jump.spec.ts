// Search hits land on their rows.
//
// Coverage: search opens with its input focused, so the query is typed
// directly. A global search hit far above another thread's loaded tail
// switches to that thread and centers the row, with "Load newer messages"
// below it, and the row stays put while the window settles. A hit on a
// background command's bell, which the notification filter hides behind
// the completed command, lands on that command inside its collapsed run.
// A hit inside a subagent opens the agent pane at the row; a hit in another
// agent moves the pane to it; and an agent the pane leaves keeps its
// reading position for the reader's return. No hit reports the row as
// gone. Unit coverage of the jump session: timelineRestoreJump.svelte.test.ts.
import type { Locator, Page } from '@playwright/test';
import type { HarnessApp } from '../src/harness.js';
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

interface SeedTurn {
  userText: string;
  items: Array<{ kind: string; summary: string; toolName?: string; role?: string; meta?: Record<string, unknown> }>;
}

function fillerTurn(i: number): SeedTurn {
  return { userText: `Question ${i}`, items: [{ kind: 'assistant_text', summary: `Answer ${i}\n\n${FILLER}` }] };
}

async function searchAndOpen(page: Page, query: string): Promise<void> {
  await page.keyboard.press('ControlOrMeta+Shift+f');
  // The dialog opens with the input focused, so the query is typed directly.
  await expect(page.getByTestId('message-search-input')).toBeFocused();
  await page.keyboard.type(query);
  const hits = page.getByTestId('message-search-results').getByRole('button');
  await expect(hits).toHaveCount(1);
  await hits.click();
  await expect(page.getByTestId('message-search')).toHaveCount(0);
}

/** Resolves once the scroller's position and content height hold for ten frames. */
function settled(scroller: Locator): Promise<void> {
  return scroller.evaluate((el) => new Promise<void>((resolve) => {
    let last = '';
    let stable = 0;
    const step = () => {
      const now = `${el.scrollTop}:${el.scrollHeight}`;
      stable = now === last ? stable + 1 : 0;
      last = now;
      if (stable >= 10) resolve();
      else requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }));
}

/** The row's center relative to the scroller's, as a fraction of the scroller's height. */
async function centerOffset(scroller: Locator, row: Locator): Promise<number> {
  const box = (await scroller.boundingBox())!;
  const rowBox = (await row.boundingBox())!;
  return (rowBox.y + rowBox.height / 2 - (box.y + box.height / 2)) / box.height;
}

/** The first row showing in the scroller and where its top sits relative to the scroller's top. */
function readingPosition(scroller: Locator): Promise<{ id: string; top: number } | null> {
  return scroller.evaluate((el) => {
    const top = el.getBoundingClientRect().top;
    for (const row of el.querySelectorAll<HTMLElement>('[data-item-id]')) {
      const rect = row.getBoundingClientRect();
      if (rect.bottom > top) return { id: row.dataset.itemId!, top: Math.round(rect.top - top) };
    }
    return null;
  });
}

async function expectNoGoneNotice(page: Page): Promise<void> {
  await expect(page.getByTestId('toast').filter({ hasText: /no longer in this thread|Could not show that message/ })).toHaveCount(0);
}

async function seedThreads(harness: HarnessApp, target: { title: string; turns: SeedTurn[] }): Promise<void> {
  await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'search-jump', repo: {}, threads: [
      { title: target.title, provider: 'claude', turns: target.turns },
      { title: 'Elsewhere', provider: 'claude', turns: [{ userText: 'Other', items: [{ kind: 'assistant_text', summary: 'Elsewhere answer.' }] }] },
    ] }],
  });
}

test('a hit far above another thread\'s tail switches to it and centers the row above Load newer', async ({ harness, page }) => {
  const turns = Array.from({ length: TURNS }, (_, i) => fillerTurn(i));
  turns[20] = { userText: 'Question 20', items: [{ kind: 'assistant_text', summary: `needle-cold answer\n\n${FILLER}` }] };
  await seedThreads(harness, { title: 'Long history', turns });
  await harness.open(page);
  await page.getByText('Elsewhere', { exact: true }).click();
  await expect(page.getByText('Elsewhere answer.', { exact: true })).toBeVisible();

  await searchAndOpen(page, 'needle-cold');

  const pane = page.locator('section[data-pane-kind="thread"]');
  const scroller = pane.getByTestId('message-timeline-scroll');
  const row = pane.getByText('needle-cold answer', { exact: false });
  await expect(row).toBeInViewport();
  await expect(pane.getByTestId('load-newer-messages')).toHaveCount(1);
  // The landing holds through the window's settle: estimates correcting
  // above the row and the footer below it do not carry it away.
  await settled(scroller);
  await expect(row).toBeInViewport();
  expect(Math.abs(await centerOffset(scroller, scroller.locator('[data-item-id]').filter({ hasText: 'needle-cold answer' })))).toBeLessThan(0.25);
  await expectNoGoneNotice(page);
});

test('a hit on a hidden background-command bell lands on the command that covers it', async ({ harness, page }) => {
  await harness.rpc('UpdateSettings', { activityRunDefault: 'collapsed' });
  const turns = Array.from({ length: TURNS }, (_, i) => fillerTurn(i));
  turns[20] = { userText: 'Run it in the background', items: [
    { kind: 'tool_call', toolName: 'Bash', summary: 'echo covered-command', meta: { task_id: 'bg-7' } },
    { kind: 'tool_call', toolName: 'Bash', summary: 'echo second-command' },
    { kind: 'notification', role: 'system', summary: 'Background command "needle-bell" completed (exit code 0)', meta: { task_id: 'bg-7' } },
    { kind: 'assistant_text', summary: `The command finished.\n\n${FILLER}` },
  ] };
  await seedThreads(harness, { title: 'Bell history', turns });
  await harness.open(page);
  await page.getByText('Elsewhere', { exact: true }).click();
  await expect(page.getByText('Elsewhere answer.', { exact: true })).toBeVisible();

  await searchAndOpen(page, 'needle-bell');

  const pane = page.locator('section[data-pane-kind="thread"]');
  await expect(pane.getByText('needle-bell')).toHaveCount(0);
  await expect(pane.getByText('echo covered-command').first()).toBeInViewport();
  await expectNoGoneNotice(page);
});

test.describe('agent transcripts', () => {
  const CHILDREN = 60;

  function agentLines(name: string, toolUseId: string, needleAt: number, needle: string): string[] {
    const children = Array.from({ length: CHILDREN }, (_, i) =>
      textLines(`${name}-child-${i}`, `${i === needleAt ? needle : `${name} note ${i}`}\n\n${FILLER}`, toolUseId)).flat();
    return [
      toolUseLine(`${name}-launch`, toolUseId, 'Agent', { description: `${name} survey`, subagent_type: name }),
      taskStartedLine(`${name}-task`, toolUseId, `${name} survey`),
      ...children,
      taskUpdatedLine(`${name}-task`, { status: 'completed', end_time: 1787415964725 }),
      toolResultLine(toolUseId, `${name} done.`),
    ];
  }

  async function runAgents(harness: HarnessApp, page: Page): Promise<void> {
    await harness.rpc('HarnessSetScenario', { scenario: claudeScenario('search-agents', [emitBurst([
      ...textLines('lead', 'Delegating two surveys.'),
      ...agentLines('alpha', 'tu-alpha', 12, 'needle-alpha'),
      ...agentLines('beta', 'tu-beta', 40, 'needle-beta'),
      ...textLines('wrap', 'Both surveys are done.'),
      RESULT_LINE,
    ])]) });
    const threadId = await seedAgentThread(harness, 'search-agents', 'Agent history');
    await harness.open(page);
    await page.getByText('Agent history', { exact: true }).click();
    await startMock(harness, threadId);
    const settled = harness.waitForEvent('provider:turn_completed');
    await harness.rpc('SendMessage', threadId, 'survey', null);
    await settled;
    await expect(page.getByText('Both surveys are done.', { exact: true })).toBeVisible();
  }

  test('a hit opens the agent pane at the row, and a hit in another agent moves it there', async ({ harness, page }) => {
    await runAgents(harness, page);
    const agentPane = page.getByTestId('companion-pane-agent-body');

    await searchAndOpen(page, 'needle-alpha');
    await expect(agentPane.getByTestId('agent-pane-breadcrumb-current')).toContainText(/alpha/i);
    await expect(agentPane.getByText('needle-alpha')).toBeInViewport();

    await searchAndOpen(page, 'needle-beta');
    await expect(agentPane.getByTestId('agent-pane-breadcrumb-current')).toContainText(/beta/i);
    await expect(agentPane.getByText('needle-beta')).toBeInViewport();
    await expect(agentPane.getByText('needle-alpha')).toHaveCount(0);
    await expectNoGoneNotice(page);
  });

  test('an agent the pane leaves keeps its reading position for the return', async ({ harness, page }) => {
    await runAgents(harness, page);
    const cards = page.getByTestId('message-timeline-scroll').first().getByTestId('subagent-group');
    const agentPane = page.getByTestId('companion-pane-agent-body');
    const scroller = agentPane.getByTestId('message-timeline-scroll');

    await cards.nth(0).getByTestId('subagent-group-open-pane').first().click();
    await expect(agentPane.getByText('alpha done.', { exact: false }).or(agentPane.getByText(`alpha note ${CHILDREN - 1}`))).toBeVisible();
    await scroller.hover();
    await page.mouse.wheel(0, -2500);
    await page.mouse.wheel(0, -1337);
    await settled(scroller);
    const left = await readingPosition(scroller);
    expect(left).not.toBeNull();

    await cards.nth(1).getByTestId('subagent-group-open-pane').first().click();
    await expect(agentPane.getByTestId('agent-pane-breadcrumb-current')).toContainText(/beta/i);
    await expect(agentPane.getByText(`beta note ${CHILDREN - 1}`)).toBeVisible();

    await cards.nth(0).getByTestId('subagent-group-open-pane').first().click();
    await expect(agentPane.getByTestId('agent-pane-breadcrumb-current')).toContainText(/alpha/i);
    await expect.poll(() => readingPosition(scroller)).toEqual(left);
  });
});
