// Shared setup for the background tray's wake rows (wake-rows.spec.ts and
// compact-wake-rows.spec.ts): a standing agent seeded with its scripts, the
// tray row locators, reads of the rows the tray shows, and captures.
import { existsSync } from 'node:fs';
import { copyFile, mkdir, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import type { Locator, Page } from '@playwright/test';

import { expect, test, type SeedResult } from './fixtures.js';
import type { HarnessApp, HarnessMockEventData } from '../src/harness.js';
import {
  awaitToolAnswer,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
  type ScriptedTurn,
} from './thread-tools-helpers.js';

/** The computer name a capture may show. */
export const DEVICE_NAME = 'Wake tray computer';

// ci.sh reports a failed pipeline with a line on stderr; hold.sh parks while
// hold.flag exists, so a run can be caught in flight.
const SCRIPTS: Record<string, string> = {
  'ci.sh': `echo "ci: checked pipeline 12 at the default branch" >&2
echo "pipeline 12 failed: lint"
`,
  'hold.sh': `: > "$AO_AGENT_HOME/hold.running"
while [ -f "$AO_AGENT_HOME/hold.flag" ]; do sleep 0.1; done
rm -f "$AO_AGENT_HOME/hold.running"
`,
};

export const run = (script: string) => `exec sh "$AO_AGENT_HOME/${script}"`;

export interface WakeAgent {
  thread: string;
  cwd: string;
  home: string;
  dataDir: string;
}

/** Seeds one thread, promotes it to a standing agent and writes its scripts. */
export async function seedWakeAgent(harness: HarnessApp, provider: ProviderName, tag: string, title: string): Promise<WakeAgent> {
  await harness.rpc('SetDeviceName', DEVICE_NAME);
  const seed = await harness.rpc<SeedResult>('HarnessSeed', {
    projects: [{ name: `${tag}-${provider}`, repo: {}, threads: [{ title, provider }] }],
  });
  const { path: cwd, threadIds } = seed.projects[0];
  const thread = threadIds[0];
  const agent = await harness.rpc<{ home: string }>('PromoteStandingAgent', thread);
  for (const [name, body] of Object.entries(SCRIPTS)) await writeFile(join(agent.home, name), body);
  return { thread, cwd, home: agent.home, dataDir: harness.bootstrap.dataDir };
}

/**
 * Runs the first turn, which registers through `calls`, and waits for each
 * tool's answer. Later turns answer every wake with `reply`.
 */
export async function registerThrough(
  harness: HarnessApp,
  provider: ProviderName,
  agent: WakeAgent,
  name: string,
  calls: { tool: string; args: Record<string, unknown> }[],
  later: ScriptedTurn[] = [],
): Promise<Record<string, unknown>[]> {
  await setScenario(
    harness,
    agent.cwd,
    threadToolsScenario({
      name,
      provider,
      echoUserMessage: true,
      turns: [{ steps: calls.map((call) => ({ call })), text: 'Scheduled.' }, ...later, { text: 'Noted.' }],
      afterTurns: 'repeatLast',
    }),
  );
  await harness.rpc('StartSession', agent.thread);
  await harness.rpc('SendMessage', agent.thread, 'set up the schedule', null);
  const answers: Record<string, unknown>[] = [];
  for (const call of calls) {
    const answer = await awaitToolAnswer(harness, { tool: call.tool, cwd: agent.cwd, timeoutMs: 60_000 });
    expect(answer.isError, answer.text).toBe(false);
    answers.push(answer.value!);
  }
  await awaitTurnCompleted(harness, agent.thread);
  return answers;
}

/** The next wake the mock takes in whose text includes `contains`. */
export function wakeInput(harness: HarnessApp, cwd: string, contains: string, timeoutMs = 60_000) {
  return harness.waitForEvent<HarnessMockEventData>(
    'harness:mock',
    (ev) => ev.report.kind === 'user_input' && ev.cwd === cwd && (ev.report.input ?? '').includes(contains),
    timeoutMs,
  );
}

/** The next `thread:wakes_changed` frame for a thread. */
export function wakesChanged(harness: HarnessApp, thread: string, timeoutMs = 30_000) {
  return harness.waitForEvent<{ threadId: string }>('thread:wakes_changed', (data) => data.threadId === thread, timeoutMs);
}

export function trayRow(page: Page, kind: 'loop' | 'watch' | 'reminder', label: string): Locator {
  return page.locator(`[data-testid="wake-tray-row"][data-kind="${kind}"][data-label="${label}"]`);
}

/** Opens the thread and its Background tray. */
export async function openTray(page: Page, title: string): Promise<void> {
  await page.getByTestId('thread-row').getByText(title, { exact: true }).click();
  const toggle = page.getByTestId('activity-rail-background-toggle').filter({ visible: true });
  await expect(toggle).toBeVisible();
  if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
  await expect(page.getByTestId('activity-rail-background-body').filter({ visible: true })).toBeVisible();
}

export interface DbWake {
  id: string;
  state: string;
  last_outcome: string;
  next_at: number | null;
  last_stderr: string;
}

export function dbWake(dataDir: string, thread: string, label: string): DbWake | undefined {
  const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return db
      .prepare(
        `SELECT id, state, last_outcome, next_at, last_stderr
           FROM agent_wakes WHERE thread_id = ? AND label = ? AND state <> 'cancelled'`,
      )
      .get(thread, label) as DbWake | undefined;
  } finally {
    db.close();
  }
}

export interface DbScheduleRow {
  id: string;
  tool: string;
  next_fire_at: number;
  next_fire_from: number;
}

/** The thread's schedule calls whose answers named a next fire, as triage stamped them. */
export function dbScheduleRows(dataDir: string, thread: string): DbScheduleRow[] {
  const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    return db
      .prepare(
        `SELECT id, json_extract(meta, '$.mcp.tool') AS tool, json_extract(meta, '$.next_fire_at') AS next_fire_at,
                json_extract(meta, '$.next_fire_from') AS next_fire_from
           FROM items WHERE thread_id = ? AND json_extract(meta, '$.next_fire_at') IS NOT NULL`,
      )
      .all(thread) as unknown as DbScheduleRow[];
  } finally {
    db.close();
  }
}

/**
 * The `· next …` a schedule row's summary ends with, formatted in the page
 * as the app formats it: the time alone when the fire is on the row's own
 * day, the date as well when it is not.
 */
export async function expectedNextFire(page: Page, row: DbScheduleRow): Promise<string> {
  return await page.evaluate(({ at, from }) => {
    const time = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(at);
    if (new Date(at).toDateString() === new Date(from).toDateString()) return `next ${time}`;
    const day = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' }).format(at);
    return `next ${day} ${time}`;
  }, { at: row.next_fire_at, from: row.next_fire_from });
}

/** A reminder's request state, or undefined once it is gone. */
export function dbReminderState(dataDir: string, token: string): string | undefined {
  const db = new DatabaseSync(join(dataDir, 'agent-overflow.db'), { readOnly: true });
  try {
    const row = db.prepare(`SELECT state FROM thread_requests WHERE token = ?`).get(token) as { state: string } | undefined;
    return row?.state;
  } finally {
    db.close();
  }
}

/** Parks the next hold.sh run until the returned release is called. */
export async function holdNextRun(home: string): Promise<{ running: () => Promise<void>; release: () => Promise<void> }> {
  await writeFile(join(home, 'hold.flag'), '');
  return {
    running: () => expect.poll(() => existsSync(join(home, 'hold.running')), { timeout: 30_000 }).toBe(true),
    release: async () => {
      await rm(join(home, 'hold.flag'), { force: true });
    },
  };
}

/**
 * Captures the window once every target is visible and wholly in it. The
 * harness's temporary root, which a watch's directory shows, is written as
 * `<harness root>` in the capture: the spec asserts the real path before it,
 * and the captures are published. The sidebar's system footer, the host
 * machine's own CPU and memory, is not evidence and is hidden.
 */
export async function capture(page: Page, name: string, ...targets: Locator[]): Promise<void> {
  for (const target of targets) {
    await expect(target).toBeVisible();
    await expect(target).toBeInViewport({ ratio: 1 });
  }
  await page.evaluate(() => {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent ?? '';
      if (text.includes('/ao-harness-')) node.textContent = text.replace(/\/\S*?\/ao-harness-[^/\s]+/g, '<harness root>');
    }
  });
  await page.evaluate(
    () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
  );
  await page.screenshot({
    path: test.info().outputPath(name),
    animations: 'disabled',
    style: '[data-testid="sidebar-system-stats"] { display: none !important; }',
  });
}

/**
 * capture, also copied to AO_AGENT_MESSAGE_SHOTS when it is set, since the
 * next run wipes the run's output: the agent message rows' published shots.
 */
export async function captureAgentMessage(page: Page, name: string, ...targets: Locator[]): Promise<void> {
  await capture(page, name, ...targets);
  const dir = process.env.AO_AGENT_MESSAGE_SHOTS;
  if (!dir) return;
  await mkdir(dir, { recursive: true });
  await copyFile(test.info().outputPath(name), `${dir}/${name}`);
}
