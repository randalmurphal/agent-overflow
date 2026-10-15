// New Thread, Same Assistant under the compact layout
// (docs/specs/standing-agents.md, "Next thread"): the sidebar row's button
// shows at rest and fits the phone, a tap moves the assistant to a new
// thread whose opening block names the previous one, and the thread
// header's button does the same from the thread screen.
import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { capture } from './wake-rows-helpers.js';
import { plainScenario, setScenario, threadRows } from './thread-tools-helpers.js';

async function handedOff(harness: HarnessApp, cwd: string, previous: string) {
  return harness.waitForEvent<{ cwd: string; report: { kind: string; input?: string } }>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(`The user replaced your previous thread ${previous} with this one`),
    60_000,
  );
}

async function currentThread(harness: HarnessApp, not: string): Promise<string> {
  let id = '';
  await expect
    .poll(async () => {
      id = (await threadRows(harness)).find((row) => row.title === 'Ops' && row.id !== not && !row.archived)?.id ?? '';
      return id;
    })
    .not.toBe('');
  return id;
}

test('compact: the row and header buttons move the assistant to a new thread', async ({ harness, page }) => {
  test.setTimeout(120_000);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: 'compact-assistant-handoff', repo: {}, threads: [{ title: 'Ops', assistant: true, turns: [{ userText: 'hi', items: [{ kind: 'assistant_text', summary: 'ready' }] }] }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const [first] = threadIds;
  await setScenario(harness, cwd, plainScenario({ name: 'compact-assistant-handoff', provider: 'claude', texts: ['Picked up.'], afterTurns: 'repeatLast' }));

  await harness.open(page);
  const width = page.viewportSize()!.width;
  const firstRow = page.locator(`[data-sidebar-thread-id="${first}"]`);
  const button = firstRow.getByTestId('thread-row-next-thread');
  await expect(button).toBeVisible();
  const box = (await button.boundingBox())!;
  expect(box.x + box.width).toBeLessThanOrEqual(width);
  expect(box.width).toBeGreaterThanOrEqual(32);
  await capture(page, 'compact-assistant-row-next-thread.png', firstRow);

  // ---- From the row: the successor is the agent row, the old thread its done child.
  const fromRow = handedOff(harness, cwd, first);
  await button.tap();
  await fromRow;
  const second = await currentThread(harness, first);
  await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${second}"]`)).toBeVisible();
  const footer = page.getByTestId('thread-row-agent-done');
  await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');

  // ---- From the thread header.
  await page.locator(`[data-sidebar-thread-id="${second}"]`).click();
  await expect(page.locator('html')).toHaveAttribute('data-compact-screen', 'thread');
  await expect(page.getByTestId('chat-header-title')).toHaveText('Ops');
  const header = page.getByTestId('chat-header-next-thread').filter({ visible: true });
  await expect(header).toBeInViewport({ ratio: 1 });
  await capture(page, 'compact-assistant-header-next-thread.png', page.getByTestId('chat-header-title-row'));
  const fromHeader = handedOff(harness, cwd, second);
  await header.tap();
  await fromHeader;
  const third = await currentThread(harness, second);
  expect(third).not.toBe(first);
  // The thread screen switches to the successor.
  const handoff = page.locator('[data-testid="agent-message-row"][data-agent-message-kind="handoff"]');
  await expect(handoff).toHaveCount(1);
  await expect(handoff.getByTestId('agent-message-sender')).toHaveText('Agent Overflow');
  await expect(handoff).toContainText(second);
  await capture(page, 'compact-assistant-handoff.png', handoff);
  expect((await harness.rpc<{ threadId: string }>('GetStandingAgent', third)).threadId).toBe(third);
});
