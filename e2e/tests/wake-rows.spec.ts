// The open thread's loops, watches and reminders in the composer's
// background tray (docs/specs/standing-agents.md, "Activity rail"), end to
// end on both mock providers.
//
// Coverage: rows registered through real thread_loop, thread_watch and
// thread_remind calls, each with its kind, label, next fire or running, and
// the tray toggle counting them; the registering calls' timeline rows naming
// the next fire; Wake now firing a watch, a loop and a reminder through the
// sweep's own path, the loop and the watch keeping their schedule, and the
// watch's expanded row showing its command, directory and last stderr;
// Wake now refused, with the reason, for a run in flight, a long-running
// watch, a cancelled row and an archived thread; Cancel removing each kind;
// rows following the sweep live as a short loop fires; and a wake whose
// session ended before taking it in turning its row's outcome to a warning
// without a reload.
import type { Page } from '@playwright/test';

import { test, expect } from './fixtures.js';
import type { HarnessApp } from '../src/harness.js';
import {
  awaitGate,
  awaitTurnCompleted,
  setScenario,
  threadToolsScenario,
  type ProviderName,
} from './thread-tools-helpers.js';
import {
  capture,
  dbReminderState,
  dbScheduleRows,
  dbWake,
  expectedNextFire,
  holdNextRun,
  openTray,
  registerThrough,
  run,
  seedWakeAgent,
  trayRow,
  wakeInput,
} from './wake-rows-helpers.js';

const UNDELIVERED = 'not delivered: the session ended before it took the wake in';

async function toggleCount(page: Page): Promise<string> {
  return ((await page.getByTestId('activity-rail-background-count').filter({ visible: true }).textContent()) ?? '').trim();
}

async function expectRefused(harness: HarnessApp, args: [string, string, string], reason: RegExp): Promise<void> {
  const refusal = await harness.rpc('WakeThreadWakeNow', ...args).then(
    () => null,
    (err: Error) => err.message,
  );
  expect(refusal, `Wake now ${args[1]} ${args[2]}`).toMatch(reason);
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: the tray lists each loop, watch and reminder, and Wake now and Cancel act on it`, async ({ harness, page }) => {
    test.setTimeout(240_000);
    const agent = await seedWakeAgent(harness, provider, 'wake-tray', 'Release agent');
    const answers = await registerThrough(harness, provider, agent, `wake-tray-${provider}`, [
      { tool: 'thread_loop', args: { label: 'standup', note: 'post the standup summary', every_seconds: 3600 } },
      { tool: 'thread_watch', args: { label: 'ci', command: run('ci.sh'), every_seconds: 3600 } },
      { tool: 'thread_watch', args: { label: 'held', command: run('hold.sh'), every_seconds: 3600 } },
      { tool: 'thread_watch', args: { label: 'events', command: 'exec sleep 3600', long_running: true } },
      { tool: 'thread_remind', args: { after_seconds: 3600, note: 'review the deploy notes' } },
      { tool: 'thread_remind', args: { after_seconds: 7200, note: 'renew the certificate' } },
    ]);
    const [reviewToken, renewToken] = [answers[4].token as string, answers[5].token as string];

    await harness.open(page);
    await openTray(page, 'Release agent');
    expect(await toggleCount(page)).toBe('6');
    const standup = trayRow(page, 'loop', 'standup');
    const ci = trayRow(page, 'watch', 'ci');
    const held = trayRow(page, 'watch', 'held');
    const events = trayRow(page, 'watch', 'events');
    const review = trayRow(page, 'reminder', 'review the deploy notes');
    const renew = trayRow(page, 'reminder', 'renew the certificate');
    for (const row of [standup, ci, held]) await expect(row.getByTestId('wake-tray-row-when')).toHaveText(/^next 59m$/);
    await expect(review.getByTestId('wake-tray-row-when')).toHaveText(/^next 59m$/);
    await expect(renew.getByTestId('wake-tray-row-when')).toHaveText(/^next 1h 59m$/);
    await expect(events.getByTestId('wake-tray-row-when')).toHaveText('running');
    await expect(events).toHaveAttribute('data-state', 'running');
    await expect(events.getByTestId('wake-tray-row-wake-now')).toBeDisabled();
    await expect(events.getByTestId('wake-tray-row-wake-now')).toHaveAttribute('title', /runs all the time/);
    await expect(standup.getByTestId('wake-tray-row-kind')).toHaveText('loop');
    await expect(review.getByTestId('wake-tray-row-kind')).toHaveText('remind');
    // Desktop keeps one line: the label, when it fires and the actions side by side.
    await expect(page.getByTestId('wake-tray-row-controls')).toHaveCount(0);
    const [labelBox, whenBox, cancelBox] = await Promise.all(
      ['wake-tray-row-label', 'wake-tray-row-when', 'wake-tray-row-cancel'].map(async (id) => (await review.getByTestId(id).boundingBox())!),
    );
    for (const box of [whenBox, cancelBox]) expect(Math.abs(box.y + box.height / 2 - (labelBox.y + labelBox.height / 2))).toBeLessThanOrEqual(2);
    await capture(page, `${provider}-tray-collapsed.png`, page.getByTestId('activity-rail-background-body').filter({ visible: true }));

    // The registering calls name their next fire in the timeline; the
    // long-running watch has none.
    const header = page.getByTestId('activity-run-header').filter({ visible: true });
    if ((await header.count()) > 0 && (await page.getByTestId('activity-run').first().getAttribute('data-collapsed')) === 'true') {
      await header.first().click();
    }
    // Triage stamped each answer's next fire and the row's own time; the
    // card says the time alone for a fire on the row's day and the date as
    // well for a later one, whenever it is read.
    const stamped = dbScheduleRows(agent.dataDir, agent.thread);
    expect(stamped.map((row) => row.tool).sort()).toEqual(['thread_loop', 'thread_remind', 'thread_remind', 'thread_watch', 'thread_watch']);
    for (const row of stamped) {
      expect(row.next_fire_from, `${row.tool} row time`).toBeGreaterThan(0);
      await expect(page.locator(`[data-item-id="${row.id}"]`).getByTestId('tool-call-card')).toContainText(`· ${await expectedNextFire(page, row)}`);
    }
    const scheduled = page.getByTestId('tool-call-card').filter({ hasText: '· next ' });
    await expect(scheduled).toHaveCount(5);
    await expect(page.getByTestId('tool-call-card').filter({ hasText: 'events' })).not.toContainText('· next');
    await scheduled.first().scrollIntoViewIfNeeded();
    await capture(page, `${provider}-timeline-next-fire.png`, scheduled.first());
    await openTray(page, 'Release agent');

    // ---- Wake now on a watch runs it through the sweep: its stdout wakes
    // the thread, its stderr shows when the row is expanded, and its
    // schedule stands.
    const ciNext = await ci.getAttribute('data-next-at');
    const ciWake = wakeInput(harness, agent.cwd, 'Watch "ci" reported.');
    await ci.getByTestId('wake-tray-row-wake-now').click();
    expect((await ciWake).report.input).toContain('"pipeline 12 failed: lint"');
    await awaitTurnCompleted(harness, agent.thread);
    await expect.poll(() => dbWake(agent.dataDir, agent.thread, 'ci')?.last_stderr).toBe('ci: checked pipeline 12 at the default branch\n');
    const ciRow = dbWake(agent.dataDir, agent.thread, 'ci')!;
    await expect(ci.getByTestId('wake-tray-row-outcome')).toHaveText(ciRow.last_outcome);
    await expect(ci).toHaveAttribute('data-next-at', ciNext!);
    expect(String(ciRow.next_at)).toBe(ciNext);
    await ci.getByTestId('wake-tray-row-toggle').click();
    await expect(ci.getByTestId('wake-tray-row-command')).toHaveText(run('ci.sh'));
    await expect(ci.getByTestId('wake-tray-row-cwd')).toHaveText(agent.cwd);
    await expect(ci.getByTestId('wake-tray-row-stderr')).toHaveText('ci: checked pipeline 12 at the default branch');
    await capture(page, `${provider}-tray-watch-expanded.png`, ci);
    await ci.getByTestId('wake-tray-row-toggle').click();
    await expect(ci.getByTestId('wake-tray-row-detail')).toHaveCount(0);
    // A loop has nothing to expand.
    await expect(standup.getByTestId('wake-tray-row-toggle')).toHaveAttribute('aria-disabled', 'true');

    // ---- Wake now on a loop fires it once and keeps its schedule.
    const standupNext = await standup.getAttribute('data-next-at');
    const standupWake = wakeInput(harness, agent.cwd, 'Loop "standup" fired.');
    await standup.getByTestId('wake-tray-row-wake-now').click();
    expect((await standupWake).report.input).toContain('post the standup summary');
    await awaitTurnCompleted(harness, agent.thread);
    await expect.poll(() => dbWake(agent.dataDir, agent.thread, 'standup')?.last_outcome).toBeTruthy();
    await expect(standup.getByTestId('wake-tray-row-outcome')).toHaveText(dbWake(agent.dataDir, agent.thread, 'standup')!.last_outcome);
    await expect(standup).toHaveAttribute('data-next-at', standupNext!);

    // ---- A run in flight says so and refuses another.
    const hold = await holdNextRun(agent.home);
    await held.getByTestId('wake-tray-row-wake-now').click();
    await hold.running();
    await expect(held.getByTestId('wake-tray-row-when')).toHaveText('running');
    await expect(held.getByTestId('wake-tray-row-wake-now')).toBeDisabled();
    await expect(held.getByTestId('wake-tray-row-wake-now')).toHaveAttribute('title', 'Already running');
    const heldID = dbWake(agent.dataDir, agent.thread, 'held')!.id;
    await expectRefused(harness, [agent.thread, 'watch', heldID], /the watch "held" is already running/);
    await expectRefused(harness, [agent.thread, 'watch', dbWake(agent.dataDir, agent.thread, 'events')!.id], /runs all the time/);
    await capture(page, `${provider}-tray-running.png`, held, events);
    await hold.release();
    await expect(held.getByTestId('wake-tray-row-when')).toHaveText(/^next 5\dm$/);
    await expect(held.getByTestId('wake-tray-row-wake-now')).toBeEnabled();

    // ---- Wake now on a reminder fires it, and it leaves the tray.
    const reviewWake = wakeInput(harness, agent.cwd, 'review the deploy notes');
    await review.getByTestId('wake-tray-row-wake-now').click();
    await reviewWake;
    await awaitTurnCompleted(harness, agent.thread);
    await expect(review).toHaveCount(0);
    expect(dbReminderState(agent.dataDir, reviewToken)).not.toBe('accepted');
    expect(await toggleCount(page)).toBe('5');

    // ---- Cancel removes each kind, as thread_cancel does.
    const standupID = dbWake(agent.dataDir, agent.thread, 'standup')!.id;
    for (const row of [standup, ci, events, renew]) {
      await row.getByTestId('wake-tray-row-cancel').click();
      await expect(row).toHaveCount(0);
    }
    expect(dbWake(agent.dataDir, agent.thread, 'standup')).toBeUndefined();
    expect(dbWake(agent.dataDir, agent.thread, 'ci')).toBeUndefined();
    expect(dbWake(agent.dataDir, agent.thread, 'events')).toBeUndefined();
    expect(dbReminderState(agent.dataDir, renewToken)).toBeUndefined();
    expect(await toggleCount(page)).toBe('1');
    await expectRefused(harness, [agent.thread, 'loop', standupID], /the loop "standup" was cancelled/);

    // ---- An archived thread is refused before anything fires.
    await harness.rpc('ArchiveThread', agent.thread);
    await expectRefused(harness, [agent.thread, 'watch', heldID], /archived; unarchive it first/);
  });

  test(`${provider}: rows follow the sweep live as a short loop fires`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const agent = await seedWakeAgent(harness, provider, 'wake-live', 'Heartbeat agent');
    await registerThrough(harness, provider, agent, `wake-live-${provider}`, [
      { tool: 'thread_loop', args: { label: 'beat', note: 'check in', every_seconds: 3 } },
    ]);
    await harness.open(page);
    await openTray(page, 'Heartbeat agent');
    const beat = trayRow(page, 'loop', 'beat');
    await expect(beat).toBeVisible();
    interface Change { threadId: string; id: string; row?: { label: string; nextAt?: number }; removed?: boolean }
    const changes = () => harness.countEvents<Change>('thread:wakes_changed', (d) => d.threadId === agent.thread);

    // Two fires, each moving the row's next fire and recording its outcome,
    // with nothing but the pushed changes to tell the page.
    let next = Number(await beat.getAttribute('data-next-at'));
    for (let fire = 0; fire < 2; fire++) {
      const before = changes();
      await expect.poll(async () => Number(await beat.getAttribute('data-next-at')), { timeout: 30_000 }).toBeGreaterThan(next);
      next = Number(await beat.getAttribute('data-next-at'));
      expect(changes()).toBeGreaterThan(before);
    }
    await expect(beat.getByTestId('wake-tray-row-outcome')).toHaveText(/\S/);
    // Each frame carries the row as it stands: the newest is the one shown.
    const beatID = dbWake(agent.dataDir, agent.thread, 'beat')!.id;
    await expect.poll(async () => {
      const shown = Number(await beat.getAttribute('data-next-at'));
      return harness.countEvents<Change>('thread:wakes_changed', (d) => d.id === beatID && d.row?.label === 'beat' && d.row.nextAt === shown);
    }).toBeGreaterThan(0);
    expect(dbWake(agent.dataDir, agent.thread, 'beat')!.last_outcome).toBeTruthy();
    await capture(page, `${provider}-tray-live.png`, beat);
    await beat.getByTestId('wake-tray-row-cancel').click();
    await expect(beat).toHaveCount(0);
    await expect.poll(() => harness.countEvents<Change>('thread:wakes_changed', (d) => d.id === beatID && d.removed === true && !d.row)).toBe(1);
    await expect(page.getByTestId('activity-rail-background-toggle').filter({ visible: true })).toHaveCount(0);
  });

  test(`${provider}: a wake its session ended before taking in turns its row to a warning live`, async ({ harness, page }) => {
    test.setTimeout(120_000);
    const agent = await seedWakeAgent(harness, provider, 'wake-lost', 'Doomed agent');
    // Codex: the wake starts turn 2, which parks without echoing it.
    await registerThrough(
      harness,
      provider,
      agent,
      `wake-lost-${provider}`,
      [{ tool: 'thread_loop', args: { label: 'standup', note: 'post the standup summary', every_seconds: 3600 } }],
      provider === 'codex' ? [{ steps: [{ gate: 'hold' }], text: 'unreached', echo: false }] : [],
    );
    // Claude's mock echoes whatever reaches a running session, so the wake
    // goes to a session still starting, which reads it and holds.
    if (provider === 'claude') {
      await harness.rpc('StopSession', agent.thread);
      await setScenario(harness, agent.cwd, {
        ...threadToolsScenario({ name: `wake-lost-slow-${provider}`, provider, turns: [{ text: 'unreached' }] }),
        startupDelayMs: 30_000,
      });
    }
    await harness.open(page);
    await openTray(page, 'Doomed agent');
    const standup = trayRow(page, 'loop', 'standup');
    const taken = wakeInput(harness, agent.cwd, 'Loop "standup" fired.');
    await standup.getByTestId('wake-tray-row-wake-now').click();
    const mockId = provider === 'claude' ? (await taken).mockId : (await awaitGate(harness, 'hold', agent.cwd)).mockId;
    // The fire is over before the session dies: its row says delivered, and
    // nothing else is due to write it. The one frame that can turn the row
    // is the session end's.
    const outcome = standup.getByTestId('wake-tray-row-outcome');
    await expect(outcome).toHaveText('delivered', { timeout: 30_000 });
    await expect(standup).toHaveAttribute('data-state', 'armed');
    const standupID = dbWake(agent.dataDir, agent.thread, 'standup')!.id;
    const framesFor = () => harness.countEvents<{ id: string }>('thread:wakes_changed', (d) => d.id === standupID);
    const settled = framesFor();
    await harness.rpc('HarnessMockCommand', mockId, { type: 'exit', code: 1 });
    await expect(outcome).toHaveText(UNDELIVERED, { timeout: 30_000 });
    expect(framesFor() - settled, 'frames for the row after the session ended').toBe(1);
    await expect(outcome).toHaveAttribute('data-tone', 'warning');
    expect(dbWake(agent.dataDir, agent.thread, 'standup')!.last_outcome).toBe(UNDELIVERED);
    await capture(page, `${provider}-tray-undelivered.png`, standup);
  });
}
