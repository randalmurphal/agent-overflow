// A dead session's frames that are still queued when a replacement session
// starts never reach the replacement (docs/specs/standing-agents.md, slice
// 4a). The thread's provider event worker is held (HarnessStallProviderEvents)
// while the session's last turn writes its frames, so they queue behind the
// stall. A reconnect stops the session, whose drain gives up at its stall
// bound (shortened by the `drain-stall` timing entry), and starts the
// replacement. The worker is released only once the replacement's process
// has registered, which is after the start committed the thread to it, so
// every queued frame of the dead session is handled after the replacement
// owns the thread. Each frame carries its session's token and triage refuses
// it: the replacement holds no schedule and no stale row.
//
// On Claude the queued frames arm a ScheduleWakeup timer and a one-shot
// CronCreate job; on Codex, which has no session schedule, they are the
// turn's reply and its completion. Both carry a reply that must never render.

import { HARNESS_TIMING, launchHarness, type HarnessApp } from '../src/harness.js';
import { expect, test } from './fixtures.js';
import { listItems, seedAgentThread, startMock, textLines, toolResultLine, toolUseLine } from './agent-visibility-helpers.js';
import { threadToolsScenario, type ProviderName } from './thread-tools-helpers.js';

const DRAIN_STALL = '500ms';
const STALE_REPLY = 'Reply from the stopped session.';
const REPLACEMENT_REPLY = 'Reply from the replacement.';

/** The queued frames' ScheduleWakeup and one-shot CronCreate acks, shaped like the captured ones (claude-wire.md §E11). */
function scheduleAckLines(): string[] {
  return [
    toolUseLine('msg-stale-wake', 'toolu_stale_wake', 'ScheduleWakeup', { delaySeconds: 1800, prompt: 'Check the deploy again.', reason: 'deploy check' }),
    toolResultLine('toolu_stale_wake', 'Next wakeup scheduled in 1800s.', {
      toolUseResult: { clampedDelaySeconds: 1800, scheduledFor: Date.now() + 30 * 60_000, wasClamped: false },
    }),
    toolUseLine('msg-stale-cron', 'toolu_stale_cron', 'CronCreate', { cron: '0 9 * * 1', prompt: 'Post the weekly summary.', recurring: false }),
    toolResultLine('toolu_stale_cron', 'Scheduled one-shot task 0e5a1c99 (0 9 * * 1). Session-only (not written to disk, dies when Claude exits). It will fire once then auto-delete.', {
      toolUseResult: { id: '0e5a1c99', humanSchedule: '0 9 * * 1', recurring: false, durable: false },
    }),
    ...textLines('msg-stale-armed', 'Armed the schedule.'),
  ];
}

function updateWorkReason(harness: HarnessApp): Promise<string> {
  return harness.rpc<string>('HarnessUpdateWorkReason');
}

function pendingEvents(harness: HarnessApp, threadId: string): Promise<number> {
  return harness.rpc<number>('HarnessPendingProviderEvents', threadId);
}

for (const provider of ['claude', 'codex'] as const satisfies readonly ProviderName[]) {
  test(`${provider}: a stopped session's queued frames never reach its replacement`, async ({ page }) => {
    test.setTimeout(90_000);
    const harness = await launchHarness({ env: { AO_HARNESS_TIMING: `${HARNESS_TIMING},drain-stall=${DRAIN_STALL}` } });
    try {
      await harness.rpc('HarnessSetScenario', {
        scenario: threadToolsScenario({
          name: `stale-frames-${provider}`,
          provider,
          echoUserMessage: true,
          turns: [
            { text: 'First answer.' },
            {
              steps: [{ gate: 'queue' }, ...(provider === 'claude' ? [{ emitLines: scheduleAckLines() }] : [])],
              text: STALE_REPLY,
            },
          ],
          afterTurns: 'silent',
        }),
      });
      const title = `Stale frames ${provider}`;
      const threadId = await seedAgentThread(harness, `stale-frames-${provider}`, title, provider);
      await harness.open(page);
      await page.getByTestId('thread-row-title').getByText(title, { exact: true }).click();
      const first = await startMock(harness, threadId);
      await harness.rpc('SendMessage', threadId, 'Answer once.', null);
      await expect(page.getByText('First answer.', { exact: true })).toBeVisible();
      await expect.poll(() => updateWorkReason(harness)).toBe('');

      // The second turn parks before writing anything the test is about.
      await harness.rpc('SendMessage', threadId, 'Arm the schedule.', null);
      await harness.waitForEvent('harness:mock', (ev: any) => ev.mockId === first && ev.report.kind === 'waiting_signal' && ev.report.detail === 'queue');

      await harness.rpc('HarnessStallProviderEvents', threadId);
      const stalled = await pendingEvents(harness, threadId);
      await harness.rpc('HarnessMockCommand', first, { type: 'advance', name: 'queue' });
      // Every frame of the turn queues behind the stall: the turn's third and
      // last step writes the reply and the turn's end, and the read loop
      // queues each line it reads. The reply's frames alone are several.
      await harness.waitForEvent('harness:mock', (ev: any) =>
        ev.mockId === first && ev.report.kind === 'step_completed' && ev.report.turn === 2 && ev.report.step === 3);
      await expect.poll(() => pendingEvents(harness, threadId)).toBeGreaterThan(stalled + 2);

      // The process the reconnect spawns answers with its own text.
      await harness.rpc('HarnessSetScenario', {
        scenario: threadToolsScenario({
          name: `stale-frames-${provider}-replacement`,
          provider,
          echoUserMessage: true,
          turns: [{ text: REPLACEMENT_REPLY }],
          afterTurns: 'silent',
        }),
      });
      const reconnect = harness.rpc('ReconnectSession', threadId);
      const replacement = (await harness.waitForEvent<{ mockId: string }>('harness:mock', (ev: any) => ev.report.kind === 'registered', 30_000)).mockId;
      expect(replacement).not.toBe(first);
      await harness.rpc('HarnessReleaseProviderEvents', threadId);
      await reconnect;
      await expect.poll(() => pendingEvents(harness, threadId)).toBe(0);

      // Soft, so a failure records everything that leaked.
      expect.soft(await updateWorkReason(harness), 'the replacement holds the stopped session\'s schedule or turn').toBe('');
      const items = await listItems(harness, threadId);
      expect.soft(items.filter((item) => item.summary.includes(STALE_REPLY) || item.summary.includes('Armed the schedule.')), 'a stopped session\'s reply reached the replacement').toEqual([]);
      expect.soft(items.filter((item) => item.toolName === 'ScheduleWakeup' || item.toolName === 'CronCreate'), 'a stopped session\'s schedule call reached the replacement').toEqual([]);
      // The replacement serves the thread, and the stale reply never shows.
      await harness.rpc('SendMessage', threadId, 'Answer again.', null);
      await expect(page.getByText(REPLACEMENT_REPLY, { exact: true })).toBeVisible();
      await expect(page.getByText('First answer.', { exact: true })).toBeVisible();
      await expect(page.getByText(STALE_REPLY)).toHaveCount(0);
      await expect.poll(() => updateWorkReason(harness)).toBe('');
    } finally {
      await page.close();
      await harness.close();
    }
  });
}
