// Agent visibility — the two CONTROL criteria
// (docs/specs/agent-visibility.md § "Success criteria", items 4 and 5):
//
//   4. A subagent's `permission_denied` and `can_use_tool` rows nest under
//      its card; the card shows the approval pill while the prompt is
//      pending.
//   5. The background button on a running inline agent returns the main
//      turn, the card flips to background, the pane shows the paused
//      marker, and the transcript completes on the task notification.
//
// Both scenarios replay a checked-in 2026-08-22 capture line for line:
// `can_use_tool_agent_id_20260822.ndjson` for the first (async launch →
// subagent tool_use → `can_use_tool` carrying `agent_id`) and
// `background_tasks_control_20260822.ndjson` for the second (the control
// request, then `background_tasks_changed` + `task_updated
// {patch:{is_backgrounded:true}}` + the async ack + `result`).
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  asyncAgentAckLine,
  backgroundTasksChangedLine,
  claudeScenario,
  emit,
  listItems,
  permissionDeniedLine,
  seedAgentThread,
  sidechainTranscript,
  startMock,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const WRITE_PATH = 'spike3.txt';
const DENY_REASON = 'Bash(rm:*) is denied by a project rule';
const BACKFILL_TEXT = 'Backfilled: the corpus sweep found two drifts.';

test('a subagent’s approval and denial rows nest under its card, and the card asks for approval', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('subagent-approval', [
      emit([
        ...textLines('msg-lead', 'Launching the writer agent.'),
        toolUseLine('msg-agent', 'tu-agent', 'Agent', {
          description: 'spike3',
          subagent_type: 'writer',
        }),
        backgroundTasksChangedLine([
          { task_id: 'task-writer', task_type: 'local_agent', description: 'spike3' },
        ]),
        taskStartedLine('task-writer', 'tu-agent', 'spike3'),
        asyncAgentAckLine('tu-agent', 'task-writer', 'spike3'),

        // The subagent's own tool_use, on the parent stream with
        // parent_tool_use_id — exactly as the capture carries it.
        toolUseLine('msg-write', 'tu-write', 'Write', {
          file_path: WRITE_PATH,
          content: 'hello',
        }, 'tu-agent'),
      ]),
      // `agent_id` is the whole point: it is the ONLY thing on the
      // control_request that says whose prompt this is.
      {
        approval: {
          toolName: 'Write',
          input: { file_path: WRITE_PATH, content: 'hello' },
          toolUseId: 'tu-write',
          agentId: 'task-writer',
          onAllow: [
            emit([
              toolResultLine('tu-write', 'File created successfully.', {
                parentToolUseId: 'tu-agent',
              }),

              // A PRE-ASK refusal for the subagent's next tool: no
              // approval card ever appears for it, only this notice.
              toolUseLine('msg-bash', 'tu-bash', 'Bash', {
                command: 'rm -rf ./scratch',
              }, 'tu-agent'),
              permissionDeniedLine('Bash', 'tu-bash', 'task-writer', DENY_REASON),
              toolResultLine('tu-bash', 'Permission to use Bash has been denied.', {
                parentToolUseId: 'tu-agent',
                isError: true,
              }),

              taskUpdatedLine('task-writer', { status: 'completed', end_time: 1787419835322 }),
              taskNotificationLine('task-writer', 'tu-agent', 'WROTE'),
              ...textLines('msg-final', 'The writer agent finished.'),
              RESULT_LINE,
            ]),
          ],
          onDeny: [emit([RESULT_LINE])],
        },
      },
    ]),
  });

  const threadId = await seedAgentThread(harness, 'approval-app', 'Subagent approval');
  await page.goto(harness.url);
  await page.getByText('Subagent approval').click();
  await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'write the spike file', null);

  // --- Pending: the prompt is the composer's, the pill is the card's --
  await harness.waitForEvent(
    'harness:mock',
    (ev: any) => ev.report.kind === 'approval_pending',
  );
  await expect(page.getByTestId('composer-pending-approval')).toBeVisible();

  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-label')).toContainText('Writer');
  await expect(card.getByTestId('subagent-group-approval-pill')).toBeVisible();

  // --- Resolved: the pill goes with the prompt ----------------------
  await page.getByTestId('approval-allow').click();
  await harness.waitForEvent(
    'harness:mock',
    (ev: any) => ev.report.kind === 'approval_decided' && ev.report.detail === 'allow',
  );
  await expect(card.getByTestId('subagent-group-approval-pill')).toHaveCount(0);
  await harness.waitForEvent('provider:turn_completed');

  // --- Attribution, at the row level --------------------------------
  // `system/permission_denied` is a TOP-LEVEL envelope carrying no
  // parent_tool_use_id, so the notice inherits the denied tool's scope.
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      const notice = items.find((i) => i.id === 'permission-denied:tu-bash');
      if (!notice) return null;
      return {
        noticeParent: notice.parentId ?? '',
        noticeKind: notice.kind,
        writeParent: items.find((i) => i.id === 'tu-write')?.parentId ?? '',
        bashParent: items.find((i) => i.id === 'tu-bash')?.parentId ?? '',
        topLevelToolCalls: items
          .filter((i) => i.kind === 'tool_call' && !i.parentId)
          .map((i) => i.toolName)
          .sort(),
      };
    })
    .toEqual({
      noticeParent: 'tu-agent',
      noticeKind: 'notification',
      writeParent: 'tu-agent',
      bashParent: 'tu-agent',
      topLevelToolCalls: ['Agent'],
    });

  // The main timeline shows one card and no denial notice of its own.
  await expect(timeline.getByTestId('subagent-group')).toHaveCount(1);

  // --- ...and both rows render INSIDE the card ----------------------
  await card.getByTestId('subagent-group-toggle').first().click();
  const body = card.getByTestId('subagent-group-body').first();
  await expect(body.getByText(WRITE_PATH).first()).toBeVisible();
  await expect(body.getByTestId('notification-row')).toHaveCount(1);
  await expect(body.getByTestId('permission-denied-reason')).toContainText(DENY_REASON);
  // The denied tool row itself is marked declined, not merely errored.
  await expect(body.getByTestId('tool-decision-chip')).toHaveCount(1);
});

test('backgrounding a running inline agent returns the turn and the transcript completes on the notification', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', {
    scenario: claudeScenario('background-button', [
      emit([
        ...textLines('msg-lead', 'Starting the corpus sweep.'),
        toolUseLine('msg-agent', 'tu-agent', 'Agent', {
          description: 'sweep the corpus',
          subagent_type: 'sweeper',
        }),
        taskStartedLine('task-sweep', 'tu-agent', 'sweep the corpus'),
        // One streamed sidechain row BEFORE the cut, so "streaming
        // stopped here" is a claim about a transcript that had content.
        ...textLines('msg-child', 'Reading the first shard.', 'tu-agent'),
      ]),
      // Held while the agent runs in the FOREGROUND: this is the only
      // window in which the background button exists.
      { waitSignal: { name: 'cut' } },
      emit([
        // The CLI's answer to the `background_tasks` control_request, in
        // capture order. The control_response itself is the mock's
        // (writeClaudeControlAck answers `{backgrounded:true}`).
        backgroundTasksChangedLine([
          { task_id: 'task-sweep', task_type: 'local_agent', description: 'sweep the corpus' },
        ]),
        taskUpdatedLine('task-sweep', { is_backgrounded: true }),
        asyncAgentAckLine('tu-agent', 'task-sweep', 'sweep the corpus'),
        RESULT_LINE,
      ]),
      { waitSignal: { name: 'finish' } },
      // The sidechain the agent produced while it streamed nothing. This
      // file IS the transcript for a mid-flight backgrounded agent.
      {
        writeFile: {
          path: 'sweep-output.jsonl',
          content: sidechainTranscript([
            { text: BACKFILL_TEXT },
            { tool: { id: 'tu-backfill-read', name: 'Read', result: '# fixture' } },
          ]),
        },
      },
      emit([
        taskUpdatedLine('task-sweep', { status: 'completed', end_time: 1787419835322 }),
        taskNotificationLine('task-sweep', 'tu-agent', 'Sweep complete.', {
          outputFile: '${CWD}/sweep-output.jsonl',
          usage: { total_tokens: 24110, tool_uses: 2, duration_ms: 9312 },
        }),
      ]),
    ]),
  });

  const threadId = await seedAgentThread(harness, 'background-app', 'Background button');
  await page.goto(harness.url);
  await page.getByText('Background button').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'sweep the corpus', null);

  await waitForGate(harness, 'cut');
  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card.getByTestId('subagent-group-label')).toContainText('Sweeper');
  // Running, in the foreground, and the turn owns the composer.
  await expect(card.getByTestId('subagent-group-background-pill')).toHaveCount(0);
  await expect(page.getByTestId('composer-interrupt')).toBeVisible();

  // --- Background it -------------------------------------------------
  await card.hover();
  await card.getByTestId('subagent-group-background-button').click();
  await expect(card.getByTestId('subagent-group-error')).toHaveCount(0);
  await advance(harness, mockId, 'cut');
  await harness.waitForEvent('provider:turn_completed');

  // The main turn returned: the composer is a composer again.
  await expect(page.getByTestId('composer-send')).toBeVisible();
  await expect(page.getByTestId('composer-interrupt')).toHaveCount(0);
  await expect(card.getByTestId('subagent-group-background-pill')).toBeVisible();

  // --- The pane marks the cut ---------------------------------------
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane.getByTestId('agent-pane-background-pill')).toBeVisible();
  await expect(pane.getByTestId('agent-pane-streaming-paused')).toBeVisible();
  const paneTimeline = pane.getByTestId('agent-pane-timeline');
  await expect(paneTimeline.getByText('Reading the first shard.')).toBeVisible();
  // Nothing streamed after the cut — that is what "paused" means.
  await expect(paneTimeline.getByText(BACKFILL_TEXT)).toHaveCount(0);

  // --- The task notification completes the transcript ---------------
  await waitForGate(harness, 'finish');
  await advance(harness, mockId, 'finish');
  await expect(paneTimeline.getByText(BACKFILL_TEXT)).toBeVisible();
  // ...tool rows included, not just the text.
  await expect(paneTimeline.getByRole('link', { name: 'Open README.md in editor' })).toBeVisible();
  // A file the backfill could not read would say so on the card.
  await expect(card.getByTestId('subagent-group-output-error')).toHaveCount(0);
  await expect(pane.getByTestId('agent-pane-streaming-paused')).toHaveCount(0);
  // The notification's `usage` is the whole run's, and it persists onto
  // the launch row — a backgrounded agent's live ticks are gone by then.
  await expect(pane.getByTestId('agent-pane-status-line')).toContainText('2 tools');
  await expect(pane.getByTestId('agent-pane-status-line')).toContainText('24.1k tokens');

  // The backfilled rows belong to the agent, not the main thread.
  await expect
    .poll(async () => {
      const items = await listItems(harness, threadId);
      return items
        .filter((i) => i.summary?.includes('Backfilled:'))
        .map((i) => i.parentId ?? '');
    })
    .toEqual(['tu-agent']);
});
