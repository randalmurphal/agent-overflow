// New Thread, Same Assistant (docs/specs/standing-agents.md, "Next thread"),
// end to end on both mock providers.
//
// Coverage: the header button moves the assistant to a new thread in the
// same pane; the successor's opening block names the previous thread and
// what moved, and reaches the provider; it shows as an agent row from
// Agent Overflow; the agent row is the successor and the previous thread
// sits archived in its done footer; the loop the old thread registered is
// the successor's; the sidebar row's button does the same from the list.
import type { Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { captureAgentMessage, trayRow } from './wake-rows-helpers.js';
import {
  awaitToolAnswer,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';

/** The next input a mock in cwd takes whose text includes `contains`. */
async function inputIn(harness: HarnessApp, cwd: string, contains: string): Promise<string> {
  const event = await harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(contains),
    60_000,
  );
  return event.report.input ?? '';
}

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
    const { path: cwd, threadIds } = seed.projects[0];
    const [first] = threadIds;

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
    const handedOff = inputIn(harness, cwd, `The user replaced your previous thread ${first} with this one`);
    await page.getByTestId('chat-header-next-thread').filter({ visible: true }).click();

    // The successor opens in the same pane with the handoff block.
    const second = await currentThread(harness, 'Ops', first);
    const input = await handedOff;
    expect(input).toContain('You are "Ops", a standing agent.');
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
    expect(await harness.rpc('GetStandingAgent', first)).toBeNull();
    expect((await harness.rpc<{ threadId: string }>('GetStandingAgent', second)).threadId).toBe(second);
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
    const third = inputIn(harness, cwd, `The user replaced your previous thread ${second} with this one`);
    await row(page, second).hover();
    await row(page, second).getByTestId('thread-row-next-thread').click();
    await third;
    const next = await currentThread(harness, 'Ops', second);
    expect(next).not.toBe(first);
    await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${next}"]`)).toBeVisible();
    await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('2 done');
  });
}
