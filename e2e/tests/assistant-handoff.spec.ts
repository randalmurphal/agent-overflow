// New Thread, Same Assistant (docs/specs/standing-agents.md, "Next thread"),
// end to end on both mock providers.
//
// Coverage: the header button moves the assistant to a new thread in the
// same pane; the successor's opening block names the previous thread and
// what moved, and reaches the provider; it shows as an agent row from
// Agent Overflow; the agent row is the successor and the previous thread
// sits archived in its done footer; the loop the old thread registered is
// the successor's; the sidebar row's button does the same from the list; a
// second click on the row's button while the first handoff runs, with no
// pane showing the thread, makes no second thread and no error.
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { captureAgentMessage, trayRow } from './wake-rows-helpers.js';
import { methodIdByName } from './offhost-helpers.js';
import {
  AO_MESSAGE_CLOSING,
  aoMessageOpening,
  assistantHome,
  awaitMockInput,
  awaitToolAnswer,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';

/** The next input a mock in cwd takes whose text includes `contains`. */
/** The assistant's current thread: its live row named `name`. */
async function currentThread(harness: HarnessApp, name: string, not: string): Promise<string> {
  let id = '';
  await expect
    .poll(async () => {
      id = (await threadRows(harness)).find((row) => row.title === name && row.id !== not && !row.archived)?.id ?? '';
      return id;
    })
    .not.toBe('');
  return id;
}

function row(page: Page, id: string) {
  return page.locator(`[data-sidebar-thread-id="${id}"]`);
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: New Thread, Same Assistant moves the assistant, its loop and its row to a new thread`, async ({ harness, page }) => {
    test.setTimeout(180_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `assistant-handoff-${provider}`, repo: {}, threads: [{ title: 'Ops', provider, assistant: true }] }],
    });
    const [first] = seed.projects[0].threadIds;
    // Every thread of the assistant runs in its home.
    const cwd = await assistantHome(harness, first);

    // The first thread registers a loop.
    await setScenario(harness, cwd, threadToolsScenario({
      name: `assistant-handoff-loop-${provider}`,
      provider,
      echoUserMessage: true,
      turns: [{ steps: [{ call: { tool: 'thread_loop', args: { label: 'board check', note: 'look at the board', every_seconds: 3600 } } }], text: 'Scheduled.' }],
      afterTurns: 'silent',
    }));
    await harness.rpc('StartSession', first);
    await harness.rpc('SendMessage', first, 'Look at the board every hour.', null);
    const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd });
    expect(loop.isError, loop.text).toBe(false);
    await awaitTurnCompleted(harness, first);
    await setScenario(harness, cwd, plainScenario({ name: `assistant-handoff-${provider}`, provider, texts: ['Picked up the board check.'], afterTurns: 'repeatLast', echoUserMessage: true }));

    await harness.open(page);
    await row(page, first).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const handedOff = awaitMockInput(harness, { cwd, includes: `The user replaced your previous thread ${first} with this one` });
    await page.getByTestId('chat-header-next-thread').filter({ visible: true }).click();

    // The successor opens in the same pane with the handoff block.
    const second = await currentThread(harness, 'Ops', first);
    const input = (await handedOff).report.input ?? '';
    expect(input.startsWith(`${aoMessageOpening()}You are "Ops", a standing agent.`), input).toBe(true);
    expect(input.endsWith(AO_MESSAGE_CLOSING), input).toBe(true);
    expect(input).toContain('your 1 loop or watch');
    expect(input).toContain(`Read your previous thread with thread_show (thread_id ${first})`);
    await expect(page.locator('section[data-pane-id]')).toHaveCount(1);
    await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
    const handoff = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="handoff"]');
    await expect(handoff).toHaveCount(1);
    await expect(handoff.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
    await expect(page.getByText('Picked up the board check.')).toBeVisible();
    await captureAgentMessage(page, `${provider}-assistant-handoff.png`, handoff);

    // The agent row is the successor; the previous thread is archived in its done footer.
    await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${second}"]`)).toBeVisible();
    const footer = page.getByTestId('thread-row-agent-done');
    await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
    await footer.getByTestId('thread-row-agent-done-toggle').click();
    await expect(row(page, first)).toHaveAttribute('data-archived-child', 'true');
    await expect(row(page, first).getByTestId('thread-row-time')).toHaveText(/^superseded /);
    expect(await harness.rpc('GetStandingAgent', first)).toBeNull();
    expect((await harness.rpc<{ threadId: string }>('GetStandingAgent', second)).threadId).toBe(second);
    // Hover swaps a row's time for its actions; the shot shows the time.
    await page.mouse.move(800, 400);
    await captureAgentMessage(page, `${provider}-assistant-handoff-sidebar.png`, row(page, second), footer, row(page, first));

    // The loop is the successor's.
    const wakes = async (id: string) => (await harness.rpc<{ rows: { label: string }[] }>('ListThreadWakes', id)).rows.map((r) => r.label);
    expect(await wakes(first)).toEqual([]);
    expect(await wakes(second)).toEqual(['board check']);
    await row(page, second).click();
    const toggle = page.getByTestId('activity-rail-background-toggle').filter({ visible: true });
    if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
    await expect(trayRow(page, 'loop', 'board check')).toBeVisible();

    // ---- The sidebar row's button does the same from the list.
    const third = awaitMockInput(harness, { cwd, includes: `The user replaced your previous thread ${second} with this one` });
    await row(page, second).hover();
    await row(page, second).getByTestId('thread-row-next-thread').click();
    await third;
    const next = await currentThread(harness, 'Ops', second);
    expect(next).not.toBe(first);
    await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${next}"]`)).toBeVisible();
    await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('2 done');
  });

  test(`${provider}: a second click on the row's New Thread, Same Assistant while the first runs makes one thread`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{ name: `assistant-handoff-twice-${provider}`, repo: {}, threads: [{ title: 'Ops', provider, assistant: true }] }],
    });
    const [first] = seed.projects[0].threadIds;
    const cwd = await assistantHome(harness, first);
    await setScenario(harness, cwd, plainScenario({ name: `assistant-handoff-twice-${provider}`, provider, texts: ['Picked up.'], afterTurns: 'repeatLast', echoUserMessage: true }));
    const turn = awaitTurnCompleted(harness, first);
    await harness.rpc('SendMessage', first, 'hello', null);
    await turn;
    // The calls the page makes, read off its socket: the second click's
    // would be refused, and its error toast fades, so the wire is the
    // record that it never went.
    const startNext = methodIdByName('StartNextAssistantThread');
    let calls = 0;
    page.on('websocket', (ws) => {
      ws.on('framesent', (frame) => {
        const sent = JSON.parse(String(frame.payload)) as { type?: string; methodId?: number };
        if (sent.type === 'rpc' && sent.methodId === startNext) calls++;
      });
    });
    await harness.open(page);
    // Both clicks in one task, so the second lands while the first call is
    // in flight.
    await row(page, first).hover();
    await row(page, first).getByTestId('thread-row-next-thread').evaluate((button: HTMLElement) => {
      button.click();
      button.click();
    });
    const second = await currentThread(harness, 'Ops', first);
    await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${second}"]`)).toBeVisible();
    await expect(page.getByTestId('thread-row-agent-done').getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
    expect((await threadRows(harness)).filter((r) => r.title === 'Ops').map((r) => r.id)).toEqual([second]);
    expect(calls).toBe(1);
    expect(await page.getByTestId('toast').count()).toBe(0);
  });
}
