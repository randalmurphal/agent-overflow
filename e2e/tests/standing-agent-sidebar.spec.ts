// The standing agent's sidebar row (docs/specs/standing-agents.md,
// "Sidebar"), end to end: an assistant's row is an agent row; the threads
// the agent spawned, seeded or spawned live
// through thread_spawn, nest under it behind the group rail; a spawn that
// names a group is refused for an agent; the state line names the agent's
// soonest loop; the chevron and the arrow keys fold the children, a
// collapsed agent previews the focused child; a drag onto an agent, or of
// an agent or its child onto a group, moves nothing; the menu offers the
// agent items and no pin or group item; archiving a child moves it to the
// `N done` footer, which lists it dimmed and lets it back out on
// unarchive; Archive Done Children archives the idle children behind a
// confirm; promotion and demotion are offered nowhere.
import type { Locator, Page } from '@playwright/test';

import { test, expect, type SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  awaitToolAnswer,
  awaitTurnCompleted,
  plainScenario,
  setScenario,
  threadRows,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import { capture } from './wake-rows-helpers.js';

interface ThreadGroup {
  id: string;
  name: string;
}

interface SidebarRow {
  id: string;
  groupId?: string;
  archived: boolean;
}

function row(page: Page, id: string): Locator {
  return page.locator(`[data-sidebar-thread-id="${id}"]`);
}

/** The state line under a row: the row's sibling inside its shell. */
function stateLine(page: Page, id: string): Locator {
  return page.locator(`[data-testid="thread-row-shell"]:has([data-sidebar-thread-id="${id}"]) [data-testid="thread-row-agent-state"]`);
}

/** The ids of the rows behind the agent's rail, top to bottom. */
async function memberIds(page: Page): Promise<string[]> {
  return await page.locator('[data-agent-member] [data-sidebar-thread-id]').evaluateAll((els) =>
    els.map((el) => (el as HTMLElement).dataset.sidebarThreadId ?? ''),
  );
}

async function rowById(harness: HarnessApp, id: string): Promise<SidebarRow | undefined> {
  return (await threadRows(harness)).find((r) => r.id === id);
}

async function menuLabels(page: Page, target: Locator): Promise<string[]> {
  await target.click({ button: 'right' });
  const labels = await page.getByRole('menuitem').evaluateAll((els) =>
    els.map((el) => (el.textContent ?? '').trim().replace(/[▸\s]+$/u, '')),
  );
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menuitem')).toHaveCount(0);
  return labels;
}

const other = (provider: ProviderName): ProviderName => (provider === 'claude' ? 'codex' : 'claude');

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: an agent row nests its spawned threads, folds, previews, refuses groups and files its done children`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const seed = await harness.rpc<SeedResult>('HarnessSeed', {
      projects: [{
        name: `agent-row-${provider}`, repo: {},
        threads: [
          { title: 'Release agent', provider, assistant: true, turns: [{ userText: 'hello', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
          { title: 'Worker A', provider, spawnedBy: 'Release agent', turns: [{ userText: 'a', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
          { title: 'Worker B', provider, spawnedBy: 'Release agent', turns: [{ userText: 'b', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
          { title: 'Loose thread', provider, turns: [{ userText: 'c', items: [{ kind: 'assistant_text', summary: 'done' }] }] },
        ],
      }],
    });
    const { projectId, path: cwd, threadIds } = seed.projects[0];
    const [agent, workerA, workerB, loose] = threadIds;
    await harness.rpc('PinThread', workerB);

    await harness.open(page);
    const agentRow = row(page, agent);
    await expect(agentRow).toHaveAttribute('data-sidebar-agent', '');
    await expect(agentRow).toHaveAttribute('data-expanded', 'true');
    await expect(agentRow.getByTestId('thread-row-agent-icon')).toBeVisible();
    await expect(agentRow.getByTestId('thread-row-pin')).toHaveCount(0);
    // The children sit behind the agent's rail; the pinned one keeps its pin
    // inside the fold and is offered no group move.
    await expect.poll(async () => (await memberIds(page)).sort()).toEqual([workerA, workerB].sort());
    await expect(row(page, workerB).getByTestId('thread-row-pin')).toHaveAttribute('data-pin-group', 'front');
    expect(await menuLabels(page, row(page, workerB))).not.toContain('Move to Group');
    // The agent block sits above the divider, Loose thread below it.
    const divider = page.getByTestId('thread-section-divider');
    await expect(divider).toHaveCount(1);
    expect((await row(page, workerB).boundingBox())!.y).toBeLessThan((await divider.boundingBox())!.y);
    expect((await row(page, loose).boundingBox())!.y).toBeGreaterThan((await divider.boundingBox())!.y);
    await capture(page, `${provider}-agent-row.png`, agentRow, row(page, workerB));

    // ---- A live spawn nests under the agent at once; a spawn naming a
    // group is refused; a loop puts the state line under the name. The
    // worker runs on the other provider so its own session answers from a
    // plain script in the same project.
    await setScenario(harness, cwd, plainScenario({ name: `agent-row-worker-${provider}`, provider: other(provider), texts: ['Working.'], afterTurns: 'silent' }));
    await setScenario(
      harness,
      cwd,
      threadToolsScenario({
        name: `agent-row-${provider}`,
        provider,
        echoUserMessage: true,
        turns: [{
          steps: [
            { call: { tool: 'thread_spawn', args: { prompt: 'Ship it.', title: 'Spawned worker', provider: other(provider) } } },
            { call: { tool: 'thread_spawn', args: { prompt: 'Ship it.', title: 'Grouped worker', provider: other(provider), group: 'Release' } } },
            { call: { tool: 'thread_loop', args: { label: 'board check', note: 'look at the board', every_seconds: 3600 } } },
          ],
          text: 'Spawned and scheduled.',
        }],
        afterTurns: 'silent',
      }),
    );
    await harness.rpc('StartSession', agent);
    await harness.rpc('SendMessage', agent, 'spawn a worker', null);
    const spawn = await awaitToolAnswer<{ thread_id: string }>(harness, { tool: 'thread_spawn', cwd });
    expect(spawn.isError, spawn.text).toBe(false);
    const spawned = spawn.value!.thread_id;
    const refused = await awaitToolAnswer(harness, { tool: 'thread_spawn', cwd });
    expect(refused.isError).toBe(true);
    expect(refused.text).toContain("This thread is an assistant's");
    expect(refused.text).toContain('Omit group');
    const loop = await awaitToolAnswer(harness, { tool: 'thread_loop', cwd });
    expect(loop.isError, loop.text).toBe(false);
    await awaitTurnCompleted(harness, agent);
    expect(await threadRows(harness)).toHaveLength(5);
    await expect.poll(() => memberIds(page)).toContain(spawned);
    await expect(row(page, spawned).getByTestId('thread-row-title')).toHaveText('Spawned worker');
    const state = stateLine(page, agent);
    await expect(state).toHaveText(/^loop board check · next (59m|1h)$/);
    await expect(page.getByTestId('thread-row-agent-state')).toHaveCount(1);

    // ---- Fold on the chevron, unfold from the keyboard.
    const chevron = agentRow.getByTestId('thread-row-agent-expand');
    await chevron.click();
    await expect(agentRow).toHaveAttribute('data-expanded', 'false');
    await expect(page.locator('[data-agent-member]')).toHaveCount(0);
    await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveText('3');
    await expect(state).toBeVisible();
    await capture(page, `${provider}-agent-folded.png`, agentRow);
    await agentRow.focus();
    await page.keyboard.press('ArrowRight');
    await expect(agentRow).toHaveAttribute('data-expanded', 'true');
    await expect.poll(() => memberIds(page)).toHaveLength(3);
    await page.keyboard.press('ArrowLeft');
    await expect(agentRow).toHaveAttribute('data-expanded', 'false');
    await page.reload();
    await expect(agentRow).toHaveAttribute('data-expanded', 'false');
    await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveText('3');

    // ---- A collapsed agent previews the focused child alone.
    await chevron.click();
    await row(page, workerB).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText('Worker B');
    await chevron.click();
    await expect.poll(() => memberIds(page)).toEqual([workerB]);
    await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveText('3');
    await capture(page, `${provider}-agent-collapsed-preview.png`, agentRow, row(page, workerB));
    await chevron.click();
    await expect.poll(() => memberIds(page)).toHaveLength(3);

    // ---- Drags: nothing joins a group through an agent. A new group holds
    // Loose thread so there is a group row to drop on.
    const ops = await harness.rpc<ThreadGroup>('CreateThreadGroup', projectId, 'Ops');
    await harness.rpc('SetThreadGroup', [loose], ops.id);
    const groupRow = page.getByTestId('thread-group-row');
    await expect(groupRow).toHaveCount(1);
    await row(page, workerA).dragTo(groupRow);
    await agentRow.dragTo(groupRow);
    await row(page, loose).dragTo(agentRow);
    await expect.poll(() => memberIds(page)).toHaveLength(3);
    expect((await rowById(harness, workerA))?.groupId ?? null).toBeNull();
    expect((await rowById(harness, agent))?.groupId ?? null).toBeNull();
    expect((await rowById(harness, loose))?.groupId).toBe(ops.id);
    await expect(groupRow.getByTestId('thread-group-row-count')).toHaveCount(0);

    // ---- Menus.
    const agentMenu = await menuLabels(page, agentRow);
    expect(agentMenu).toEqual(expect.arrayContaining(['Loops and Watches', 'New Thread, Same Assistant']));
    expect(agentMenu.some((label) => label.startsWith('Archive Done Children'))).toBe(true);
    for (const absent of ['Pin Thread', 'Move to Group']) expect(agentMenu).not.toContain(absent);
    expect(agentMenu.some((label) => /Promote|Demote/.test(label))).toBe(false);
    const childMenu = await menuLabels(page, row(page, workerA));
    expect(childMenu).toContain('Pin Thread');
    expect(childMenu.some((label) => /Promote|Demote/.test(label))).toBe(false);
    expect(childMenu).not.toContain('Move to Group');
    const looseMenu = await menuLabels(page, row(page, loose));
    expect(looseMenu).toContain('Remove from Group');
    // Loops and Watches opens the agent with its background tray open.
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Loops and Watches' }).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText('Release agent');
    await expect(page.getByTestId('activity-rail-background-toggle').filter({ visible: true })).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('[data-testid="wake-tray-row"][data-kind="loop"][data-label="board check"]')).toBeVisible();

    // ---- A child archived from its menu drops into the done footer.
    await row(page, workerA).click({ button: 'right' });
    await page.getByRole('menuitem', { name: 'Archive Thread' }).click();
    await page.getByRole('dialog').or(page.getByRole('alertdialog')).getByRole('button', { name: 'Archive', exact: true }).click();
    await expect.poll(() => memberIds(page)).toEqual(expect.not.arrayContaining([workerA]));
    const footer = page.getByTestId('thread-row-agent-done');
    await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('1 done');
    await expect(footer).toHaveAttribute('data-expanded', 'false');
    await footer.getByTestId('thread-row-agent-done-toggle').click();
    await expect(row(page, workerA)).toHaveAttribute('data-archived-child', 'true');
    expect(await row(page, workerA).evaluate((el) => getComputedStyle(el).opacity)).not.toBe('1');
    // An archived row offers no pin: pinning belongs to live rows.
    await expect(row(page, workerA).getByTestId('thread-row-pin')).toHaveCount(0);
    await expect(page.locator('[data-agent-member]', { has: row(page, workerA) })).toHaveCount(1);
    await capture(page, `${provider}-agent-done-footer.png`, agentRow, footer, row(page, workerA));
    // An archived child opens from the footer like any row.
    await row(page, workerA).click();
    await expect(page.getByTestId('chat-header-title')).toHaveText('Worker A');
    // Unarchiving returns it to the fold and clears the footer.
    await harness.rpc('UnarchiveThread', workerA);
    await expect.poll(() => memberIds(page)).toContain(workerA);
    await expect(row(page, workerA)).not.toHaveAttribute('data-archived-child', 'true');
    await expect(footer).toHaveCount(0);

    // ---- Archive Done Children archives every idle child behind a confirm.
    await agentRow.click({ button: 'right' });
    await page.getByRole('menuitem', { name: /^Archive Done Children \(3\)$/ }).click();
    const dialog = page.getByRole('dialog').or(page.getByRole('alertdialog'));
    await expect(dialog).toContainText('3 spawned threads');
    await dialog.getByRole('button', { name: 'Archive', exact: true }).click();
    await expect.poll(() => memberIds(page)).toEqual([]);
    await expect(footer.getByTestId('thread-row-agent-done-label')).toHaveText('3 done');
    await expect.poll(async () => (await harness.rpc<SidebarRow[]>('HarnessListThreadRows')).map((r) => r.id).sort()).toEqual([agent, loose].sort());
    // The footer lists the three, newest first, dimmed.
    await footer.getByTestId('thread-row-agent-done-toggle').click();
    await expect(page.locator('[data-archived-child]')).toHaveCount(3);
    await expect(agentRow.getByTestId('thread-row-agent-count')).toHaveCount(0);

  });
}

test('a spawned thread that is an assistant leaves the fold, and a thread nobody spawned never joins it', async ({ harness, page }) => {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{
      name: 'agent-row-nesting', repo: {},
      threads: [
        { title: 'Lead', turns: [{ userText: 'lead', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
        { title: 'Child', spawnedBy: 'Lead', turns: [{ userText: 'child', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
        { title: 'Grandchild', spawnedBy: 'Child', turns: [{ userText: 'gc', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
        { title: 'Stranger', turns: [{ userText: 's', items: [{ kind: 'assistant_text', summary: 'ready' }] }] },
      ],
    }],
  });
  const [lead, child, grandchild, stranger] = seed.projects[0].threadIds;
  await harness.rpc('HarnessMakeAssistant', lead);
  await harness.open(page);
  // Only the lead's direct spawn nests; the grandchild's spawner is not an
  // agent, so it stays a plain row with the stranger.
  await expect.poll(() => memberIds(page)).toEqual([child]);
  for (const id of [grandchild, stranger]) await expect(page.locator('[data-agent-member]', { has: row(page, id) })).toHaveCount(0);

  // The child made an assistant is an agent row of its own with the grandchild under it.
  await harness.rpc('HarnessMakeAssistant', child);
  // Its agent row arrives as its member row leaves: wait out the exit.
  await expect(page.locator(`[data-sidebar-agent][data-sidebar-thread-id="${child}"]`)).toBeVisible();
  await expect(row(page, child)).toHaveCount(1);
  await expect.poll(() => memberIds(page)).toEqual([grandchild]);
  await expect(row(page, lead).getByTestId('thread-row-agent-count')).toHaveCount(0);
  await row(page, lead).getByTestId('thread-row-agent-expand').click();
  await expect(row(page, lead).getByTestId('thread-row-agent-count')).toHaveCount(0);

  // The store refuses to file an agent or its child in a group.
  const group = await harness.rpc<ThreadGroup>('CreateThreadGroup', seed.projects[0].projectId, 'Nope');
  for (const id of [lead, grandchild]) {
    const refusal = await harness.rpc('SetThreadGroup', [id], group.id).then(() => null, (err: Error) => err.message);
    expect(refusal, id).toMatch(/standing agent/);
  }
  await harness.rpc('SetThreadGroup', [stranger], group.id);
  expect((await rowById(harness, stranger))?.groupId).toBe(group.id);
});
