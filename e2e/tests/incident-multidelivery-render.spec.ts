// The 2026-08-29 pane-freeze incident, replayed against the real app.
//
// THE INCIDENT. A background agent that stops more than once delivers more
// than one task_notification for ONE launch. The grouping pass used to mint
// a fresh card per delivery under the same key, Svelte's keyed `{#each}`
// threw `each_key_duplicate`, and the throw aborted the whole update flush:
// the pane froze mid-reveal and the assistant text stayed truncated. Each
// stop is now a sibling row with an id of its own (a parked stop's
// `complete:<launch>:parked:<uuid>`, then the ending `complete:<launch>`),
// so each stop's card has its own key (docs/specs/agent-visibility.md,
// §Agent runs and stops), and a repair-and-report tripwire
// (`enforceUniqueTimelineNodeKeys`) makes a duplicate key impossible to
// throw from the timeline at all.
//
// WHAT ONLY THIS LEVEL PROVES. The unit and browser suites pin the grouping
// and the tripwire in isolation; `codex-collab.spec.ts` pins the store rows.
// This spec is the composite: the real wire, the real triage/store hop, and
// the real SPA revealing text WHILE the deliveries land, the exact flush
// the incident aborted. Three verdicts, each observable only here:
//   1. zero page errors (nothing threw inside a flush),
//   2. text emitted AFTER the deliveries fully reveals (the drain never froze),
//   3. one card per stop with no `[subagentGrouping] duplicate` repair
//      warning (the fix holds at the root; the tripwire stayed idle).
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  agentWakeLine,
  claudeScenario,
  emit,
  seedAgentThread,
  shellBackgroundAckLine,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

test.beforeEach(async ({ harness }) => {
  // These assertions inspect delivery cards inside completed activity runs.
  await harness.rpc('UpdateSettings', { activityRunDefault: 'expanded' });
});

const MID = 'Mid-stream note: the background scan is still running.';
const FINAL =
  'Final summary after all three stop notifications: the background agent ' +
  'reported twice mid-turn and once at completion, and every word of this ' +
  'sentence must still reveal; a frozen drain truncates it here.';

const AGENT = { task_id: 'task-bg', task_type: 'local_agent', description: 'scan the tree' };
const SHELLS = [1, 2].map((n) => ({
  task_id: `task-scan-${n}`,
  task_type: 'local_bash',
  description: `scan part ${n}`,
  toolUseId: `tu-scan-${n}`,
}));

// The agent starts a background scan it owns.
function ownedScanLines(shell: (typeof SHELLS)[number]): string[] {
  return [
    toolUseLine(`msg-${shell.toolUseId}`, shell.toolUseId, 'Bash', { command: shell.description, run_in_background: true }, 'tu-bg'),
    taskStartedLine(shell.task_id, shell.toolUseId, shell.description, { taskType: 'local_bash', ownedBySubagent: true }),
    shellBackgroundAckLine(shell.toolUseId, shell.task_id, 'tu-bg'),
  ];
}

// One stop of the agent: its task_updated, then its notification. A stop
// while one of its scans runs is a pause; the last one is the end.
function agentStopLines(boundToolUse: string, report: string, n: number): string[] {
  return [
    taskUpdatedLine('task-bg', { status: 'completed', end_time: 1787419835000 + n }),
    taskNotificationLine('task-bg', boundToolUse, report, {
      outputFile: '${CWD}/scan-output.jsonl',
      uuid: `notify-${n}`,
      usage: { total_tokens: 1200 * n, tool_uses: 3 * n, duration_ms: 900 * n },
    }),
  ];
}

// A scan finishes and the CLI wakes the agent.
function scanDoneLines(shell: (typeof SHELLS)[number]): string[] {
  const summary = `Background command "${shell.description}" completed (exit code 0)`;
  return [
    taskUpdatedLine(shell.task_id, { status: 'completed', end_time: 1787419845000 }),
    taskNotificationLine(shell.task_id, shell.toolUseId, summary, { uuid: `done-${shell.task_id}` }),
    agentWakeLine('task-bg', 'scan the tree', { taskId: shell.task_id, toolUseId: shell.toolUseId, status: 'completed', summary }),
  ];
}

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
  'three stops of one Claude background launch give three cards and never freeze the reveal',
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
          ...SHELLS.flatMap(ownedScanLines),
          backgroundTasksChangedLine([AGENT, ...SHELLS.map(({ task_id, task_type, description }) => ({ task_id, task_type, description }))]),
        ]),
        { waitSignal: { name: 'first-stop' } },
        // Stop 1 lands while the turn is still streaming, then more text:
        // the flush the incident aborted.
        emit([
          ...agentStopLines('tu-bg', 'First stop: scanned internal/.', 1),
          ...textLines('msg-mid', MID),
        ]),
        { waitSignal: { name: 'later-stops' } },
        // Each woken run's stop names no tool_use (claude-wire.md §E6b).
        emit([
          ...scanDoneLines(SHELLS[0]),
          ...agentStopLines('', 'Second stop: scanned frontend/.', 2),
          ...scanDoneLines(SHELLS[1]),
          ...agentStopLines('', 'Third stop: scan complete.', 3),
          ...textLines('msg-final', FINAL),
          backgroundTasksChangedLine([]),
          RESULT_LINE,
        ]),
      ]),
    });

    const threadId = await seedAgentThread(harness, 'incident-multidelivery-app', 'Multi delivery');
    await harness.open(page);
    const watch = watchRender(page);
    await page.getByText('Multi delivery').click();
    const mockId = await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'scan everything', null);

    const timeline = page.getByTestId('message-timeline-scroll');
    await waitForGate(harness, 'first-stop');
    // Before any stop there is no card yet: the launch row is the
    // immutable spawn record and a card renders at each stop (ruling
    // 2026-08-23). Stop 1 below mints the first.
    await expect(timeline.getByText('Launching the background scanner.')).toBeVisible({
      timeout: 20_000,
    });
    const cards = timeline.getByTestId('subagent-group');
    await expect(cards).toHaveCount(0);

    // Stop 1 + mid text: the agent paused on its scans, so the stop is a
    // parked card, and the pane kept revealing. Before the fix, a second
    // delivery minted a duplicate-keyed card and the throw froze the
    // reveal.
    await advance(harness, mockId, 'first-stop');
    await expect(cards).toHaveCount(1, { timeout: 20_000 });
    await expect(cards.nth(0)).toHaveAttribute('data-status', 'parked');
    await expect(cards.nth(0).getByTestId('subagent-group-preview')).toContainText('First stop: scanned internal/.');
    await expect(timeline.getByText(MID)).toBeVisible({ timeout: 20_000 });

    // Stops 2 and 3 + the final text whose truncation WAS the incident.
    await waitForGate(harness, 'later-stops');
    await advance(harness, mockId, 'later-stops');
    await harness.waitForEvent('provider:turn_completed');
    await expect(timeline.getByText(FINAL)).toBeVisible({ timeout: 30_000 });
    await expect(cards).toHaveCount(3);
    expect(await cards.evaluateAll((nodes) => nodes.map((node) => node.getAttribute('data-status')))).toEqual([
      'parked',
      'parked',
      'completed',
    ]);
    await expect(cards.nth(1).getByTestId('subagent-group-preview')).toContainText('Second stop: scanned frontend/.');
    await expect(cards.nth(2).getByTestId('subagent-group-preview')).toContainText('Third stop: scan complete.');

    expect(watch.pageErrors).toEqual([]);
    expect(watch.duplicateKeyWarnings).toEqual([]);
  },
);

test(
  'two Codex FINAL_ANSWERs under one spawn render one card each without a render throw',
  async ({ harness, page }) => {
    await harness.rpc('HarnessSetScenario', { name: 'codex-collab-two-deliveries' });
    const threadId = await seedAgentThread(
      harness,
      'incident-codex-two-deliveries',
      'Codex deliveries',
      'codex',
    );
    await harness.open(page);
    const watch = watchRender(page);
    await page.getByText('Codex deliveries').click();
    await startMock(harness, threadId);
    await harness.rpc('SendMessage', threadId, 'review this', null);
    await harness.waitForEvent('provider:turn_completed');

    const timeline = page.getByTestId('message-timeline-scroll');
    const cards = timeline.getByTestId('subagent-group');
    // Each delivery owns its card under its own completion key
    // (docs/specs/agent-visibility.md); the previews read from their own rows.
    await expect(cards).toHaveCount(2, { timeout: 20_000 });
    await expect(cards.nth(0).getByTestId('subagent-group-preview')).toContainText('First review pass.');
    await expect(cards.nth(1).getByTestId('subagent-group-preview')).toContainText('Second review pass.');

    expect(watch.pageErrors).toEqual([]);
    expect(watch.duplicateKeyWarnings).toEqual([]);
  },
);
