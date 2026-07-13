import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { __setSmoothingClockForTest, createThreadPane } from '../../stores/thread.svelte';
import type { SmoothingClock } from '../../markdown/smoothing/PerItemSmoother';
import {
  projectTurnCompleted,
  projectTurnStarted,
} from '../../stores/threadStatuses.svelte';
import { getToasts } from '../../stores/toast.svelte';
import { clearThreadScrollSnapshotsForTest } from '../../utils/threadScrollSnapshots';
import MessageTimeline from './MessageTimeline.svelte';

beforeAll(() => {
  if (typeof (Element.prototype as unknown as { animate?: unknown }).animate !== 'function') {
    (Element.prototype as unknown as { animate: (...args: unknown[]) => unknown }).animate =
      function fakeAnimate() {
        return {
          finished: Promise.resolve(),
          currentTime: 0,
          playState: 'finished' as const,
          cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
          addEventListener() {}, removeEventListener() {},
          onfinish: null, oncancel: null,
        };
      };
  }
});

function agentMeta(description: string): string {
  return JSON.stringify({
    toolName: 'Agent',
    input: { description, subagent_type: 'Explore' },
  });
}

class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number { return this.current; }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void { this.pending.delete(h); }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
}

describe('<MessageTimeline>', () => {
  beforeEach(async () => {
    resetBindingMocks();
    clearThreadScrollSnapshotsForTest();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });

  it('renders the empty state for a blank thread', async () => {
    const pane = await buildPane();
    const { getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText(/No messages yet/i)).toBeInTheDocument();
  });

  it('mounts persisted workflow proposals at their timeline position', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'workflow-proposal:1', kind: 'workflow_proposal', summary: 'Ship it',
        meta: JSON.stringify({
          state: 'pending', projectId: 'p', projectName: 'AO', workflowId: 'build',
          workflowName: 'Build', workflowScope: 'shared', goal: 'Ship it', seeds: {},
          baseBranch: 'main', stepMode: false,
        }),
      }),
    ]);
    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    expect(getByTestId('wf-confirm-card')).toHaveTextContent('AO · Ship it · Build · main');
  });

  it('keeps active-turn status out of the virtualized history', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
    ]);
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() - 3_000 });

    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    const scroll = getByTestId('message-timeline-scroll');
    expect(scroll.querySelectorAll('[data-testid="message-timeline-node"]')).toHaveLength(1);
    expect(queryByTestId('activity-rail-working')).toBeNull();
  });

  it('hides the empty state while a blank thread is working without mounting live UI', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() - 3_000 });

    const { queryByTestId, queryByText } = render(MessageTimeline, { props: { pane } });

    expect(queryByTestId('activity-rail-working')).toBeNull();
    expect(queryByText(/No messages yet/i)).toBeNull();
  });

  it('renders user, assistant, error, and compaction rows from unified items', async () => {
    const pane = await buildPane(undefined, [
      makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({ id: 'text:0:0', itemIndex: 1, kind: 'assistant_text', summary: 'hello' }),
      makeItem({ id: 'error:0:0', itemIndex: 2, kind: 'error', role: 'system', summary: 'boom' }),
      makeItem({ id: 'compact:1', turnIndex: 1, itemIndex: 0, kind: 'compaction', role: 'system', summary: 'Context compacted' }),
    ]);

    const { getByTestId, getByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText('hi')).toBeInTheDocument();
    expect(getByText('hello')).toBeInTheDocument();
    expect(getByText('boom')).toBeInTheDocument();
    expect(getByText('Context compacted')).toBeInTheDocument();

    const compactionDivider = getByTestId('compaction-divider');
    expect(compactionDivider).toHaveClass('my-8');
  });

  it('updates a visible leaf row from pane state', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        summary: 'first version',
        updatedAt: 1,
      }),
    ]);
    const { getByText, queryByText } = render(MessageTimeline, { props: { pane } });

    expect(getByText('first version')).toBeInTheDocument();

    pane.applyItemPatch({
      threadId: 'thread-1',
      itemId: 'text:0:0',
      kind: 'assistant_text',
      patch: { summary: 'second version', updatedAt: 2 },
    });
    await tick();

    expect(queryByText('first version')).toBeNull();
    expect(getByText('second version')).toBeInTheDocument();
  });

  it('dispatches terminal_interaction items to TerminalInteractionRow', async () => {
    // Phase 6: `terminal_interaction` items land in the timeline as
    // muted "Waited for background terminal" markers — a distinct
    // render path from AssistantMessage / ToolCallCard / the compaction
    // divider. Pinning the dispatch here keeps the MessageTimeline
    // switch honest as new kinds get added.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'waited:pid-42:0:0',
        kind: 'terminal_interaction',
        role: 'assistant',
        summary: 'Waited for background terminal',
      }),
    ]);

    const { getByTestId } = render(MessageTimeline, { props: { pane } });

    const row = getByTestId('terminal-interaction-row');
    expect(row.textContent).toContain('Waited for background terminal');
  });

  it('updates a visible terminal wait carrier when its command completion arrives', async () => {
    const wait = makeItem({
      id: 'waited:pid-42:0:0',
      kind: 'terminal_interaction',
      role: 'assistant',
      status: 'running',
      summary: 'Waiting for background terminal',
      meta: JSON.stringify({ process_id: 'pid-42' }),
    });
    const completion = makeItem({
      id: 'complete-cmd-1',
      itemIndex: 1,
      kind: 'tool_completion',
      toolName: 'command_execution',
      completionOf: 'cmd-1',
      status: 'errored',
      summary: 'Command failed',
      meta: JSON.stringify({ process_id: 'pid-42' }),
      payloadKind: 'command_output',
      payloadId: 'payload-cmd-1',
      payloadMeta: JSON.stringify({
        command: 'sleep 1; echo done',
        exitCode: 1,
        lineCount: 1,
        preview: 'Command failed',
      }),
    });
    const completedWait = makeItem({
      ...wait,
      status: 'completed',
      summary: 'Waited for background terminal',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ provider: 'codex' }), [wait]);
    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('terminal-interaction-row').textContent).toContain(
      'Waiting for background terminal',
    );
    expect(queryByTestId('wait-group-children')).toBeNull();

    pane.upsertItems([completion, completedWait]);
    await tick();

    expect(getByTestId('terminal-interaction-row').textContent).toContain(
      'Waited for background terminal',
    );
    expect(queryByTestId('wait-group-children')).toBeNull();
    expect(getByTestId('command-output-row').textContent).toContain('sleep 1; echo done');
    expect(getByTestId('command-output-row').textContent).toContain('exit 1');
  });

  it('keeps a same-row Bash completion visually fresh without a structural rebuild', async () => {
    const running = makeItem({
      id: 'bash-inline',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'Bash: sleep 1',
      meta: JSON.stringify({ input: { command: 'sleep 1' } }),
    });
    const pane = await buildPane(undefined, [running]);
    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('command-output-command').textContent?.trim()).toBe('sleep 1');
    expect(getByTestId('command-output-status').getAttribute('data-state')).toBe('running');
    const revision = pane.timelineRevision;

    pane.upsertItem(makeItem({
      ...running,
      status: 'completed',
      payloadId: 'payload-bash-inline',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({ command: 'sleep 1', exitCode: 0, preview: 'done' }),
      updatedAt: running.updatedAt + 1,
    }));
    await tick();

    expect(pane.timelineRevision).toBe(revision);
    expect(getByTestId('command-output-command').textContent?.trim()).toBe('sleep 1');
    expect(queryByTestId('command-output-status')).toBeNull();
    expect(getByTestId('command-output-toggle')).toHaveAttribute('aria-expanded', 'false');
  });

  it('folds the redundant Codex wait_agent completion into the wait header instead of a separate row', async () => {
    const wait = makeItem({
      id: 'wait-agents',
      kind: 'tool_call',
      role: 'assistant',
      status: 'completed',
      toolName: 'wait_agent',
      summary: 'wait_agent',
      meta: JSON.stringify({
        input: {
          tool: 'wait_agent',
          receiverThreadIds: ['child-1'],
          agentsStates: {
            'child-1': { status: 'completed', message: 'Agent finished cleanly' },
          },
        },
      }),
    });
    const waitCompletion = makeItem({
      id: 'complete-wait-agents',
      itemIndex: 1,
      kind: 'tool_completion',
      role: 'assistant',
      status: 'completed',
      toolName: 'wait_agent',
      completionOf: 'wait-agents',
      summary: 'wait_agent',
      payloadId: 'payload-wait-agents',
      payloadKind: 'tool_call_result',
      payloadMeta: JSON.stringify({ itemStatus: 'completed', preview: 'Agent finished cleanly' }),
      meta: wait.meta,
    });
    const agentCompletion = makeItem({
      id: 'complete-spawn-agent',
      itemIndex: 2,
      kind: 'tool_completion',
      role: 'assistant',
      status: 'completed',
      toolName: 'collab_agent',
      completionOf: 'spawn-agent',
      summary: 'collab_agent: review -> done',
      payloadId: 'payload-wait-agents',
      payloadKind: 'tool_call_result',
      payloadMeta: JSON.stringify({ itemStatus: 'completed', preview: 'Agent finished cleanly' }),
    });
    const pane = await buildPane(makeThread({ provider: 'codex' }), [wait, waitCompletion, agentCompletion]);

    const { getByTestId, queryAllByText } = render(MessageTimeline, { props: { pane } });

    // (b) the standalone wait_agent completion is folded into the wait group as
    // its header (rendered "Finished waiting"), replacing the carrier's "Waiting
    // for N agents" — NOT shown as a separate redundant row. (c) nests below.
    const waitGroup = getByTestId('wait-group');
    expect(waitGroup.textContent).toContain('Finished waiting');
    expect(waitGroup.textContent).not.toContain('Waiting for Agent');
    expect(queryAllByText('Finished waiting')).toHaveLength(1);
    expect(getByTestId('wait-group-children').textContent).toContain('Agent finished cleanly');
  });

  it('renders a live Codex spawn/wait completion sequence before refresh', async () => {
    const pane = await buildPane(makeThread({ provider: 'codex' }), [
      makeItem({
        id: 'assistant-before-review',
        itemIndex: 32,
        kind: 'assistant_text',
        summary: 'The diff is tiny.',
      }),
    ]);
    const { getByTestId, queryByText } = render(MessageTimeline, { props: { pane } });

    pane.upsertItems([
      makeItem({
        id: 'spawn-review',
        itemIndex: 25,
        kind: 'tool_call',
        toolName: 'collab_agent',
        isBackground: true,
        summary: 'collab_agent: review',
        meta: JSON.stringify({
          toolName: 'collab_agent',
          input: {
            tool: 'spawn_agent',
            receiverThreadIds: ['child-review'],
            newAgentNickname: 'Chandrasekhar',
          },
        }),
      }),
      makeItem({
        id: 'child-prompt',
        itemIndex: 26,
        kind: 'user_text',
        role: 'user',
        parentId: 'spawn-review',
        summary: 'Review the timeline code',
      }),
      makeItem({
        id: 'child-progress',
        itemIndex: 27,
        kind: 'assistant_text',
        parentId: 'spawn-review',
        summary: 'I will inspect the live path.',
      }),
      makeItem({
        id: 'wait-review',
        itemIndex: 33,
        kind: 'tool_call',
        toolName: 'wait_agent',
        summary: 'wait_agent',
        meta: JSON.stringify({
          input: {
            tool: 'wait_agent',
            receiverThreadIds: ['child-review'],
            agentsStates: {
              'child-review': {
                status: 'completed',
                message: 'Recommended | frontend/src/lib/components/chat/MessageTimeline.svelte:223 | retry layout',
              },
            },
          },
        }),
      }),
    ]);
    await tick();

    expect(getByTestId('wait-group').textContent).toContain('Waiting for Agent');
    expect(getByTestId('collab-tool-row-receivers').textContent).toBe('└ Chandrasekhar');
    expect(queryByText('Review the timeline code')).toBeNull();
    expect(queryByText('I will inspect the live path.')).toBeNull();

    pane.upsertItems([
      makeItem({
        id: 'complete-wait-review',
        itemIndex: 68,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        payloadMeta: JSON.stringify({ itemStatus: 'completed', preview: 'Review finished' }),
        summary: 'wait_agent',
        meta: JSON.stringify({
          input: {
            tool: 'wait_agent',
            receiverThreadIds: ['child-review'],
            agentsStates: {
              'child-review': {
                status: 'completed',
                message: 'Recommended | frontend/src/lib/components/chat/MessageTimeline.svelte:223 | retry layout',
              },
            },
          },
        }),
      }),
      makeItem({
        id: 'complete-spawn-review',
        itemIndex: 69,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        payloadMeta: JSON.stringify({ itemStatus: 'completed', preview: 'Review finished' }),
        summary: 'collab_agent: review -> done',
        meta: JSON.stringify({ wait_carrier_id: 'wait-review', item_status: 'completed' }),
      }),
      makeItem({
        id: 'assistant-after-review',
        itemIndex: 70,
        kind: 'assistant_text',
        summary: 'The review caught one edge I agree with.',
      }),
    ]);
    await tick();

    expect(getByTestId('wait-group-children').textContent).toContain('Review finished');
    // (b) folds into the wait header — the group flips from "Waiting" to
    // "Finished waiting" in place (no separate row, no flash).
    expect(getByTestId('wait-group').textContent).toContain('Finished waiting');
    expect(getByTestId('wait-group').textContent).not.toContain('Waiting for');
    expect(queryByText('The review caught one edge I agree with.')).toBeInTheDocument();
  });

  it('renders notification rows without routing them through tool lifecycle cards', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'notif-1',
        kind: 'notification',
        role: 'system',
        summary: 'Background command "sleep 10" completed',
      }),
    ]);

    const { getByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getByTestId('notification-row').textContent).toContain('Background command "sleep 10" completed');
    expect(queryByTestId('tool-call-card')).toBeNull();
  });

  it('renders Claude model fallback reasons as a warning notification', async () => {
    const reason = 'Fable 5 safeguards flagged this cybersecurity request. Switched to Opus 4.8.';
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'model-fallback:req-1',
        kind: 'notification',
        role: 'system',
        toolName: 'model_refusal_fallback',
        summary: reason,
        meta: JSON.stringify({
          kind: 'model_refusal_fallback',
          originalModel: 'claude-fable-5',
          fallbackModel: 'claude-opus-4-8',
          category: 'cyber',
        }),
      }),
    ]);

    const { getByTestId } = render(MessageTimeline, { props: { pane } });
    const row = getByTestId('notification-row');
    expect(row.textContent).toContain(reason);
    expect(row.textContent).toContain('Reason: Cybersecurity safety classifier');
    expect(row).toHaveAttribute('role', 'status');
    expect(row.className).toContain('text-warning');
  });

  it('renders one DiffFileBlock per file for multi-file tool_result rows', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        summary: 'Edit src/a.ts',
        payloadId: 'payload-1',
        payloadKind: 'tool_result',
        payloadMeta: JSON.stringify({
          title: 'File change',
          inlineDiff: {
            availability: 'exact_patch',
            files: [
              { path: 'src/a.ts', kind: 'modified', insertions: 5, deletions: 2 },
              { path: 'src/b.ts', kind: 'added', insertions: 3, deletions: 0 },
            ],
          },
        }),
      }),
    ]);

    const { getAllByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    // No outer card chrome, no decision/completion badge, no chip
    // strip. Each file is its own DiffFileBlock keyed by data-file-path.
    const blocks = getAllByTestId('diff-file-block');
    const paths = blocks.map((el) => el.getAttribute('data-file-path'));
    expect(paths).toEqual(['src/a.ts', 'src/b.ts']);
    expect(queryByTestId('turn-diff-badge')).toBeNull();
  });

  it('renders proposed plans from payload-bearing tool rows', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: '# Ship it' }));
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-1',
        kind: 'tool_call',
        summary: 'Plan',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Ship it',
          lineCount: 3,
          charCount: 12,
          preview: '# Ship it',
        }),
      }),
    ]);

    const { getAllByText } = render(MessageTimeline, { props: { pane } });

    expect(getAllByText('Ship it').length).toBeGreaterThan(0);
  });

  it('renders proposed plan tables with Streamdown wrapping within the message width', async () => {
    const planMarkdown = [
      '# Behavior Spec',
      '',
      '| Behavior Spec | Predicate |',
      '| --- | --- |',
      '| There is an active turn | `canReview` |',
    ].join('\n');
    setBindingMock('GetPayloadData', async () => ({ data: planMarkdown }));
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'plan-table',
        kind: 'tool_call',
        summary: 'Plan',
        payloadId: 'plan-table-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Behavior Spec',
          lineCount: 5,
          charCount: planMarkdown.length,
          preview: planMarkdown,
        }),
      }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });

    const tableWrapper = container.querySelector('[data-streamdown-table]');
    expect(tableWrapper).not.toBeNull();
    expect(tableWrapper).toHaveClass('overflow-visible');
    expect(tableWrapper).toHaveClass('max-w-full');
    expect(tableWrapper?.querySelector('table')).not.toBeNull();
  });

  it('marks tool / think leaf rows with data-rail="true" and flat rows with "false"', async () => {
    // Phase 1 rail: every per-row wrapper carries a `data-rail`
    // attribute derived from the leaf item's kind at first render.
    // Tool / completion / thinking leaves participate in the
    // continuous left-border; assistant_text, user_text, and other
    // structural rows opt out. The attribute is the behavior contract;
    // the `border-l ...` class is the implementation. Assert on the
    // attribute so a future class rename doesn't silently break the
    // discriminator.
    const pane = await buildPane(undefined, [
      makeItem({ id: 'u:0', kind: 'user_text', role: 'user', summary: 'hi' }),
      makeItem({
        id: 't:0',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Bash',
        summary: 'ls',
      }),
      makeItem({
        id: 't:1',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Bash',
        summary: 'ls',
      }),
      makeItem({
        id: 'th:0',
        itemIndex: 3,
        kind: 'thinking',
        summary: 'pondering',
      }),
      makeItem({
        id: 'a:0',
        itemIndex: 4,
        kind: 'assistant_text',
        summary: 'done',
      }),
    ]);
    const { container } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers.length).toBe(5);
    // Order matches the items array above.
    expect(wrappers[0].getAttribute('data-rail')).toBe('false'); // user_text
    expect(wrappers[1].getAttribute('data-rail')).toBe('true');  // tool_call
    expect(wrappers[2].getAttribute('data-rail')).toBe('true');  // tool_completion
    expect(wrappers[3].getAttribute('data-rail')).toBe('true');  // thinking
    expect(wrappers[4].getAttribute('data-rail')).toBe('false'); // assistant_text

    // Rail-bearing rows also carry the border-l utility; flat rows
    // don't. Pin both since the class is what produces the visual.
    expect(wrappers[1].className).toContain('border-l');
    expect(wrappers[0].className).not.toContain('border-l');
    expect(wrappers[4].className).not.toContain('border-l');
  });

  it('extends data-rail="true" to subagent / wait group container rows', async () => {
    // Group containers (SubagentGroup, WaitGroup) participate in the
    // rail so consecutive agent/wait rows form one continuous left
    // border with adjacent tool rows.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool:0',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Bash',
        summary: 'ls',
      }),
      makeItem({
        id: 'agent:0',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: one',
        meta: agentMeta('First agent'),
      }),
      makeItem({
        id: 'agent:1',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: two',
        meta: agentMeta('Second agent'),
      }),
      makeItem({
        id: 'wait:0',
        itemIndex: 3,
        kind: 'tool_call',
        toolName: 'wait_agent',
        summary: 'wait_agent',
        meta: JSON.stringify({ input: { tool: 'wait_agent' } }),
      }),
    ]);
    const { container } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    // tool_call leaf + agent group + agent group + wait_group wrapper.
    expect(wrappers.length).toBe(4);
    for (const wrapper of wrappers) {
      expect(wrapper.getAttribute('data-rail')).toBe('true');
      expect(wrapper.className).toContain('border-l');
    }
  });

  it('folds consecutive Read tool_calls into a single rail-bearing read_group row', async () => {
    // Three reads in a row collapse to one ReadGroupRow with three
    // EditorLink members. Pin: (1) only ONE wrapper appears for the
    // run (vs. three when the grouping is bypassed); (2) the wrapper
    // carries data-rail="true" so it stays under the continuous rail
    // alongside neighboring tool rows; (3) each member surfaces as a
    // discrete EditorLink keyed off its workspace-relative path.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'read:0',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: src/lib/foo.ts',
      }),
      makeItem({
        id: 'read:1',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: src/lib/bar.ts',
      }),
      makeItem({
        id: 'read:2',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: src/lib/baz.ts',
      }),
    ]);
    const { container, getByTestId, getAllByTestId } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers).toHaveLength(1);
    expect(wrappers[0].getAttribute('data-rail')).toBe('true');
    expect(getByTestId('read-group-row')).toBeInTheDocument();
    const links = getAllByTestId('editor-link');
    expect(links.map((el) => el.getAttribute('data-path'))).toEqual([
      'src/lib/foo.ts',
      'src/lib/bar.ts',
      'src/lib/baz.ts',
    ]);
  });

  it('keeps proposed-plan rows out of the continuous left rail', async () => {
    // Proposed plans render as standalone markdown sections, not the
    // compact chev/icon/label/preview pattern other tool rows share.
    // The rail running alongside that body would make the plan look
    // nested under the tool gutter, so plan rows opt out of `data-rail`
    // and the border-l/ml/pl shell.
    setBindingMock('GetPayloadData', async () => ({ data: '# Ship it' }));
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-before',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Bash',
        summary: 'Bash: ls',
      }),
      makeItem({
        id: 'plan-1',
        itemIndex: 1,
        kind: 'tool_call',
        summary: 'Plan',
        payloadId: 'plan-payload',
        payloadKind: 'proposed_plan',
        payloadMeta: JSON.stringify({
          title: 'Ship it',
          lineCount: 3,
          charCount: 12,
          preview: '# Ship it',
        }),
      }),
    ]);

    const { container } = render(MessageTimeline, { props: { pane } });
    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers).toHaveLength(2);
    // The non-plan tool row keeps the rail so this test would also
    // catch a regression that disabled the rail wholesale.
    expect(wrappers[0].getAttribute('data-rail')).toBe('true');
    expect(wrappers[0].className).toContain('border-l');
    expect(wrappers[1].getAttribute('data-rail')).toBe('false');
    expect(wrappers[1].className).not.toContain('border-l');
  });

  it('updates leaf rail chrome from pane state after a non-structural upsert', async () => {
    const tool = makeItem({
      id: 'plan-1',
      itemIndex: 0,
      kind: 'tool_call',
      summary: 'Plan',
    });
    const pane = await buildPane(undefined, [tool]);

    const { container } = render(MessageTimeline, { props: { pane } });
    const wrapper = () => container.querySelector('[data-testid="message-timeline-node"]');

    expect(wrapper()?.getAttribute('data-rail')).toBe('true');
    expect(wrapper()?.className).toContain('border-l');

    const revisionBefore = pane.timelineRevision;
    pane.upsertItem({
      ...tool,
      payloadId: 'plan-payload',
      payloadKind: 'proposed_plan',
      payloadMeta: JSON.stringify({
        title: 'Ship it',
        lineCount: 3,
        charCount: 12,
        preview: '# Ship it',
      }),
      updatedAt: tool.updatedAt + 1,
    });
    await tick();

    expect(pane.timelineRevision).toBe(revisionBefore);
    expect(wrapper()?.getAttribute('data-rail')).toBe('false');
    expect(wrapper()?.className).not.toContain('border-l');
  });

  it('renders a single Read through the stable read_group row from first appearance', async () => {
    // A one-item read_group keeps the same virtualized row key and
    // component shell when another adjacent Read arrives later.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'read:lonely',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: src/lib/foo.ts',
      }),
    ]);
    const { container, queryByTestId, getByTestId, getAllByTestId } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers).toHaveLength(1);
    expect(wrappers[0].getAttribute('data-rail')).toBe('true');
    const initialReadRow = getByTestId('read-group-row');
    expect(initialReadRow).toBeInTheDocument();
    expect(getByTestId('read-group-row-label').textContent).toBe('read');
    expect(queryByTestId('tool-call-card')).toBeNull();
    expect(getAllByTestId('editor-link').map((el) => el.getAttribute('data-path'))).toEqual([
      'src/lib/foo.ts',
    ]);

    pane.upsertItem(makeItem({
      id: 'read:second',
      itemIndex: 1,
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'Read: src/lib/bar.ts',
    }));
    await tick();

    expect(getByTestId('read-group-row')).toBe(initialReadRow);
    expect(container.querySelectorAll('[data-testid="message-timeline-node"]')).toHaveLength(1);
    expect(getAllByTestId('editor-link').map((el) => el.getAttribute('data-path'))).toEqual([
      'src/lib/foo.ts',
      'src/lib/bar.ts',
    ]);
  });

  it('renders one wrapper per timeline node', async () => {
    // Virtualization is owned by TimelineVirtualizer; in production, the
    // engine mounts only the rows that fit the viewport plus an overscan
    // buffer. The test environment runs in happy-dom where all dimensions
    // are 0, so bufferSize-based windowing would render zero rows;
    // MessageTimeline passes `renderAll` under
    // `import.meta.env.MODE === 'test'` so tests can assert on rendered
    // DOM. The contract verified here: every grouped node produces
    // exactly one `[data-testid="message-timeline-node"]` wrapper.
    const items = Array.from({ length: 50 }, (_, i) =>
      makeItem({
        id: `text:${i}`,
        turnIndex: Math.floor(i / 10),
        itemIndex: i % 10,
        summary: `message ${i}`,
        createdAt: i,
      }),
    );
    const pane = await buildPane(undefined, items);
    const { container } = render(MessageTimeline, { props: { pane } });

    const wrappers = container.querySelectorAll('[data-testid="message-timeline-node"]');
    expect(wrappers.length).toBe(50);
  });

  it('renders Claude foreground Agents as independent SubagentGroup cards', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'agent-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: one',
        meta: agentMeta('First agent'),
      }),
      makeItem({
        id: 'agent-2',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        status: 'running',
        summary: 'Agent: two',
        meta: agentMeta('Second agent'),
      }),
    ]);

    const { getAllByTestId, queryByTestId } = render(MessageTimeline, { props: { pane } });

    expect(getAllByTestId('subagent-group')).toHaveLength(2);
    expect(getAllByTestId('subagent-group-preview').map((node) => node.textContent?.trim())).toEqual([
      '└ Initializing...',
      '└ Initializing...',
    ]);
  });

  describe('windowed history', () => {
    // Build a pane driven directly (not via buildPane) so the test can
    // prime the initial-slice binding with its own items + hasMore flag.
    // The integration shape is stable: createThreadPane + switchThread
    // reads the paged binding we stub below.
    async function buildWindowedPane(opts: {
      items: ReturnType<typeof makeItem>[];
      hasMore?: boolean;
      hasMoreNewer?: boolean;
      oldestTurnIndex?: number;
      newestTurnIndex?: number;
    }): Promise<ReturnType<typeof createThreadPane>> {
      const { items, hasMore = false, hasMoreNewer = false, oldestTurnIndex, newestTurnIndex } = opts;
      const floor =
        oldestTurnIndex ?? (items.length > 0 ? items[0].turnIndex : -1);
      const ceiling =
        newestTurnIndex ?? (items.length > 0 ? items[items.length - 1].turnIndex : -1);
      setBindingMock('SwitchThread', async () => {});
      setBindingMock('ListThreadSliceAround', async () => ({
        items,
        oldestTurnIndex: floor,
        newestTurnIndex: ceiling,
        hasMore,
        hasMoreOlder: hasMore,
        hasMoreNewer,
      }));
      setBindingMock('ListRecentTurns', async () => []);
      const pane = createThreadPane();
      await pane.switchThread(makeThread());
      return pane;
    }

    it('renders the Load older button when pane.hasMoreHistory is true', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const button = getByTestId('load-older-messages') as HTMLButtonElement;
      expect(button.textContent ?? '').toContain('Load older messages');
      expect(button.disabled).toBe(false);
    });

    it('hides the Load older button when pane.hasMoreHistory is false', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a' })],
        hasMore: false,
      });

      const { queryByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryByTestId('load-older-messages')).toBeNull();
    });

    // Stage 1 redesign: the button was restyled off raw border-border
    // onto the subtle border + control radius + ghost-text hover pattern
    // the rest of the app uses. Pin the class list so a lazy edit can't
    // drift it back toward the old heavy chrome.
    it('Load older button uses the redesigned token classes', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });
      const button = getByTestId('load-older-messages');
      const cls = button.className;
      // Post-Button-migration the chrome comes from the primitive's
      // `secondary` variant — we still assert the redesigned design
      // tokens flow through (border-subtle at rest, muted fg, control
      // radius, hover-to-foreground color). hover:border-border is
      // expected on the secondary variant so we don't assert against
      // it here.
      expect(cls).toContain('border-border-subtle');
      expect(cls).toContain('rounded-[var(--radius-control)]');
      expect(cls).toContain('text-fg-muted');
      expect(cls).toContain('hover:text-fg');
    });

    it('clicking Load older invokes pane.loadOlder', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      const loadOlderSpy = vi.spyOn(pane, 'loadOlder').mockResolvedValue({
        status: 'noop',
        insertedBeforeWindow: false,
        insertedRows: false,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });
      await fireEvent.click(getByTestId('load-older-messages'));
      await tick();

      expect(loadOlderSpy).toHaveBeenCalledTimes(1);
    });

    it('disables the button while loadOlder is in flight', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'tail', turnIndex: 10 })],
        hasMore: true,
        oldestTurnIndex: 10,
      });
      // Hold ListItemsBeforeCursor open so the store's loadingOlder stays
      // true across the render we want to assert on.
      let release: (value: unknown) => void = () => {};
      const pending = new Promise((resolve) => { release = resolve; });
      setBindingMock('ListItemsBeforeCursor', async () => {
        await pending;
        return { items: [], oldestTurnIndex: 10, hasMore: false };
      });

      const { getByTestId, rerender } = render(MessageTimeline, { props: { pane } });
      void pane.loadOlder();
      // One synchronous task boundary is enough for loadingOlder=true to
      // flip before Svelte paints; rerender makes the $effect re-read
      // the getter.
      await tick();
      await rerender({ pane });

      const button = getByTestId('load-older-messages') as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button.textContent ?? '').toContain('Loading');

      release({ items: [], oldestTurnIndex: 10, hasMore: false });
      await tick();
    });

    it('renders newer-history controls when pane.hasMoreNewer is true', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'old-window', turnIndex: 3 })],
        hasMore: true,
        hasMoreNewer: true,
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      expect(getByTestId('load-newer-messages')).toBeInTheDocument();
      expect(getByTestId('jump-to-latest-messages')).toBeInTheDocument();
      expect(getByTestId('scroll-to-bottom')).toBeInTheDocument();
    });

    it('clicking Load newer invokes pane.loadNewer', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'old-window', turnIndex: 3 })],
        hasMoreNewer: true,
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
      });
      const loadNewerSpy = vi.spyOn(pane, 'loadNewer').mockResolvedValue({
        status: 'noop',
        insertedBeforeWindow: false,
        insertedRows: false,
      });

      const { getByTestId } = render(MessageTimeline, { props: { pane } });
      await fireEvent.click(getByTestId('load-newer-messages'));
      await tick();

      expect(loadNewerSpy).toHaveBeenCalledTimes(1);
    });

    it('scroll intents route through pane.loadUntilItem before touching the DOM', async () => {
      // Covers both directions of the windowed scroll contract:
      //   1) The pane publishes a requestScrollToItem nonce.
      //   2) MessageTimeline's $effect picks that up and calls
      //      pane.loadUntilItem first so the target is guaranteed in
      //      the window before scrollIntoView runs.
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'a', turnIndex: 5 })],
      });
      const loadSpy = vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(true);

      render(MessageTimeline, { props: { pane } });
      pane.requestScrollToItem('a');
      // Two ticks: one for the $effect to fire, one for the scrollToItem
      // awaits inside it to settle to the point where loadUntilItem was
      // called.
      await tick();
      await tick();

      expect(loadSpy).toHaveBeenCalledWith('a');
    });

    it('surfaces a warning toast when the scroll target no longer exists', async () => {
      const pane = await buildWindowedPane({
        items: [makeItem({ id: 'visible', turnIndex: 5 })],
      });
      vi.spyOn(pane, 'loadUntilItem').mockResolvedValue(false);
      const toastsBefore = getToasts().length;

      render(MessageTimeline, { props: { pane } });
      pane.requestScrollToItem('missing');
      await tick();
      await tick();

      const added = getToasts().slice(toastsBefore);
      expect(added.some((t) => t.type === 'warning')).toBe(true);
    });
  });

  describe('response divider integration', () => {
    it('renders a response divider before assistant text that follows tool activity', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'final answer',
        }),
      ]);

      const { getByTestId, container } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider).toBeInTheDocument();
      // The single assistant_text in this turn is also the final one,
      // and there's no active turn — so the pill is rendered. The
      // labeled and unlabeled branches share a pinned wrapper height,
      // so toggling between them doesn't shift row geometry.
      expect(divider.getAttribute('data-final-response')).toBe('true');
      expect(divider.textContent).toContain('Response');

      // Pin the reading order: divider sits BEFORE the assistant leaf.
      // The leaf is wrapped in a [data-item-id] div inside a
      // [data-testid="message-timeline-node"] wrapper; the divider
      // must appear in document order ahead of that wrapper.
      const assistantLeafWrapper = container.querySelector('[data-item-id="text:0:0"]');
      expect(assistantLeafWrapper).not.toBeNull();
      // Node-ordering compare: DOCUMENT_POSITION_FOLLOWING = 4.
      const following = divider.compareDocumentPosition(assistantLeafWrapper!) & 4;
      expect(following).toBe(4);
    });

    it('renders zero response dividers when assistant text follows user text directly', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({ id: 'text:0:0', kind: 'assistant_text', summary: 'hi' }),
      ]);

      const { container, queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('response-divider')).toHaveLength(0);
      // Pin the silent contract: a no-tool turn shows no Response cue
      // anywhere — neither a divider nor a stray pill marker.
      expect(container.querySelector('[data-final-response]')).toBeNull();
    });

    it('renders only one response divider for consecutive assistant text after tools', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'first paragraph',
        }),
        makeItem({
          id: 'text:0:1',
          itemIndex: 3,
          kind: 'assistant_text',
          summary: 'second paragraph',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('response-divider')).toHaveLength(1);
    });

    it('shows the "Response" pill only on the final wire round of a settled turn', async () => {
      // Two wire rounds inside one logical turn: each round ends with
      // assistant_text after a tool. Only the SECOND round's divider
      // should carry the "Response" pill — the first round's divider
      // is just a plain line.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'mid-turn observation',
        }),
        makeItem({
          id: 'tool:0:1',
          itemIndex: 3,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'cat README',
        }),
        makeItem({
          id: 'text:0:1',
          itemIndex: 4,
          kind: 'assistant_text',
          summary: 'final answer',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      const dividers = queryAllByTestId('response-divider');
      expect(dividers).toHaveLength(2);
      expect(dividers[0].getAttribute('data-final-response')).toBe('false');
      expect(dividers[0].textContent).not.toContain('Response');
      expect(dividers[1].getAttribute('data-final-response')).toBe('true');
      expect(dividers[1].textContent).toContain('Response');

      // Pin the structural shape of each branch: unlabeled mode is one
      // full-width line (one hairline span), labeled mode is two
      // (line | gap | pill | gap | line). A regression that swaps the
      // conditional or accidentally renders both flank lines without
      // the pill would leave the empty-divider void back in the UI.
      // (.timeline-hairline is the band-limited gradient rule — see
      // app.css — not a 1px background.)
      expect(dividers[0].querySelectorAll('span.timeline-hairline')).toHaveLength(1);
      expect(dividers[1].querySelectorAll('span.timeline-hairline')).toHaveLength(2);

      // Pin the geometry contract: both branches share the same
      // wrapper height class. Without this, the engine re-measures to a
      // different height when an intermediate divider promotes to
      // "final" on settle — exactly the bug the row contract forbids.
      for (const divider of dividers) {
        const inner = divider.querySelector('div');
        expect(inner?.classList.contains('h-[1.625rem]')).toBe(true);
      }
    });

    it('suppresses the "Response" pill while the turn is still in flight', async () => {
      const thread = makeThread();
      const pane = await buildPane(thread, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'streaming so far',
        }),
      ]);
      // Mark turn 0 as in flight: more rounds may yet arrive, so the
      // current "last assistant_text" is not necessarily final.
      projectTurnStarted(thread.id, 'turn-0', 0, Date.now());

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider.getAttribute('data-final-response')).toBe('false');
      expect(divider.textContent).not.toContain('Response');

      // Once the turn settles, the pill materialises on the SAME
      // divider element — no new rows inserted, no row shell mutation,
      // just the inner branch swapping the continuous line for the
      // labeled "line | gap | pill | gap | line" structure. The
      // wrapper's pinned height (h-[1.625rem]) keeps the row geometry
      // identical across the swap, protecting the load-bearing "no
      // late transcript adornments" contract that the chat row
      // contract spells out.
      projectTurnCompleted(thread.id, 'turn-0');
      await tick();
      const settledDivider = getByTestId('response-divider');
      expect(settledDivider).toBe(divider);
      expect(settledDivider.getAttribute('data-final-response')).toBe('true');
      expect(settledDivider.textContent).toContain('Response');
    });

    it('marks the final assistant_text of every settled turn in a multi-turn thread', async () => {
      // Two completed turns, each ending with an assistant_text after a
      // tool. Both turns are settled (no active turn entry), so each
      // should get a "Response" pill on its own final divider.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 0 final',
        }),
        makeItem({
          id: 'user:1',
          turnIndex: 1,
          itemIndex: 0,
          kind: 'user_text',
          role: 'user',
          summary: 'follow up',
        }),
        makeItem({
          id: 'tool:1:0',
          turnIndex: 1,
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'cat',
        }),
        makeItem({
          id: 'text:1:0',
          turnIndex: 1,
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 1 final',
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      const dividers = queryAllByTestId('response-divider');
      expect(dividers).toHaveLength(2);
      for (const divider of dividers) {
        expect(divider.getAttribute('data-final-response')).toBe('true');
        expect(divider.textContent).toContain('Response');
      }
    });

    it('adds elapsed time only to the latest settled response divider', async () => {
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi' }),
        makeItem({
          id: 'tool:0:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 0 final',
        }),
        makeItem({
          id: 'user:1',
          turnIndex: 1,
          itemIndex: 0,
          kind: 'user_text',
          role: 'user',
          summary: 'follow up',
        }),
        makeItem({
          id: 'tool:1:0',
          turnIndex: 1,
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Bash',
          summary: 'cat',
        }),
        makeItem({
          id: 'text:1:0',
          turnIndex: 1,
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'turn 1 final',
        }),
      ]);
      pane.settleTurn({
        turnId: 'turn-1',
        turnIndex: 1,
        startedAt: 1_000,
        completedAt: 65_000,
        stopReason: 'end_turn',
        assistantMessageId: 'provider-message-id',
        tokenUsage: null,
        aborted: false,
        errorMessage: '',
      });

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      const dividers = queryAllByTestId('response-divider');
      expect(dividers).toHaveLength(2);
      expect(dividers[0].textContent?.trim()).toBe('Response');
      expect(dividers[1].textContent).toContain('Response 1m 4s');
    });

    it('treats a subagent group as tool activity for the trailing pill', async () => {
      // Common Claude turn shape: user → inline Agent (subagent) →
      // assistant_text summary. The subagent group counts as tool
      // activity (`nodeRole(group) === 'tool'`), so the trailing
      // assistant_text gets a divider AND the Response pill — exactly
      // like a Bash-then-text turn.
      const pane = await buildPane(undefined, [
        makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'investigate' }),
        makeItem({
          id: 'agent:0',
          itemIndex: 1,
          kind: 'tool_call',
          toolName: 'Agent',
          summary: 'Agent: explore',
          meta: agentMeta('explore the auth module'),
        }),
        makeItem({
          id: 'text:0:0',
          itemIndex: 2,
          kind: 'assistant_text',
          summary: 'subagent finished — here is the answer',
        }),
      ]);

      const { getByTestId } = render(MessageTimeline, { props: { pane } });

      const divider = getByTestId('response-divider');
      expect(divider.getAttribute('data-final-response')).toBe('true');
      expect(divider.textContent).toContain('Response');
    });
  });

  describe('integration with utility helpers', () => {
    // The pure contracts live in `notificationFilter.test.ts` and
    // `subagentGrouping.test.ts`. The smoke tests below pin only the
    // wiring — that the filter is plumbed into the grouped-nodes derived,
    // and that the boundary classifier reaches the per-row wrapper class.

    it('drops a redundant task_notification from the rendered timeline', async () => {
      const pane = await buildPane(undefined, [
        makeItem({
          id: 'fg-1',
          itemIndex: 0,
          kind: 'tool_call',
          status: 'completed',
          toolName: 'Bash',
          summary: 'Bash: ls',
          meta: JSON.stringify({ task_id: 'T1' }),
        }),
        makeItem({
          id: 'task-notification:T1',
          itemIndex: 1,
          kind: 'notification',
          role: 'system',
          summary: 'Bash command "ls" completed',
          meta: JSON.stringify({ task_id: 'T1', source: 'task_notification' }),
        }),
      ]);

      const { queryAllByTestId } = render(MessageTimeline, { props: { pane } });

      expect(queryAllByTestId('notification-row')).toHaveLength(0);
    });

    it('applies the boundary mt-4 class to the per-row wrapper at a tool → text boundary', async () => {
      const pane = await buildPane(undefined, [
        makeItem({
          id: 'tool-1',
          itemIndex: 0,
          kind: 'tool_call',
          status: 'completed',
          toolName: 'Bash',
          summary: 'ls',
        }),
        makeItem({ id: 'text-1', itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
      ]);

      const { container } = render(MessageTimeline, { props: { pane } });

      const row = container.querySelector('[data-row-index="1"]');
      if (!row) throw new Error('row 1 not found');
      expect(row.classList.contains('mt-4')).toBe(true);
    });
  });

  describe('reveal gate', () => {
    function countNodes(scroll: HTMLElement): number {
      return scroll.querySelectorAll('[data-testid="message-timeline-node"]').length;
    }

    it('withholds the next row while the prior item streams, then reveals it', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'thread-1' }), [
          makeItem({ id: 'user:0', kind: 'user_text', role: 'user', summary: 'hi', turnIndex: 0, itemIndex: 0 }),
        ]);
        pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() });
        // A thinking row streams; the wire then moves on to a tool call while
        // the thinking smoother still has a backlog to reveal.
        pane.upsertItem(makeItem({
          id: 'think:0:1', kind: 'thinking', role: 'assistant', status: 'streaming',
          turnIndex: 0, itemIndex: 1, summary: '', payloadId: 'p', updatedAt: 1,
        }));
        pane.applyItemDelta({
          threadId: 'thread-1', itemId: 'think:0:1', kind: 'thinking',
          delta: 'word '.repeat(40), updatedAt: 2,
        });
        pane.upsertItem(makeItem({
          id: 'tool:0:2', kind: 'tool_call', role: 'assistant', status: 'running',
          turnIndex: 0, itemIndex: 2, toolName: 'Bash', summary: 'Bash command', updatedAt: 3,
        }));

        const { getByTestId } = render(MessageTimeline, { props: { pane } });
        await tick();

        const scroll = getByTestId('message-timeline-scroll');
        // user + thinking render; the tool call is withheld behind the
        // still-streaming thinking row.
        expect(countNodes(scroll)).toBe(2);
        expect(scroll.textContent).not.toContain('Bash command');

        // Drain the thinking smoother (fast-drain finishes within ~200ms) and
        // the gate drops — the tool call row reveals. Loop until the boundary
        // clears rather than a fixed frame count so the assertion proves "the
        // gate dropped", independent of the exact fast-drain constant.
        for (let i = 0; i < 40 && pane.revealBoundary !== null; i++) clock.tickFrame(16);
        await tick();

        expect(countNodes(scroll)).toBe(3);
        expect(scroll.textContent).toContain('Bash command');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });
});
