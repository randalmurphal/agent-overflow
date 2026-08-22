// Agent visibility — the PANE criterion (docs/specs/agent-visibility.md
// § "Success criteria", item 3):
//
//   Opening any card shows that node's full transcript in the agent pane;
//   opening a child from inside swaps scope with a working breadcrumb;
//   reload restores the pane to the same scope.
//
// The card body is deliberately a DIGEST (tool calls, final text, child
// cards — Q2), so "full transcript" is only provable by contrast: the same
// thinking block and intermediate text that the card omits must be present
// in the pane.
//
// Both tests replay the same FOREGROUND (awaited) agent shape: an awaited
// agent's terminal `system/task_updated` lands BEFORE its real
// `tool_result` (claude-wire.md §task_started). Leave the terminal out and
// the parser's live-agent-task fallback reads the bare result as an async
// ack and backgrounds the launch — which is the wire's rule, not a bug, but
// it would make this a background scenario by accident.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  thinkingLines,
  toolResultLine,
  toolUseLine,
} from './agent-visibility-helpers.js';

const THINKING = 'Deciding which files matter first.';
const INTERMEDIATE = 'Intermediate note: starting with the parser.';
const SURVEY_FINAL = 'Survey complete: no drift.';
const PROBE_NOTE = 'Probe note: the grammar looks stable.';

/** localStorage key the appStorage bucket cache lives under. */
const CLIENT_ID_CACHE_KEY = 'agent-overflow:uistate:clientId';

/**
 * One awaited agent (`tu-survey`) that thinks, narrates, reads a file, and
 * launches one awaited child agent (`tu-probe`) of its own.
 */
function surveyScenario() {
  return claudeScenario('pane-survey', [
    emit([
      ...textLines('msg-lead', 'Delegating the survey.'),
      toolUseLine('msg-survey', 'tu-survey', 'Agent', {
        description: 'survey the parser',
        subagent_type: 'surveyor',
      }),
      taskStartedLine('task-survey', 'tu-survey', 'survey the parser'),

      // Everything the surveyor produced. Thinking and the intermediate
      // text are the two kinds the card body filters out.
      ...thinkingLines('msg-survey-think', THINKING, 'tu-survey'),
      ...textLines('msg-survey-mid', INTERMEDIATE, 'tu-survey'),
      toolUseLine('msg-survey-read', 'tu-survey-read', 'Read', {
        file_path: 'internal/provider/claude/parser.go',
      }, 'tu-survey'),
      toolResultLine('tu-survey-read', 'package claude', { parentToolUseId: 'tu-survey' }),

      // A child agent, so the pane has something to descend into.
      toolUseLine('msg-probe', 'tu-probe', 'Agent', {
        description: 'probe the grammar',
        subagent_type: 'prober',
      }, 'tu-survey'),
      taskStartedLine('task-probe', 'tu-probe', 'probe the grammar'),
      ...textLines('msg-probe-text', PROBE_NOTE, 'tu-probe'),
      toolUseLine('msg-probe-grep', 'tu-probe-grep', 'Grep', { pattern: 'grammar' }, 'tu-probe'),
      toolResultLine('tu-probe-grep', '3 matches', { parentToolUseId: 'tu-probe' }),
      taskUpdatedLine('task-probe', { status: 'completed', end_time: 1787415964724 }),
      toolResultLine('tu-probe', 'Probe done.', { parentToolUseId: 'tu-survey' }),

      ...textLines('msg-survey-final', SURVEY_FINAL, 'tu-survey'),
      taskUpdatedLine('task-survey', { status: 'completed', end_time: 1787415964725 }),
      toolResultLine('tu-survey', SURVEY_FINAL),
      RESULT_LINE,
    ]),
  ]);
}

/** Seeds, boots the UI, runs the survey turn, returns the thread id. */
async function runSurveyTurn(
  harness: Parameters<typeof seedAgentThread>[0],
  page: import('@playwright/test').Page,
  projectName: string,
  title: string,
): Promise<string> {
  await harness.rpc('HarnessSetScenario', { scenario: surveyScenario() });
  const threadId = await seedAgentThread(harness, projectName, title);
  await page.goto(harness.url);
  await page.getByText(title).click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'survey the parser', null);
  await harness.waitForEvent('provider:turn_completed');
  return threadId;
}

test('an agent card opens as a scoped, read-only thread view that survives reload', async ({
  harness,
  page,
}) => {
  await runSurveyTurn(harness, page, 'pane-app', 'Agent pane');

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-label')).toContainText('Surveyor');

  // --- The card body is a digest, not the transcript ----------------
  await card.getByTestId('subagent-group-toggle').first().click();
  const cardBody = card.getByTestId('subagent-group-body').first();
  await expect(cardBody.getByText(SURVEY_FINAL)).toBeVisible();
  await expect(cardBody.getByTestId('thinking-toggle')).toHaveCount(0);
  await expect(cardBody.getByText(INTERMEDIATE)).toHaveCount(0);

  // --- Opening it shows the FULL transcript -------------------------
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane).toBeVisible();
  await expect(pane.getByTestId('agent-pane-kind')).toHaveText('agent');
  await expect(pane.getByTestId('agent-pane-breadcrumb-entry')).toHaveText(['main']);
  await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toHaveText('Surveyor');

  const paneTimeline = pane.getByTestId('agent-pane-timeline');
  await expect(paneTimeline.getByTestId('thinking-toggle')).toHaveCount(1);
  await expect(paneTimeline.getByText(INTERMEDIATE)).toBeVisible();
  await expect(paneTimeline.getByText(SURVEY_FINAL)).toBeVisible();

  // Read-only by construction (Q20): a composer-shaped shell that is not a
  // composer, and no Stop for a settled agent (the one live control exists
  // only while the task is running).
  await expect(pane.getByTestId('agent-pane-composer-shell')).toContainText(
    'Read-only agent transcript.',
  );
  await expect(pane.getByTestId('agent-pane-composer-shell').getByRole('textbox')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-stop')).toHaveCount(0);

  // --- Reload restores the pane at the scope it was left on ---------
  // The scope rides the pane-layout snapshot, which flushes to the
  // backend's ui_state through a debounce. Wait for the durable copy
  // rather than the local cache: hydration lets the SERVER value win for
  // any key with no pending local write, so reloading before the flush
  // would restore whatever the server still had.
  const clientId = await page.evaluate((key) => localStorage.getItem(key), CLIENT_ID_CACHE_KEY);
  expect(clientId).toBeTruthy();
  await expect
    .poll(async () => {
      const state = await harness.rpc<Record<string, string>>('GetUIState', clientId);
      return state?.paneLayout ?? '';
    })
    .toContain('tu-survey');

  await page.reload();
  const restored = page.getByTestId('companion-pane-agent-body');
  await expect(restored).toBeVisible();
  await expect(restored.getByTestId('agent-pane-breadcrumb-current')).toHaveText('Surveyor');
  await expect(restored.getByTestId('agent-pane-timeline').getByText(INTERMEDIATE)).toBeVisible();
});

// Regression pin — descending from inside the pane, then popping back.
// This spec started as a `test.fail()` bug document and forced three
// product fixes, each of which it still guards:
//
//   1. The pane's grouping input includes the SCOPE ROW (and its
//      completion sibling). Without it, every direct child's parentId
//      was absent from the grouping's id set, so children ranked as
//      `orphanIds`: orphan warning banners, no card for a nested launch
//      (nothing to descend into), grandchildren dropped outright.
//   2. The scope row goes in with its parentId CLEARED. A descended-into
//      scope points at a parent outside the pane's input, which
//      orphan-leafed the scope itself — no group node to unwrap, empty
//      pane at exactly the scope the breadcrumb named.
//   3. Retention + hydration honor the pane. Settled child rows fold out
//      of pane memory unless their card is expanded; the eviction guard
//      now also spares anchors on the open pane's scope trail
//      (`agentPaneScopeTrailHolds`), and the pane's hydrate gate counts
//      the fold's evicted rows instead of trusting "some rows loaded" —
//      a nested launch anchor survives eviction, so a non-empty subtree
//      proves nothing.
//
// `AgentPane.test.ts` pins 1 and 2 at unit level but misses 3: it feeds
// fully loaded items, and eviction only bites through the real
// subagent-memory pipeline — which is why this spec must click through
// descend AND pop on the live app.
test(
  'descending into a child from inside the pane swaps scope and grows the breadcrumb',
  async ({ harness, page }) => {
    await runSurveyTurn(harness, page, 'pane-descend-app', 'Agent pane descend');

    const timeline = page.getByTestId('message-timeline-scroll');
    await timeline
      .getByTestId('subagent-group')
      .first()
      .getByTestId('subagent-group-open-pane')
      .first()
      .click();

    const pane = page.getByTestId('companion-pane-agent-body');
    const paneTimeline = pane.getByTestId('agent-pane-timeline');
    await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toHaveText('Surveyor');

    // Consequence 1: rows parented at the scope are not orphans.
    await expect(pane.getByLabel('Orphan Subagent Item')).toHaveCount(0);

    // Consequence 2: the nested launch is a card with a descend control.
    const childCard = paneTimeline.getByTestId('subagent-group').first();
    await expect(childCard.getByTestId('subagent-group-label')).toContainText('Prober');
    await childCard.getByTestId('subagent-group-open-pane').first().click();

    await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toHaveText('Prober');
    await expect(pane.getByTestId('agent-pane-breadcrumb-entry')).toHaveText(['main', 'Surveyor']);

    // Consequence 3: the grandchild rows exist at all.
    await expect(paneTimeline.getByText(PROBE_NOTE)).toBeVisible();
    // Scope really SWAPPED — the surveyor's own rows are gone, not stacked.
    await expect(paneTimeline.getByText(INTERMEDIATE)).toHaveCount(0);

    // The breadcrumb pops back out.
    await pane.getByTestId('agent-pane-breadcrumb-entry').nth(1).click();
    await expect(pane.getByTestId('agent-pane-breadcrumb-current')).toHaveText('Surveyor');
    await expect(paneTimeline.getByText(INTERMEDIATE)).toBeVisible();
  },
);
