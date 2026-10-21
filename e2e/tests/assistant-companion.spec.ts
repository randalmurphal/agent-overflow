// The assistant's agent-thread companion pane and its session menu
// (docs/specs/standing-agents.md, "Agents segment" and "Interrupt"), end to
// end on both mock providers.
//
// Coverage: a click on an Agents tray row opens the worker beside the
// assistant in a companion pane whose header names the worker, with its own
// composer and Stop, which interrupts the worker and not the assistant;
// closing the companion leaves the worker's thread; Open as Pane turns the
// companion into an ordinary pane on the worker; the assistant's composer
// keeps a session menu beside Send whose Interrupt is disabled while idle
// and whose End Session ends the assistant's provider session, then is
// disabled with no session left.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { captureAgentMessage } from './wake-rows-helpers.js';
import { agentRow, mockInterrupt, openAgents, seedLeadWithWorker } from './agents-tray-helpers.js';
import { awaitTurnCompleted, threadRows, type ProviderName } from './thread-tools-helpers.js';
import type { MockInfo } from './workflows-helpers.js';

const LEAD = 'Release lead';
const WORKER = 'Changelog worker';

function companion(page: Page) {
  return page.locator('section[data-pane-kind="agent-thread"]');
}

function mainPane(page: Page) {
  return page.locator('section[data-pane-kind="thread"]');
}

/** The ordinary pane whose header shows `title`. */
function paneTitled(page: Page, title: string) {
  return mainPane(page).filter({ has: page.getByTestId('chat-header-title').getByText(title, { exact: true }) });
}

/** Whether a live mock runs in cwd. */
async function liveIn(harness: HarnessApp, cwd: string): Promise<boolean> {
  const mocks = await harness.rpc<(MockInfo & { exited: boolean })[]>('HarnessListMocks');
  return mocks.some((mock) => mock.registration.cwd === cwd && !mock.exited);
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a tray row opens the worker in a companion with its own Stop, Open as Pane promotes it, and the session menu ends the assistant's session`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const { home, workerPath, token, worker } = await seedLeadWithWorker(harness, { tag: `assistant-companion-${provider}`, provider, lead: LEAD, worker: WORKER });

    // ---- A tray click opens the worker beside the assistant.
    await harness.open(page);
    await openAgents(page, LEAD);
    await agentRow(page, token).getByTestId('agent-tray-row-open').click();
    await expect(companion(page)).toHaveCount(1);
    await expect(companion(page).getByTestId('agent-thread-preparing')).toHaveCount(0);
    await expect(companion(page).locator(`[data-ui-surface="chat"][data-thread-id="${worker}"]`)).toBeVisible();
    await expect(companion(page).getByTestId('chat-header-title')).toHaveText(WORKER);
    await expect(mainPane(page).getByTestId('chat-header-title')).toHaveText(LEAD);
    await expect(companion(page).getByTestId('agent-thread-open-as-pane')).toBeVisible();
    await captureAgentMessage(page, `${provider}-assistant-companion.png`, companion(page));

    // ---- Its own Stop interrupts the worker, not the assistant.
    const interrupt = companion(page).getByTestId('composer-interrupt');
    await expect(interrupt).toBeVisible();
    await expect(mainPane(page).getByTestId('composer-interrupt')).toHaveCount(0);
    const stopped = mockInterrupt(harness, workerPath);
    await interrupt.click();
    await stopped;
    await expect(companion(page).getByTestId('composer-send')).toBeVisible();

    // ---- Its own composer sends to the worker.
    const workerTurn = awaitTurnCompleted(harness, worker);
    await companion(page).getByLabel('Message Input').fill('Add the fixes too.');
    await companion(page).getByTestId('composer-send').click();
    await workerTurn;
    expect((await threadRows(harness)).map((row) => row.id)).toContain(worker);

    // ---- Open as Pane: the worker becomes an ordinary pane; the thread stays.
    await companion(page).getByTestId('agent-thread-open-as-pane').click();
    await expect(companion(page)).toHaveCount(0);
    await expect(mainPane(page)).toHaveCount(2);
    await expect(paneTitled(page, WORKER)).toHaveCount(1);
    await captureAgentMessage(page, `${provider}-assistant-companion-open-as-pane.png`, paneTitled(page, WORKER));

    // ---- The session menu on the assistant: Interrupt off while idle, End Session ends it.
    const leadPane = paneTitled(page, LEAD);
    expect(await liveIn(harness, home)).toBe(true);
    await leadPane.getByTestId('composer-session-menu').click();
    const menu = page.getByRole('menu', { name: 'Session' });
    await expect(menu.getByRole('menuitem', { name: 'Interrupt' })).toBeDisabled();
    await expect(menu.getByRole('menuitem', { name: 'End Session' })).not.toHaveAttribute('aria-disabled', 'true');
    // The pointer rests on the menu, and any usage gauge tooltip it raised
    // on the way has closed, so nothing covers the menu in the shot.
    await menu.hover();
    await expect(page.getByRole('tooltip')).toHaveCount(0);
    await captureAgentMessage(page, `${provider}-assistant-session-menu.png`, menu);
    await menu.getByRole('menuitem', { name: 'End Session' }).click();
    await expect.poll(() => liveIn(harness, home), { timeout: 20_000 }).toBe(false);
    expect(await liveIn(harness, workerPath)).toBe(true);
    // With no session left, End Session has nothing to end.
    await leadPane.getByTestId('composer-session-menu').click();
    const ended = page.getByRole('menu', { name: 'Session' }).getByRole('menuitem', { name: 'End Session' });
    await expect(ended).toHaveAttribute('aria-disabled', 'true');
    await expect(ended).toHaveAttribute('title', 'No session is running');
    await page.getByRole('menu', { name: 'Session' }).hover();
    await expect(page.getByRole('tooltip')).toHaveCount(0);
    await captureAgentMessage(page, `${provider}-assistant-session-menu-ended.png`, page.getByRole('menu', { name: 'Session' }));
    await page.keyboard.press('Escape');
  });
}
