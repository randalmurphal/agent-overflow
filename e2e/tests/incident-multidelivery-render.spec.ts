// The 2026-08-29 pane-freeze incident, replayed against the real app.
//
// THE INCIDENT. A background agent that stops more than once delivers more
// than one task_notification for ONE launch. The grouping pass used to mint
// a fresh card per delivery under the same key, Svelte's keyed `{#each}`
// threw `each_key_duplicate`, and the throw aborted the whole update flush:
// the pane froze mid-reveal and the assistant text stayed truncated. The
// user's error log carried 400+ of these, timestamps matching the observed
// truncations. The fix is structural — the first delivery anchors the card,
// later deliveries become leaves under it (`subagentGrouping.ts`), and a
// repair-and-report tripwire (`enforceUniqueTimelineNodeKeys`) makes a
// duplicate key impossible to throw from the timeline at all.
//
// WHAT ONLY THIS LEVEL PROVES. The unit and browser suites pin the grouping
// and the tripwire in isolation; `codex-collab.spec.ts` pins the store rows.
// This spec is the composite: the real wire, the real triage/store hop, and
// the real SPA revealing text WHILE the deliveries land — the exact flush
// the incident aborted. Three verdicts, each observable only here:
//   1. zero page errors (nothing threw inside a flush),
//   2. text emitted AFTER the deliveries fully reveals (the drain never froze),
//   3. one card per launch with no `[subagentGrouping] duplicate` repair
//      warning (the fix holds at the root; the tripwire stayed idle).
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const MID = 'Mid-stream note: the background scan is still running.';
const FINAL =
  'Final summary after all three stop notifications: the background agent ' +
  'reported twice mid-turn and once at completion, and every word of this ' +
  'sentence must still reveal — a frozen drain truncates it here.';

interface RenderWatch {
  pageErrors: string[];
  duplicateKeyWarnings: string[];
}

/** Arm the two listeners the incident verdicts read. Call before streaming. */
function watchRender(page: Page): RenderWatch {
  const watch: RenderWatch = { pageErrors: [], duplicateKeyWarnings: [] };
  page.on('pageerror', (err) => watch.pageErrors.push(String(err)));
  page.on('console', (msg) => {
    if (msg.text().includes('duplicate timeline node keys')) {
      watch.duplicateKeyWarnings.push(msg.text());
    }
  });
  return watch;
}

test(
  'three deliveries from one Claude background launch keep one card and never freeze the reveal',
  async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', {
      scenario: claudeScenario('incident-multidelivery', [
        emit([
          ...textLines('msg-lead', 'Launching the background scanner.'),
          toolUseLine('msg-agent', 'tu-bg', 'Agent', {
            description: 'scan the tree',
            subagent_type: 'general-purpose',
          }),
          taskStartedLine('task-bg', 'tu-bg', 'scan the tree'),
          asyncAgentAckLine('tu-bg', 'task-bg', 'scan the tree'),
          backgroundTasksChangedLine([
            { task_id: 'task-bg', task_type: 'local_agent', description: 'scan the tree' },
          ]),
        ]),
        { waitSignal: { name: 'first-stop' } },
        // Delivery 1 lands while the turn is still streaming, then more text —
        // the flush the incident aborted.
        emit([
          taskNotificationLine('task-bg', 'tu-bg', 'First stop: scanned internal/.', {
            uuid: 'notify-1',
            usage: { total_tokens: 1200, tool_uses: 3, duration_ms: 900 },
          }),
          ...textLines('msg-mid', MID),
        ]),
        { waitSignal: { name: 'later-stops' } },
        emit([
          taskNotificationLine('task-bg', 'tu-bg', 'Second stop: scanned frontend/.', {
            uuid: 'notify-2',
            usage: { total_tokens: 2400, tool_uses: 6, duration_ms: 1800 },
          }),
          taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835999 }),
          taskNotificationLine('task-bg', 'tu-bg', 'Third stop: scan complete.', {
            uuid: 'notify-3',
            usage: { total_tokens: 3100, tool_uses: 8, duration_ms: 2600 },
          }),
          ...textLines('msg-final', FINAL),
          backgroundTasksChangedLine([]),
          RESULT_LINE,
        ]),
      ]),
    });

    const threadId = await seedAgentThread(harness, 'incident-multidelivery-app', 'Multi delivery');
    await page.goto(harness.url);
    const watch = watchRender(page);
    await page.getByText('Multi delivery').click();
    const mockId = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'scan everything', null);

    const timeline = page.getByTestId('message-timeline-scroll');
    await waitForGate(harness, 'first-stop');
    // Before any delivery there is no card yet: the launch row is the
    // immutable spawn record and the CARD appears at completion (ruling
    // 2026-08-23) — delivery 1 below is what mints it.
    await expect(timeline.getByText('Launching the background scanner.')).toBeVisible({
      timeout: 20_000,
    });
    await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);

    // Delivery 1 + mid text: the delivery is a LEAF under the launch row
    // (still no card — the task has not completed), and the pane kept
    // revealing. Pre-fix, this delivery minted a duplicate-keyed card and
    // the throw froze the reveal right here.
    await advance(harness, mockId, 'first-stop');
    await expect(timeline.getByText('First stop: scanned internal/.')).toBeVisible({
      timeout: 20_000,
    });
    await expect(timeline.getByText(MID)).toBeVisible({ timeout: 20_000 });
    await expect(timeline.getByTestId('subagent-group')).toHaveCount(0);

    // Deliveries 2 and 3 + the final text whose truncation WAS the incident.
    await waitForGate(harness, 'later-stops');
    await advance(harness, mockId, 'later-stops');
    await harness.waitForEvent('provider:turn_completed');
    await expect(timeline.getByText(FINAL)).toBeVisible({ timeout: 30_000 });
    await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);

    expect(watch.pageErrors).toEqual([]);
    expect(watch.duplicateKeyWarnings).toEqual([]);
  },
);

test(
  'two Codex FINAL_ANSWERs under one spawn render one card without a render throw',
  async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { name: 'codex-collab-two-deliveries' });
    const threadId = await seedAgentThread(
      harness,
      'incident-codex-two-deliveries',
      'Codex deliveries',
      'codex',
    );
    await page.goto(harness.url);
    const watch = watchRender(page);
    await page.getByText('Codex deliveries').click();
    await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'review this', null);
    await harness.waitForEvent('provider:turn_completed');

    const timeline = page.getByTestId('message-timeline-scroll');
    const card = timeline.getByTestId('subagent-group');
    await expect(card).toHaveCount(1, { timeout: 20_000 });
    // Both answers live under the one card; the preview reads from the latest.
    await expect(card.getByTestId('subagent-group-preview')).toContainText('Second review pass.');

    expect(watch.pageErrors).toEqual([]);
    expect(watch.duplicateKeyWarnings).toEqual([]);
  },
);
