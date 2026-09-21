// Scenario and steps shared by the background tray digest specs (desktop
// load and compact): one background agent whose transcript is many tool
// rows, streamed across two turns so the second turn's rows land while
// the digest is open.
import type { Page } from '@playwright/test';
import { expect } from './fixtures.js';
import {
  RESULT_LINE, asyncAgentAckLine, backgroundTasksChangedLine, taskStartedLine,
  textLines, toolResultLine, toolUseLine, type ScenarioStep,
} from './agent-visibility-helpers.js';

export const AGENT = 'tu-agent';
export const FIRST_BATCH = 120;
export const SECOND_BATCH = 30;

export function childUse(i: number): string {
  return toolUseLine(`msg-child-${i}`, `tu-child-${i}`, 'Read', { file_path: `/src/file-${i}.ts` }, AGENT);
}

export function childResult(i: number): string {
  return toolResultLine(`tu-child-${i}`, `contents of file ${i}`, { parentToolUseId: AGENT });
}

export function childTools(from: number, count: number): string[] {
  const lines: string[] = [];
  for (let i = from; i < from + count; i++) lines.push(childUse(i), childResult(i));
  return lines;
}

/**
 * `pairs` settled top-level tool calls on the main thread: two top-level
 * rows each (call and completion), no text reveal to wait on.
 */
export function mainFiller(from: number, pairs: number): string[] {
  const lines: string[] = [];
  for (let i = from; i < from + pairs; i++) {
    lines.push(
      toolUseLine(`msg-filler-${i}`, `tu-filler-${i}`, 'Bash', { command: `echo filler ${i}` }),
      toolResultLine(`tu-filler-${i}`, `filler ${i}`),
    );
  }
  return lines;
}

/**
 * Filler that stays tall: each Bash pair is followed by a line of prose,
 * so no activity run folds the rows away and a reopened thread's
 * viewport fills long before the launch. Three rows per unit.
 */
export function mainProseFiller(from: number, units: number): string[] {
  const lines: string[] = [];
  for (let i = from; i < from + units; i++) {
    lines.push(...mainFiller(i, 1), ...textLines(`msg-prose-${i}`, `Note ${i}.`));
  }
  return lines;
}

/** A real touch drag inside `target`, from `fromY` to `toY` (page coordinates). */
export async function swipe(page: Page, x: number, fromY: number, toY: number): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  try {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: fromY }] });
    const steps = 12;
    for (let i = 1; i <= steps; i++) {
      const y = fromY + ((toY - fromY) * i) / steps;
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y }] });
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  } finally {
    await cdp.detach();
  }
}

export function launchLines(): string[] {
  return [
    ...textLines('msg-lead', 'Launching the reader.'),
    toolUseLine('msg-agent', AGENT, 'Agent', { description: 'read everything', subagent_type: 'reader' }),
    taskStartedLine('task-agent', AGENT, 'read everything'),
    asyncAgentAckLine(AGENT, 'task-agent', 'read everything'),
    backgroundTasksChangedLine([{ task_id: 'task-agent', task_type: 'local_agent', description: 'read everything' }]),
  ];
}

/** Two-turn Claude scenario; `afterTurns: 'silent'` so a stray send replays nothing. */
export function twoTurnScenario(name: string, first: ScenarioStep[], second: ScenarioStep[]): unknown {
  return {
    version: 1,
    name,
    provider: 'claude',
    turns: [{ label: `${name}-1`, steps: first }, { label: `${name}-2`, steps: second }],
    afterTurns: 'silent',
  };
}

export { RESULT_LINE };

export async function openTrayDigest(page: Page) {
  await page.getByTestId('activity-rail-background-toggle').click();
  const row = page.getByTestId('background-task-tray-row');
  await expect(row).toHaveCount(1);
  await row.getByTestId('agent-row-toggle').click();
  const digest = row.getByTestId('background-task-tray-row-digest');
  const clip = digest.getByTestId('subagent-group-scroll');
  await expect(clip).toBeVisible();
  return { row, digest, clip, child: (i: number) => digest.locator(`[data-item-id="tu-child-${i}"]`) };
}
