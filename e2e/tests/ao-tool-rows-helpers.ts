// One lead thread that calls each kind of AO tool through a mock provider
// making real MCP calls, shared by ao-tool-rows.spec.ts (desktop) and
// compact-ao-tool-rows.spec.ts (compact): a spawn whose prompt is long
// enough for the wire projection to clip, a send with notify, a status
// read of that request, a search, a show, a loop, a run start and a run
// status, and a show of a thread that does not exist.

import { expect, type Locator, type Page } from '@playwright/test';

import type { HarnessApp } from '../src/harness.js';
import type { SeedResult } from './fixtures.js';
import { escapeTimelineFollow, timelineOf } from './timeline-follow-helpers.js';
import {
  RESULT_TOKEN_PATTERN,
  awaitToolAnswer,
  releaseAfterBackgrounded,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import {
  START_RUN_PATTERN,
  WORKFLOW_TOOLS_SERVER,
  answerPhasesDone,
  doneFlowYaml,
  otherProvider,
} from './workflows-agent-tools-helpers.js';

export const LEAD_TITLE = 'Lead thread';
export const TARGET_TITLE = 'Target thread';
export const SPAWN_TITLE = 'Release notes';

/** Over the 2 KiB meta budget on its own, so the row ships its opening only. */
export const SPAWN_PROMPT = `Write the release notes for the next version.\n\n${'Each section lists what changed and why.\n'.repeat(80)}`;

export interface AoToolRowsRun {
  leadThreadId: string;
  targetThreadId: string;
  spawnedThreadId: string;
  runId: string;
}

/**
 * Seeds the lead's project (with one workflow) and a target project with an
 * existing thread, scripts the lead's turn and drives it to completion.
 * The spawned thread and the target answer at once, so both requests settle
 * and their wakes land in the lead.
 */
export async function driveAoToolRows(harness: HarnessApp, provider: ProviderName, tag: string): Promise<AoToolRowsRun> {
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [
      {
        name: `ao-rows-lead-${tag}`,
        repo: {},
        threads: [{ title: LEAD_TITLE, provider }],
        workflows: {
          definitions: [
            {
              name: 'digest',
              yaml: doneFlowYaml('digest', otherProvider(provider)),
              prompts: { 'digest.md': 'Do the work and report the outcome.' },
            },
          ],
        },
      },
      { name: `ao-rows-target-${tag}`, repo: {}, threads: [{ title: TARGET_TITLE, provider }] },
    ],
  });
  const lead = seed.projects[0];
  const target = seed.projects[1];
  const leadThreadId = lead.threadIds[0];
  const targetThreadId = target.threadIds[0];

  await answerPhasesDone(harness, otherProvider(provider));
  // The spawned thread and the target both run here, and each answers
  // only once both calls have returned, so neither answer rides back in
  // its call's reply.
  await setScenario(
    harness,
    target.path,
    threadToolsScenario({ name: `ao-rows-target-${tag}`, provider, turns: [{ steps: [{ gate: 'ao-rows-answer' }], text: 'Rebased and green.' }] }),
  );
  await setScenario(
    harness,
    lead.path,
    threadToolsScenario({
      name: `ao-rows-lead-${tag}`,
      provider,
      echoUserMessage: true,
      turns: [
        {
          steps: [
            {
              call: {
                tool: 'thread_spawn',
                args: { prompt: SPAWN_PROMPT, title: SPAWN_TITLE, project_id: target.projectId, notify: true },
              },
            },
            {
              call: {
                tool: 'thread_send',
                args: { thread_id: targetThreadId, message: 'Rebase onto main\nthen run the tests.', notify: true },
              },
            },
            { capture: { var: 'SEND', from: '${MCP_RESULT}', pattern: RESULT_TOKEN_PATTERN } },
            { call: { tool: 'thread_status', args: { tokens: ['${SEND}'] } } },
            { call: { tool: 'thread_search', args: { query: 'Target' } } },
            { call: { tool: 'thread_show', args: { thread_id: targetThreadId, window: 'last_answer' } } },
            { call: { tool: 'thread_loop', args: { label: 'board', note: 'Check the board.', every_seconds: 600 } } },
            { call: { tool: 'thread_show', args: { thread_id: 'nosuch1' } } },
            {
              call: {
                server: WORKFLOW_TOOLS_SERVER,
                tool: 'run_start',
                args: { workflow_id: 'digest', goal: 'Weekly summary', seeds: { goal: 'Weekly summary' } },
              },
            },
            { capture: { var: 'RUN', from: '${MCP_RESULT}', pattern: START_RUN_PATTERN } },
            { call: { server: WORKFLOW_TOOLS_SERVER, tool: 'run_status', args: { run_id: '${RUN}' } } },
          ],
          text: 'Started the work.',
        },
        { text: 'Read the answer.' },
      ],
    }),
  );

  await harness.startSession(leadThreadId);
  await harness.send(leadThreadId, 'get the release moving');
  const spawn = await awaitToolAnswer<{ thread_id: string; outcome: string }>(harness, { tool: 'thread_spawn', cwd: lead.path });
  const send = await awaitToolAnswer<{ outcome: string }>(harness, { tool: 'thread_send', cwd: lead.path });
  for (const answer of [spawn, send]) await releaseAfterBackgrounded(answer, { harness, gate: 'ao-rows-answer', cwd: target.path });
  // run_status is the turn's last call; the refusal before it is asserted
  // where it shows, on its row.
  const status = await harness.awaitMcpResult({ server: WORKFLOW_TOOLS_SERVER, tool: 'run_status', cwd: lead.path, timeoutMs: 60_000 });
  expect(status.report.isError, status.report.result).toBeFalsy();
  const runId = /"itemId":"([^"]+)"/.exec(status.report.result ?? '')?.[1] ?? '';
  expect(runId).not.toBe('');
  return { leadThreadId, targetThreadId, spawnedThreadId: spawn.value!.thread_id, runId };
}

/** Opens the lead thread with every activity run expanded, once its row with this gutter verb shows. */
export async function openLead(harness: HarnessApp, page: Page, firstLabel = 'spawn'): Promise<void> {
  await harness.open(page);
  if ((await page.locator('html').getAttribute('data-compact-screen')) === 'thread') {
    await page.getByTestId('compact-back').click();
  }
  await page.getByTestId('thread-row-title').filter({ hasText: LEAD_TITLE }).first().click();
  // The turn's calls fold into activity runs; every run opens so each row
  // is on the page.
  const runs = page.getByTestId('activity-run');
  await expect(runs.first()).toBeAttached({ timeout: 30_000 });
  for (let index = 0; index < (await runs.count()); index++) {
    const run = runs.nth(index);
    if ((await run.getAttribute('data-collapsed')) === 'true') await run.getByTestId('activity-run-header').click();
  }
  await expect(page.getByTestId('tool-call-card-label').filter({ hasText: new RegExp(`^${firstLabel}$`) }).first()).toBeAttached();
}

/** The one tool row with this gutter verb whose header reads `preview`. */
export function toolRow(page: Page, label: string, preview: string | RegExp): Locator {
  return page
    .getByTestId('tool-call-card')
    .filter({ has: page.getByTestId('tool-call-card-label').filter({ hasText: new RegExp(`^${label}$`) }) })
    .filter({ has: page.getByTestId('tool-call-card-preview').filter({ hasText: preview }) });
}

/** Whether the page's primary pointer is coarse: a touch screen, as on the compact project. */
function touchScreen(page: Page): Promise<boolean> {
  return page.evaluate(() => matchMedia('(pointer: coarse)').matches);
}

/**
 * Opens a row as a person on this device does, a tap on a touch screen and
 * a mouse click otherwise, and waits for it to read as open. Every spec
 * opens a tool row through it. The press is made on the row placed as
 * `placeRow` places it, clear of the composer and the "Jump to bottom"
 * chip. Playwright presses the toggle's center cut to 0.01 px and checks
 * with elementsFromPoint that the toggle is there, but Chromium hit-tests
 * the press at another point: a mouse press less than a pixel above the
 * chip goes to the chip and its release to the surface behind it, and a
 * tap goes to what is at the nearest whole pixel, the composer when the
 * center sits in the half pixel above it. Either way the row stays closed
 * and Playwright reports no interception. `beforePress` runs once the row
 * is where it will be pressed, for a caller that measures what opening
 * moves.
 */
export async function expandRow(row: Locator, beforePress?: () => Promise<void>): Promise<void> {
  const toggle = row.getByTestId('tool-call-card-toggle');
  await placeRow(row);
  await beforePress?.();
  if (await touchScreen(row.page())) await toggle.tap();
  else await toggle.click();
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
}

/** The "Jump to bottom" chip of the timeline that holds `row`. */
export function jumpChipOf(row: Locator): Locator {
  return timelineOf(row).locator('xpath=..').getByTestId('scroll-to-bottom');
}

/**
 * Runs `press` with the row's toggle center, cut to 0.01 px as Playwright
 * presses it, where the press that this device makes reaches `overlay`
 * (the "Jump to bottom" chip or the composer, which float over the
 * timeline's bottom) while elementFromPoint finds the toggle, then gives
 * the row back to its timeline's layout. Whole pixels come from scrolling
 * the timeline, or the row's activity run clip where the timeline stops;
 * the fraction from offsetting the row.
 *
 * A tap is hit-tested at the nearest whole pixel, so the center goes a
 * quarter pixel above the first y where elementFromPoint finds the
 * overlay, and the fixture fails unless that whole pixel is the
 * overlay's. A mouse press goes less than a pixel above the overlay's top:
 * there Chromium sends it to the chip while elementFromPoint finds the
 * toggle both at the press point and at the press's own clientY, so the
 * page can name no point that shows the miss, and that leg checks only
 * where it presses.
 */
export async function atOverlayEdge(row: Locator, overlay: Locator, press: () => Promise<void>): Promise<void> {
  const toggle = row.getByTestId('tool-call-card-toggle');
  const touch = await touchScreen(row.page());
  await placeRow(row);
  await expect(overlay).toBeVisible();
  const toggleBox = (await toggle.boundingBox())!;
  const x = Math.trunc((toggleBox.x + toggleBox.width / 2) * 100) / 100;
  // The first y, in 1/64 px steps, where elementFromPoint at the press's x
  // finds the overlay; the overlay floats over the row, so the row's own
  // position does not move it. Only the tap leg aims from it.
  const hitTop = !touch ? null : await overlay.evaluate((el, at) => {
    const top = el.getBoundingClientRect().top;
    for (let y = Math.floor(top) - 2; y <= top + 1; y += 1 / 64) {
      if (el.contains(document.elementFromPoint(at, y))) return y;
    }
    return null;
  }, x);
  if (touch) expect(hitTop, `elementFromPoint finds the overlay nowhere near its top at x ${x}`).not.toBeNull();
  const want = touch ? hitTop! - 1 / 4 : (await overlay.boundingBox())!.y - 1 + 1 / 64;
  await row.evaluate(async (card, want) => {
    const button = card.querySelector('[data-testid="tool-call-card-toggle"]')!;
    const timeline = card.closest<HTMLElement>('[data-testid="message-timeline-scroll"]')!;
    const clip = card.closest<HTMLElement>('[data-testid="activity-run-clip"]');
    const short = () => {
      const box = button.getBoundingClientRect();
      return want - (box.top + box.height / 2);
    };
    const frames = () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    for (let attempt = 0; attempt < 4 && !(short() >= 0 && short() < 1); attempt++) {
      const move = Math.floor(short());
      const before = timeline.scrollTop;
      timeline.scrollTop = before - move;
      const moved = before - timeline.scrollTop;
      if (clip && moved !== move) clip.scrollTop -= move - moved;
      await frames();
    }
    card.style.position = 'relative';
    card.style.top = `${short()}px`;
    await frames();
  }, want);
  try {
    const box = (await toggle.boundingBox())!;
    const edge = (await overlay.boundingBox())!;
    const at = { x: Math.trunc((box.x + box.width / 2) * 100) / 100, y: Math.trunc((box.y + box.height / 2) * 100) / 100 };
    const where = `press at ${at.x},${at.y}; overlay at ${edge.x},${edge.y} ${edge.width}x${edge.height}${touch ? `, hit from y ${hitTop}` : ''}`;
    if (!touch) {
      expect(at.y, where).toBeGreaterThan(edge.y - 1);
      expect(at.y, where).toBeLessThan(edge.y);
    }
    expect(at.x, where).toBeGreaterThan(edge.x);
    expect(at.x, where).toBeLessThan(edge.x + edge.width);
    const overlayTestId = (await overlay.getAttribute('data-testid'))!;
    const found = (point: { x: number; y: number }) =>
      toggle.evaluate((button, { point, overlayTestId }) => {
        const el = document.elementFromPoint(point.x, point.y);
        return {
          toggle: el !== null && button.contains(el),
          overlay: el?.closest(`[data-testid="${overlayTestId}"]`) != null,
          testId: el?.closest('[data-testid]')?.getAttribute('data-testid'),
        };
      }, { point, overlayTestId });
    const there = await found(at);
    expect(there.toggle, `elementFromPoint at the ${where} finds ${there.testId}, not the toggle`).toBe(true);
    if (touch) {
      const tapped = { x: Math.round(at.x), y: Math.round(at.y) };
      const hit = await found(tapped);
      expect(
        hit.overlay,
        `the setup no longer reproduces the miss: a tap at the ${where} is hit-tested at ${tapped.x},${tapped.y}, where elementFromPoint finds ${hit.testId}, not the overlay`,
      ).toBe(true);
    }
    await press();
  } finally {
    await row.evaluate((card) => {
      card.style.position = '';
      card.style.top = '';
    });
  }
}

/** Screenshots a row placed as `placeRow` places it. */
export async function shootRow(row: Locator, path: string): Promise<void> {
  await placeRow(row);
  await row.screenshot({ path, animations: 'disabled' });
}

/**
 * Centers a row in its timeline and waits until nothing is moving it. A
 * row merely scrolled into view can sit under the composer or the "Jump to
 * bottom" chip, which float over the timeline's bottom edge, and a timeline
 * following a live tail pulls a script-scrolled row away again: the follow
 * is left as a reader leaves it, then the row is centered and must hold
 * still on screen.
 */
async function placeRow(row: Locator): Promise<void> {
  const page = row.page();
  const timeline = timelineOf(row);
  await escapeTimelineFollow(timeline);
  await row.evaluate((element) => element.scrollIntoView({ block: 'center' }));
  let last = '';
  await expect(async () => {
    const box = await row.boundingBox();
    const viewport = page.viewportSize()!;
    const scrollTop = await timeline.evaluate((el) => el.scrollTop);
    const onScreen = box !== null && box.y >= 0 && box.y + box.height <= viewport.height;
    const at = onScreen ? `${Math.round(box.x)},${Math.round(box.y)},${Math.round(box.height)}` : '';
    const still = at !== '' && at === last;
    last = at;
    const where = box ? `y ${Math.round(box.y)}, height ${Math.round(box.height)}` : 'no box';
    expect(still, `row ${where} in a ${viewport.width}x${viewport.height} viewport, timeline scrollTop ${Math.round(scrollTop)}`).toBe(true);
  }).toPass({ intervals: [150], timeout: 10_000 });
}

/** Whether an element's first text runs to its end inside the element's own box. */
export function firstTextFits(element: Locator): Promise<boolean> {
  return element.evaluate((el) => {
    const text = document.createTreeWalker(el, NodeFilter.SHOW_TEXT).nextNode();
    if (!text) return true;
    const range = document.createRange();
    range.selectNodeContents(text);
    return range.getBoundingClientRect().right <= el.getBoundingClientRect().right + 0.5;
  });
}

/**
 * Checks the row's gutter verb shows whole in its fixed column and, once
 * open, that every listed line's title shows whole: a line's detail is
 * what truncation cuts.
 */
export async function expectRowTextWhole(row: Locator): Promise<void> {
  const label = row.getByTestId('tool-call-card-toggle-label-slot').first();
  const verb = (await label.textContent())?.trim() ?? '';
  expect(await label.evaluate((el) => el.scrollWidth <= el.clientWidth), `gutter verb "${verb}" clipped`).toBe(true);
  for (const title of await row.getByTestId('tool-call-card-ao-line-title').all()) {
    expect(await firstTextFits(title), `line title "${await title.textContent()}" clipped`).toBe(true);
  }
  for (const link of await row.getByTestId('tool-call-card-ao-line-link').all()) {
    expect(await link.evaluate((el) => el.scrollWidth <= el.clientWidth), `line link "${await link.textContent()}" clipped`).toBe(true);
  }
}
