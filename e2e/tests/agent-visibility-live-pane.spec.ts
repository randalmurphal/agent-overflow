// The agent pane is OPEN while an awaited agent streams: its instructions,
// thinking, prose, and final text all have to appear as they arrive. Every
// other pane spec opens the pane after the transcript settled; this one
// watches it live.
//
// Written against a real defect (2026-08-23): an INLINE agent's text and
// thinking never left the CLI at all, because the synchronous Task path
// drops every block that is not a tool_use/tool_result unless the session
// spawned with `--forward-subagent-text`. The mock cannot reproduce the
// CLI's filter — it is upstream of the wire this harness replays — so the
// flag itself is pinned in Go (TestBuildArgsForwardsSubagentText) and this
// spec pins what the pane must do with the envelopes once they arrive.
import { test, expect } from './fixtures.js';
import {
  RESULT_LINE,
  advance,
  claudeScenario,
  emit,
  seedAgentThread,
  startMock,
  taskStartedLine,
  taskUpdatedLine,
  textLines,
  toolResultLine,
  toolUseLine,
  waitForGate,
} from './agent-visibility-helpers.js';

const PROMPT = 'Map every agent-tool surface in AO.';
const MID = 'Live note: the parser lives in internal/provider.';
const FINAL = 'Live final: mapped every surface.';
const THINK = 'Thinking live about the map.';

/** REAL sidechain shape (docs/references/fixtures/claude/task_progress_20260822.ndjson):
 *  a subagent's text/thinking arrive as a bare `assistant` envelope with
 *  `parent_tool_use_id` and NO stream_event lifecycle. */
function sidechainBlockLine(messageId: string, block: Record<string, unknown>, parent: string): string {
  return JSON.stringify({
    type: 'assistant',
    message: { id: messageId, role: 'assistant', model: 'claude-haiku-mock', type: 'message', content: [block] },
    parent_tool_use_id: parent,
  });
}
/** The subagent's own prompt, echoed by the CLI as a parented `user`
 *  envelope (live 2.1.237 capture). The uuid is load-bearing: it is what
 *  the row is keyed and deduped on, and an envelope without one is
 *  dropped. */
function sidechainPromptLine(uuid: string, text: string, parent: string): string {
  return JSON.stringify({
    type: 'user',
    uuid,
    subagent_type: 'Explore',
    message: { role: 'user', content: [{ type: 'text', text }] },
    parent_tool_use_id: parent,
  });
}

function liveScenario() {
  return claudeScenario('live-pane', [
    emit([
      ...textLines('msg-lead', 'Delegating.'),
      toolUseLine('msg-agent', 'tu-live', 'Agent', {
        description: 'map surfaces',
        subagent_type: 'Explore',
      }),
      taskStartedLine('task-live', 'tu-live', 'map surfaces'),
      sidechainPromptLine('prompt-uuid-1', PROMPT, 'tu-live'),
      toolUseLine('msg-b1', 'tu-live-b1', 'Bash', { command: 'ls internal' }, 'tu-live'),
      toolResultLine('tu-live-b1', 'provider\nstore', { parentToolUseId: 'tu-live' }),
    ]),
    { waitSignal: { name: 'open' } },
    emit([
      sidechainBlockLine('msg-think', { type: 'thinking', thinking: THINK, signature: 'sig' }, 'tu-live'),
      sidechainBlockLine('msg-mid', { type: 'text', text: MID }, 'tu-live'),
      toolUseLine('msg-b2', 'tu-live-b2', 'Bash', { command: 'ls internal/provider' }, 'tu-live'),
      toolResultLine('tu-live-b2', 'claude\ncodex', { parentToolUseId: 'tu-live' }),
    ]),
    { waitSignal: { name: 'final' } },
    emit([
      sidechainBlockLine('msg-final', { type: 'text', text: FINAL }, 'tu-live'),
      taskUpdatedLine('task-live', { status: 'completed', end_time: 1787415964725 }),
      toolResultLine('tu-live', FINAL),
      RESULT_LINE,
    ]),
  ]);
}

test('an open pane shows prose, thinking, and the final text as the agent streams them', async ({
  harness,
  page,
}) => {
  await harness.rpc('HarnessSetScenario', { scenario: liveScenario() });
  const threadId = await seedAgentThread(harness, 'live-pane-app', 'Live pane');
  await harness.open(page);
  await page.getByText('Live pane').click();
  const mockId = await startMock(harness, threadId);
  await harness.rpc('SendMessage', threadId, 'map surfaces', null);

  await waitForGate(harness, 'open');
  const timeline = page.getByTestId('message-timeline-scroll');
  const card = timeline.getByTestId('subagent-group').first();
  await expect(card).toBeVisible();
  await card.hover();
  await card.getByTestId('subagent-group-open-pane').first().click();
  const pane = page.getByTestId('companion-pane-agent-body');
  await expect(pane).toBeVisible();
  const paneTimeline = pane.getByTestId('agent-pane-timeline');
  // The agent's instructions: a plain user-side message, first in the pane.
  await expect(paneTimeline.getByText(PROMPT)).toBeVisible();
  await expect(paneTimeline.getByTestId('user-message-bubble')).toHaveCount(1);
  await expect(paneTimeline.getByText('ls internal', { exact: true })).toBeVisible();

  await advance(harness, mockId, 'open');
  await expect(paneTimeline.getByText(MID)).toBeVisible();
  await expect(paneTimeline.getByTestId('thinking-toggle')).toHaveCount(1);
  await expect(paneTimeline.getByText('ls internal/provider', { exact: true })).toBeVisible();

  await waitForGate(harness, 'final');
  await advance(harness, mockId, 'final');
  await expect(paneTimeline.getByText(FINAL)).toBeVisible();
  await harness.waitForEvent('provider:turn_completed');
  await expect(paneTimeline.getByText(MID)).toBeVisible();
  await expect(paneTimeline.getByText(FINAL)).toBeVisible();
  await expect(paneTimeline.getByText(PROMPT)).toBeVisible();

  // The card's own body shows the instructions too — it is the first
  // user_text child, which is the body digest's initial-prompt slot.
  await card.getByTestId('subagent-group-toggle').first().click();
  await expect(card.getByTestId('subagent-group-body').first().getByText(PROMPT)).toBeVisible();
});
