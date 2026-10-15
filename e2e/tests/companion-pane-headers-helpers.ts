import type { Locator, Page } from '@playwright/test';
import { expect } from '@playwright/test';

import type { SeedResult } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import { awaitToolAnswer, awaitTurnCompleted, setScenario, threadToolsScenario } from './thread-tools-helpers.js';

/** The title floor a narrow chat header keeps: 8rem. */
export const TITLE_FLOOR_PX = 128;

/** The pane strip does not scroll sideways. */
export async function expectStripFits(page: Page): Promise<void> {
  const strip = page.locator('section[data-pane-id]').first().locator('xpath=..');
  await expect.poll(() => strip.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
}

/**
 * Each pane's title is whole, or at least the floor wide and truncated with
 * an ellipsis, or a fade under compact; a header under 36rem leaves the
 * project crumb out; and no header overflows its pane.
 */
export async function expectHeadersShareTheWidth(panes: Locator[]): Promise<void> {
  for (const pane of panes) {
    const title = pane.getByTestId('chat-header-title');
    const geometry = await title.evaluate((el) => {
      // Under compact the text sits in a swipeable scroller that fades.
      const text = el.querySelector<HTMLElement>('.pane-title-fade') ?? el;
      return {
        client: el.clientWidth,
        overflows: text.scrollWidth > text.clientWidth + 1,
        cut: text === el ? getComputedStyle(el).textOverflow === 'ellipsis' : true,
      };
    });
    if (geometry.overflows) {
      expect(geometry.client, 'a truncated title keeps the floor').toBeGreaterThanOrEqual(TITLE_FLOOR_PX);
      expect(geometry.cut, 'a truncated title ends in an ellipsis or a fade').toBe(true);
    }
    const header = pane.getByTestId('chat-header');
    // The compact facts line carries the project on a row of its own.
    const desktop = (await pane.getByTestId('chat-header-title-row').count()) === 0;
    if (desktop && await header.evaluate((el) => el.clientWidth < 576)) {
      await expect(pane.getByTestId('chat-header-project'), 'a narrow header leaves the crumb out').toBeHidden();
    }
    const paneBox = (await pane.boundingBox())!;
    const overflow = await header.evaluate((el) => {
      const right = Math.max(...[...el.querySelectorAll('*')].map((child) => child.getBoundingClientRect().right));
      return right;
    });
    expect(overflow, 'nothing in the header runs past the pane').toBeLessThanOrEqual(paneBox.x + paneBox.width + 1);
  }
}

export const LEAD = 'Release lead for the quarterly platform migration';
export const WORKER = 'Changelog worker for the quarterly platform migration';

/** An assistant whose spawn holds, so its Agents row stays to click. */
export async function seedLeadWithWorker(harness: HarnessApp, tag: string): Promise<{ token: string; worker: string }> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      { name: `${tag}-lead`, repo: {}, threads: [{ title: LEAD, assistant: true }] },
      { name: `${tag}-worker`, repo: {}, threads: [] },
    ],
  });
  const [leadProject, workerProject] = seed.projects;
  const lead = leadProject.threadIds[0];
  await setScenario(harness, leadProject.path, threadToolsScenario({
    name: `${tag}-lead`, provider: 'claude', echoUserMessage: true,
    turns: [{ steps: [{ call: { tool: 'thread_spawn', args: { prompt: 'Write the changelog.', title: WORKER, project_id: workerProject.projectId, notify: true } } }], text: 'Started.' }, { text: 'Noted.' }],
    afterTurns: 'repeatLast',
  }));
  await setScenario(harness, workerProject.path, threadToolsScenario({
    name: `${tag}-worker`, provider: 'claude', turns: [{ steps: [{ gate: 'worker-hold' }], text: 'Written.' }],
  }));
  await harness.rpc('StartSession', lead);
  await harness.rpc('SendMessage', lead, 'Get the changelog written.', null);
  const spawn = await awaitToolAnswer<{ token: string; thread_id: string }>(harness, { tool: 'thread_spawn', cwd: leadProject.path, timeoutMs: 60_000 });
  expect(spawn.isError, spawn.text).toBe(false);
  await awaitTurnCompleted(harness, lead);
  return { token: spawn.value!.token, worker: spawn.value!.thread_id };
}

export function pane(page: Page, kind: string): Locator {
  return page.locator(`section[data-pane-kind="${kind}"]`);
}
