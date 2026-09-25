import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import AgentRow from './AgentRow.svelte';
import GenericToolCallRow from './GenericToolCallRow.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { createPayloadExpansion } from '../../utils/payloadExpansion.svelte';
import type { Item } from '../../types/models';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../../test/helpers/backends';
import { __resetEntityIndexForTest, noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
import { __resetRemoteJobsForTest } from '../../stores/remoteJobs.svelte';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';

// Minimal fake pane that satisfies the expansion-registry surface
// GenericToolCallRow reads from. Shared between tests that need a pane
// reference (workspacePath, expansion state).
function makeFakePane(extra: Partial<import('../../stores/thread.svelte').ThreadPane> = {}): import('../../stores/thread.svelte').ThreadPane {
  const cache = new Map<string, ReturnType<typeof createPayloadExpansion>>();
  return {
    expansionStateFor(item: Item) {
      const key = item.id;
      let h = cache.get(key);
      if (!h) {
        h = createPayloadExpansion(() => item.payloadId, () => item.threadId);
        cache.set(key, h);
      }
      return h;
    },
    ...extra,
  } as unknown as import('../../stores/thread.svelte').ThreadPane;
}

function expectBefore(left: Element, right: Element) {
  expect(left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
}

describe('<GenericToolCallRow> editor-link wiring', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('renders no editor-link when the input preview does not lead with a path', () => {
    const item = makeItem({ kind: 'tool_call', summary: 'Waiting on agents' });
    const { queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(queryByTestId('editor-link')).toBeNull();
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('renders the path preview itself as the editor link when the preview leads with a path', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    const link = getByTestId('editor-link');
    // Click target stays workspace-relative so OpenInEditor can resolve
    // it; displayed accessible name collapses to the basename.
    expect(link.getAttribute('data-path')).toBe('src/lib/foo.ts');
    expect(link.textContent).toBe('foo.ts:12');
    expect(link.getAttribute('title')).toBe('Open foo.ts:12 in editor');
    expect(queryByTestId('editor-link-icon')).toBeNull();
  });

  it('shows read file basenames without clipping the visible label or editor tooltip', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'Read: frontend/src/lib/components/chat/WaitGroup.svelte',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    const preview = getByTestId('tool-call-card-preview');
    const link = getByTestId('editor-link');
    expect(preview.textContent).toBe('WaitGroup.svelte');
    expect(preview.className).not.toContain('truncate');
    expect(preview.className).toContain('break-all');
    expect(link.getAttribute('data-path')).toBe('frontend/src/lib/components/chat/WaitGroup.svelte');
    expect(link.getAttribute('title')).toBe('Open WaitGroup.svelte in editor');
    expect(link.getAttribute('aria-label')).toBe('Open WaitGroup.svelte in editor');
  });

  it('keeps the editor-link usable on a non-expandable row', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    expect(getByTestId('tool-call-card-toggle')).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(getByTestId('editor-link'));

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '', '']);
  });

  it('clicking the editor-link does NOT toggle the row body', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
      payloadId: 'p-1',
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });

    expect(queryByTestId('tool-call-card-body')).toBeNull();
    const link = getByTestId('editor-link');
    await fireEvent.click(link);

    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '', '']);

    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('suppresses the dropdown for Read rows even when a payload exists', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Read',
      summary: 'src/lib/foo.ts',
      payloadId: 'p-read',
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item } });
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('tabindex', '-1');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('renders Skill rows with the skill name as preview and a disabled chevron', async () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Skill',
      summary: 'Skill',
      payloadId: 'p-skill',
      meta: JSON.stringify({ toolName: 'Skill', input: { skill: 'code-review' } }),
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-label').textContent).toBe('skill');
    expect(getByTestId('tool-call-card-preview').textContent).toBe('code-review');
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  // Regression for the original click-to-open bug: tool-result paths
  // are usually agent-emitted relative paths, which the backend used
  // to reject. The row threads `pane.thread.workspacePath` through to
  // EditorLink so the backend can join. Pin that the prop chain
  // forwards the value so a future refactor that drops the prop
  // wiring fails fast.
  it('forwards pane.thread.workspacePath to the OpenInEditor binding', async () => {
    const openMock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Read',
      summary: 'src/lib/foo.ts:12',
    });
    const pane = makeFakePane({
      thread: { workspacePath: '/home/user/repo' },
    } as Partial<import('../../stores/thread.svelte').ThreadPane>);
    const { getByTestId } = render(GenericToolCallRow, { props: { pane, item } });
    await fireEvent.click(getByTestId('editor-link'));
    await waitFor(() => {
      expect(openMock).toHaveBeenCalledTimes(1);
    });
    expect(openMock.mock.calls[0]).toEqual(['src/lib/foo.ts', 12, 0, '/home/user/repo', '']);
  });

  it('places the empty indicator slot before the timestamp', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Read',
      summary: 'README.md',
    });

    const { getByTestId } = render(GenericToolCallRow, { props: { item } });

    expectBefore(getByTestId('tool-call-card-status-slot'), getByTestId('tool-call-card-time'));
  });

  it('renders the backgrounded indicator state', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      summary: 'long-running task',
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    const status = getByTestId('tool-call-card-status');
    const indicator = status.querySelector('[data-testid="indicator"]');
    expect(indicator?.getAttribute('data-state')).toBe('backgrounded');
    expect(indicator?.getAttribute('aria-label')).toBe('Backgrounded');
  });

  it('suppresses the live duration ticker on a backgrounded launch row so chat history shows only a timestamp', () => {
    // Backgrounded launches in chat are stable transcript records.
    // A wall-clock ticker that keeps counting up indefinitely is
    // misleading there — the user can't act on it from a chat row.
    // Live status lives in the tray (which passes its own
    // durationLabel; that path is exercised by BackgroundTaskTrayRow).
    // The eventual completion lands as its own sibling row with its
    // own duration. Use a generous 60s offset so the assertion stays
    // unambiguously past the 2s `RUNNING_ELAPSED_THRESHOLD_MS` gate
    // — a future bump of that threshold won't silently re-mean this
    // test as "ticker is empty in the gate window."
    const sixtySecondsAgo = Date.now() - 60_000;
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Bash',
      summary: 'long-running task',
      createdAt: sixtySecondsAgo,
      updatedAt: sixtySecondsAgo,
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    // Duration slot is reserved (always present in the DOM for layout
    // stability — see ToolHeaderMeta) but the label is empty because
    // shouldTickElapsed is gated off for backgrounded launches.
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).toBe('');
    // Timestamp still renders so the row is anchored in time.
    expect(getByTestId('tool-call-card-time')).toBeInTheDocument();
  });

  it('ticks the live duration on a non-backgrounded running tool (contrast for the suppression above)', () => {
    // Same wall-clock as the backgrounded test, no isBackground flag.
    // This locks in that the timer suppression is gated by
    // isBackgroundedLaunch and not an accidental wider regression.
    const sixtySecondsAgo = Date.now() - 60_000;
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'foreground bash',
      createdAt: sixtySecondsAgo,
      updatedAt: sixtySecondsAgo,
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).not.toBe('');
  });

  it('renders a Claude Agent row with the title-cased subagent_type label and the model affix', () => {
    // Without this header treatment a backgrounded Agent renders the
    // bare "Subagent" classifier label with no model — completely
    // different from the `SubagentGroup` card a foreground
    // Agent gets. Matching what `SubagentGroup` does here is what
    // keeps the two surfaces visually aligned.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: Review: security',
      meta: JSON.stringify({ subagent_model: 'claude-opus-4-7' }),
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'Review: security' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    // Preview shows just the description, not the "Agent: …" summary
    // line the generic preview formatter would otherwise emit.
    const preview = getByTestId('agent-row-preview');
    expect(getByTestId('agent-row-label').textContent).toBe('agent');
    expect(preview.textContent).toContain('Explore');
    expect(preview.textContent).toContain('Opus 4.7');
    expect(preview.textContent).toContain('Review: security');
    expect(preview.textContent).not.toContain('Agent:');
  });

  it('falls back to the launch input.model when no subagent_model is stamped on a backgrounded Agent yet', () => {
    // Brief window between launch and the subagent's first assistant
    // envelope — the parser hasn't stamped parent.meta.subagent_model
    // yet, but the user-supplied input.model alias should still drive
    // the affix so the row never renders without a model.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: launching',
      // No meta.subagent_model.
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'Just launched', model: 'opus' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    expect(getByTestId('agent-row-preview').textContent).toContain('Opus');
  });

  it('treats legacy Task rows as Claude Agent rows for label and model derivation', () => {
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Task',
      summary: 'Task: launching',
      payloadMeta: JSON.stringify({
        toolName: 'Task',
        input: { subagent_type: 'Explore', description: 'Legacy task launch', model: 'opus' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    const preview = getByTestId('agent-row-preview').textContent ?? '';
    expect(preview).toContain('Explore');
    expect(preview).toContain('Opus');
    expect(preview).toContain('Legacy task launch');
  });

  it('falls back to "Agent" (not "Subagent") when subagent_type is missing on an Agent row', () => {
    // Without the `.not.toContain('Subagent')` clause, this assertion
    // would also pass if the row regressed to the classifier label
    // "Subagent" (which itself contains "Agent"). Pin both directions.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: do something',
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { description: 'do something' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    const preview = getByTestId('agent-row-preview');
    expect(preview.textContent).toContain('Agent');
    expect(preview.textContent).not.toContain('Subagent');
  });

  it('falls through to the generic preview when an Agent row has no description or prompt', () => {
    // The `isClaudeAgent && subagentDescription` guard on inputPreview
    // must NOT swallow the row when description+prompt are both empty
    // — otherwise the preview would render as ''. Without this test a
    // future regression that drops the `&& subagentDescription` guard
    // would silently blank the preview.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: fallback preview',
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    // Label still renders as "Explore" (the subagent_type), and the
    // preview falls through to presentToolCardInputPreview, which
    // strips the redundant `Agent: ` prefix that triage embeds in
    // item.summary (the gutter label already says "agent").
    const previewText = getByTestId('agent-row-preview').textContent ?? '';
    expect(previewText).toContain('Explore');
    expect(previewText).toContain('fallback preview');
    expect(previewText).not.toContain('Agent: fallback preview');
  });

  it('suppresses the dropdown for TaskOutput rows even when a payload exists', async () => {
    // TaskOutput retrieves the same stdout already shown on the
    // originating Bash row, so the row keeps the stable header shell but
    // has no expandable body.
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'TaskOutput',
      summary: 'TaskOutput',
      payloadId: 'p-task-output',
    });
    const { queryByTestId, getByTestId } = render(GenericToolCallRow, { props: { item } });
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    expect(toggle).toHaveAttribute('tabindex', '-1');
    await fireEvent.click(toggle);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('renders header-only with no expandable transcript body (agent-visibility)', async () => {
    // The old ack-text body that re-parsed the payload JSONL into a
    // pseudo-transcript is deleted: the agent's transcript is real rows
    // under the launch now (card body + agent pane). The leaf row is a
    // non-expandable header.
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Agent',
      summary: 'Agent: worker -> done',
      payloadId: 'agent-jsonl',
    });
    const { getByTestId, queryByTestId } = render(AgentRow, {
      props: { item },
    });

    const toggle = getByTestId('agent-row-toggle');
    expect(toggle).toHaveAttribute('aria-disabled', 'true');
    await fireEvent.click(toggle);
    expect(queryByTestId('claude-subagent-transcript')).toBeNull();
    expect(queryByTestId('expandable-payload-body')).toBeNull();
  });

  it('renders a completion leaf with its launch identity and opens the pane on the launch', async () => {
    // The completion sibling is the row that says "this agent finished" at
    // the point it finished. It reads as the SAME agent the card named
    // (launch label/description, completion status) and carries the card's
    // open-in-pane door, keyed on the launch id, so the reader does not
    // have to scroll back to the card.
    const launch = makeItem({
      id: 'agent-1',
      kind: 'tool_call',
      status: 'completed',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: worker',
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'find the leak' },
      }),
    });
    const completion = makeItem({
      id: 'complete:agent-1',
      itemIndex: 9,
      kind: 'tool_completion',
      status: 'completed',
      isBackground: true,
      toolName: 'Agent',
      completionOf: 'agent-1',
      summary: 'Agent: worker -> done',
    });
    const opened: [string, string][] = [];
    const pane = makeFakePane({
      getItemById: (id: string) => (id === 'agent-1' ? launch : undefined),
      openAgentPane: (id: string, label: string) => {
        opened.push([id, label]);
      },
    } as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const { getByTestId } = render(AgentRow, { props: { pane, item: completion } });

    const preview = getByTestId('agent-row-preview').textContent ?? '';
    expect(preview).toContain('Explore');
    expect(preview).toContain('find the leak');

    await fireEvent.click(getByTestId('agent-row-open-pane'));
    expect(opened).toEqual([['agent-1', 'Explore']]);
  });

  it('opens a §E6 resume carrier on the ORIGINAL launch, keeping its own label', async () => {
    // Only the task lifecycle rebinds onto the carrier: every round's rows
    // stay parented to the original launch, so scoping the pane to the
    // carrier would open an empty transcript.
    const carrier = makeItem({
      id: 'toolu_resume',
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'SendMessage',
      summary: 'Agent: audit the parser',
      meta: JSON.stringify({
        task_id: 'a1',
        transcript_root_id: 'toolu_root',
        subagent_type: 'general-purpose',
      }),
    });
    const opened: [string, string][] = [];
    const pane = makeFakePane({
      getItemById: () => undefined,
      openAgentPane: (id: string, label: string) => {
        opened.push([id, label]);
      },
    } as Partial<import('../../stores/thread.svelte').ThreadPane>);

    const { getByTestId } = render(AgentRow, { props: { pane, item: carrier } });
    await fireEvent.click(getByTestId('agent-row-open-pane'));

    expect(opened).toEqual([['toolu_root', 'General Purpose']]);
  });

  it('offers no open-in-pane door without a pane to route through', () => {
    const item = makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Agent',
      completionOf: 'agent-1',
      summary: 'Agent: worker -> done',
    });
    const { queryByTestId } = render(AgentRow, { props: { item } });
    expect(queryByTestId('agent-row-open-pane')).toBeNull();
  });

  it('surfaces a failed output-file read as an inline error line', () => {
    // triage stamps notification_output_state/error on the launch row when
    // the task_notification's output_file could not be read; a silently
    // incomplete transcript reads exactly like a complete one, so the row
    // must say so.
    const item = makeItem({
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: worker',
      meta: JSON.stringify({
        notification_output_state: 'error',
        notification_output_error: 'output file vanished before read',
      }),
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'worker' },
      }),
    });
    const { getByTestId } = render(AgentRow, { props: { item } });
    expect(getByTestId('agent-row-output-error').textContent).toContain(
      'output file vanished before read',
    );
  });
});

// A background launch row is history: the one change it shows after it is
// written is its `backgrounded` dots turning off once the store settles the
// launch, their box kept so nothing on the row moves
// (docs/specs/agent-visibility.md#immutable-agent-history).
describe('<AgentRow> background launch indicator', () => {
  function launch(meta: Record<string, unknown> = {}): Item {
    return makeItem({
      id: 'toolu_bg',
      kind: 'tool_call',
      status: 'running',
      isBackground: true,
      toolName: 'Agent',
      summary: 'Agent: gate watcher',
      createdAt: 1_000,
      meta: JSON.stringify({ task_id: 'task-bg', subagent_model: 'claude-opus-4-7', ...meta }),
      payloadMeta: JSON.stringify({
        toolName: 'Agent',
        input: { subagent_type: 'Explore', description: 'gate watcher', run_in_background: true },
      }),
    });
  }
  const doorPane = () => makeFakePane({
    getItemById: () => undefined,
    openAgentPane: () => {},
  } as Partial<import('../../stores/thread.svelte').ThreadPane>);

  function indicator(container: HTMLElement): string | null {
    return container.querySelector('[data-testid="agent-row-status"] [data-testid="indicator"]')?.getAttribute('data-state') ?? null;
  }

  it('shows the dots while the launch is live and the settled box once its stored bit settles it', () => {
    for (const live of [launch(), launch({ live_background_active: true })]) {
      const view = render(AgentRow, { props: { pane: doorPane(), item: live } });
      expect(indicator(view.container)).toBe('backgrounded');
      view.unmount();
    }
    const settled = render(AgentRow, { props: { pane: doorPane(), item: launch({ live_background_active: false }) } });
    expect(indicator(settled.container)).toBe('settled');
    expect(settled.getByTestId('agent-row-status')).toHaveAttribute('data-state', 'settled');
    expect(settled.queryByRole('status')).toBeNull();
    expect(settled.queryByTestId('agent-row-error')).toBeNull();
    expect(settled.getByTestId('agent-row-duration').textContent?.trim()).toBe('');
  });

  it('keeps a parked agent’s launch row on its dots', async () => {
    // A parked stop does not settle the launch: the store leaves the bit
    // set, and the launch row never reads the parked sibling.
    const row = launch({ live_background_active: true });
    const parked = makeItem({
      id: 'complete:toolu_bg:parked:u1',
      itemIndex: 3,
      kind: 'tool_completion',
      status: 'parked',
      isBackground: true,
      toolName: 'Agent',
      completionOf: 'toolu_bg',
      createdAt: 61_000,
      meta: JSON.stringify({ task_id: 'task-bg', parked_commands: 1 }),
    });
    const pane = await buildPane(undefined, [row, parked]);
    expect(pane.getItemById(parked.id)).toBeDefined();
    const { container } = render(AgentRow, { props: { pane, item: pane.getItemById(row.id) ?? row } });
    expect(indicator(container)).toBe('backgrounded');
  });

  /**
   * Every difference between two DOM trees of the same shape: element
   * attributes (classes as added/removed sets) and text. An element is
   * named by its test id, else its tag.
   */
  function domDiff(before: Element, after: Element): string[] {
    const at = after.getAttribute('data-testid') ?? after.tagName.toLowerCase();
    if (before.tagName !== after.tagName) return [`${at}: <${before.tagName}> became <${after.tagName}>`];
    const out: string[] = [];
    const names = [...new Set([...before.getAttributeNames(), ...after.getAttributeNames()])].sort();
    for (const name of names) {
      if (name === 'class') {
        const was = new Set(before.classList);
        const now = new Set(after.classList);
        const removed = [...was].filter((c) => !now.has(c)).map((c) => `-${c}`);
        const added = [...now].filter((c) => !was.has(c)).map((c) => `+${c}`);
        if (removed.length || added.length) out.push(`${at} class ${[...removed, ...added].join(' ')}`);
      } else if (before.getAttribute(name) !== after.getAttribute(name)) {
        out.push(`${at} ${name}: ${before.getAttribute(name)} -> ${after.getAttribute(name)}`);
      }
    }
    const was = [...before.childNodes];
    const now = [...after.childNodes];
    if (was.length !== now.length) return [...out, `${at}: ${was.length} children became ${now.length}`];
    was.forEach((node, i) => {
      const other = now[i];
      if (node.nodeType !== other.nodeType) out.push(`${at}: child ${i} changed type`);
      else if (node instanceof Element) out.push(...domDiff(node, other as Element));
      else if (node.textContent !== other.textContent) out.push(`${at}: text ${node.textContent} -> ${other.textContent}`);
    });
    return out;
  }

  // The compact layout is a class on <html> that only stylesheets read; the
  // markup must be the same in both layouts, and so must the diff.
  it.each([false, true])('moves nothing on the row when the launch settles (compact: %s)', async (compact) => {
    document.documentElement.classList.toggle('layout-compact', compact);
    try {
      const { container, rerender, getByTestId } = render(AgentRow, {
        props: { pane: doorPane(), item: launch({ live_background_active: true }) },
      });
      const row = getByTestId('agent-row');
      const box = row.querySelector('[data-testid="agent-row-status"] [data-testid="indicator"]')!;
      const dots = [...box.children];
      expect(dots).toHaveLength(3);
      const before = container.cloneNode(true) as Element;

      await rerender({ pane: doorPane(), item: launch({ live_background_active: false }) });

      // The same nodes stay mounted, and the only differences are the
      // dots' visibility and the state the indicator reports.
      expect(getByTestId('agent-row')).toBe(row);
      expect(row.querySelector('[data-testid="agent-row-status"] [data-testid="indicator"]')).toBe(box);
      expect([...box.children]).toEqual(dots);
      expect(domDiff(before, container)).toEqual([
        'agent-row-status data-state: backgrounded -> settled',
        'indicator aria-hidden: null -> true',
        'indicator aria-label: Backgrounded -> null',
        'indicator data-state: backgrounded -> settled',
        'indicator role: status -> null',
        'span class -animate-pulse +invisible',
        'span class -animate-pulse +invisible',
        'span class -animate-pulse +invisible',
      ]);
    } finally {
      document.documentElement.classList.remove('layout-compact');
    }
  });
});

describe('<GenericToolCallRow> empty-body message', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  // A row that advertises a payload whose expansion resolves none lands in
  // ExpandablePayloadBody's empty branch — where an imported item whose tool
  // output the provider CLI garbage-collected also lands.
  function renderWithUnresolvedPayload(item: Item) {
    const handle = createPayloadExpansion(() => undefined, () => item.threadId);
    const pane = { expansionStateFor: () => handle } as unknown as import('../../stores/thread.svelte').ThreadPane;
    return render(GenericToolCallRow, { props: { pane, item } });
  }

  it('uses the row default when nothing marks the item as an import casualty', async () => {
    const { getByTestId } = renderWithUnresolvedPayload(makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Bash',
      payloadId: 'p-missing',
    }));

    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await waitFor(() => {
      expect(getByTestId('tool-call-card-body').textContent?.trim())
        .toBe('No stored payload for this tool result.');
    });
  });

  it('says the payload did not come across when the importer stamped the item', async () => {
    const { getByTestId } = renderWithUnresolvedPayload(makeItem({
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Bash',
      payloadId: 'p-missing',
      meta: JSON.stringify({ import_unavailable: 'tool-output-gc' }),
    }));

    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    await waitFor(() => {
      expect(getByTestId('tool-call-card-body').textContent?.trim())
        .toBe('Not available from import.');
    });
  });
});

describe('<GenericToolCallRow> permission-denial chip', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('shows Declined and carries the denial reason on the chip', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Bash',
      summary: 'rm -rf /',
      decision: 'declined',
      meta: JSON.stringify({
        toolName: 'Bash',
        permissionDenied: {
          reason: 'Denied by alwaysDenyRules: Bash(rm:*)',
          reasonType: 'rule',
        },
      }),
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    const chip = getByTestId('tool-decision-chip');
    expect(chip.textContent).toContain('Declined');
    expect(chip.getAttribute('title')).toBe('Denied by alwaysDenyRules: Bash(rm:*)');
  });

  it('leaves the chip title unset when the row carries no denial meta', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Bash',
      summary: 'ls',
      decision: 'declined',
      meta: JSON.stringify({ toolName: 'Bash' }),
    });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-decision-chip').getAttribute('title')).toBeNull();
  });
});

// A browser tool drives a real page. On the owner's own screen that page is
// the companion browser and the row says nothing extra. Read from anywhere
// else, the row is the only sign the page exists at all, so it names the
// machine — otherwise "browser_click" reads as something that happened here.
describe('<GenericToolCallRow> browser tools on another machine', () => {
  const THREAD = 'thread-on-laptop';

  function browserItem(server = BROWSER_TOOLS_SERVER): Item {
    return makeItem({
      id: 'tool-browser',
      kind: 'tool_call',
      toolName: 'MCP/browser_click',
      summary: 'browser_click',
      meta: JSON.stringify({ mcp: { server, tool: 'browser_click' } }),
    });
  }

  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    __resetEntityIndexForTest();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('names the machine on a row whose thread runs somewhere else', () => {
    stageBackend();
    noteThread(THREAD, 'laptop');
    const { getByTestId } = render(GenericToolCallRow, {
      props: { pane: makeFakePane({ threadId: THREAD }), item: browserItem() },
    });

    const where = getByTestId('tool-call-card-where');
    expect(where.getAttribute('title')).toBe(
      'Browsing on Laptop. The page is only visible there.',
    );
    expect(where.dataset.machine).toBe('Laptop');
    expect(where.textContent).toContain('Laptop');
  });

  it('says nothing on the machine the page is actually on', () => {
    const { queryByTestId } = render(GenericToolCallRow, {
      props: { pane: makeFakePane({ threadId: 'thread-at-home' }), item: browserItem() },
    });
    expect(queryByTestId('tool-call-card-where')).toBeNull();
  });

  it('says nothing for an unrelated MCP server on the same machine', () => {
    stageBackend();
    noteThread(THREAD, 'laptop');
    const { queryByTestId } = render(GenericToolCallRow, {
      props: { pane: makeFakePane({ threadId: THREAD }), item: browserItem('docs') },
    });
    expect(queryByTestId('tool-call-card-where')).toBeNull();
  });

  it('keeps the row actions the host passed alongside it', () => {
    stageBackend({ hello: { capabilities: ['browser'] } as never, backendId: REMOTE_BACKEND_UUID });
    noteThread(THREAD, 'laptop');
    const { getByTestId } = render(GenericToolCallRow, {
      props: { pane: makeFakePane({ threadId: THREAD }), item: browserItem() },
    });
    expect(getByTestId('tool-call-card-where')).toBeTruthy();
    expect(getByTestId('tool-call-card-status-slot')).toBeTruthy();
  });
});

// A tool AO serves itself presents as a proper tool call: the family icon,
// a verb in the gutter, the argument that matters, the computer it acts on
// after it, with the wire tool name kept for a hover. Ids the model used
// read as what they name: a computer, a job, a page.
describe('<GenericToolCallRow> AO tools', () => {
  const THREAD = 'thread-remote';
  const REQUEST = '98312d67-2222-4222-8222-222222222222';
  const record = (receipt: Record<string, unknown>, extra: Record<string, unknown> = {}) => ({
    computerId: 'far', computerName: 'Macaroni-air', requestId: REQUEST, threadId: THREAD,
    label: 'Go tests', command: 'go test ./...', notification: 'pending', createdAt: 1,
    receipt: { id: REQUEST, sourceThreadId: THREAD, state: 'running', startedAt: 1000, exitCode: -1, ...receipt },
    ...extra,
  });

  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    __resetEntityIndexForTest();
    __resetRemoteJobsForTest();
    resetBrowserCompanionForTest();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => []));
    noteThread(THREAD, HOME_BACKEND);
  });

  function remoteTool(tool: string, input: Record<string, unknown>, overrides: Partial<Item> = {}): Item {
    return makeItem({
      id: 'tool-remote',
      threadId: THREAD,
      kind: 'tool_call',
      toolName: `MCP/${tool}`,
      summary: `MCP/${tool}: …`,
      meta: JSON.stringify({ mcp: { server: 'ao-remote-tools', tool }, input }),
      ...overrides,
    });
  }
  const remoteRun = (input: Record<string, unknown>, overrides: Partial<Item> = {}) => remoteTool('remote_run', input, overrides);

  it('shows the command, then the attached computer it runs on', () => {
    stageBackend();
    const { getByTestId } = render(GenericToolCallRow, {
      props: { item: remoteRun({ computer_id: 'laptop', argv: ['go', 'test', './...'] }) },
    });
    expect(getByTestId('tool-call-card').dataset.toolKind).toBe('monitor');
    expect(getByTestId('tool-call-card-label').textContent).toBe('run');
    expect(getByTestId('tool-call-card-label').getAttribute('title')).toBe('remote_run');
    const where = getByTestId('tool-call-card-where');
    expect(where.dataset.machine).toBe('Laptop');
    expect(where.textContent).toBe('(Laptop)');
    expect(where.getAttribute('title')).toBe('On Laptop');
    expectBefore(getByTestId('tool-call-card-preview'), where);
    expect(getByTestId('tool-call-card-preview').textContent).toBe('go test ./...');
  });

  it('still tells two computers apart by a short id when nobody here can name one', () => {
    const { getByTestId } = render(GenericToolCallRow, {
      props: { item: remoteRun({ computer_id: 'c0ffee11-1111-4111-8111-111111111111', argv: ['make'] }) },
    });
    const where = getByTestId('tool-call-card-where');
    expect(where.textContent).toBe('(computer c0ffee11)');
    expect(where.getAttribute('title')).toContain('not paired');
    expect(getByTestId('tool-call-card-preview').textContent).toBe('make');
  });

  it("names the computer and the job from the thread's job listing", async () => {
    const list = setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record({ state: 'succeeded', finishedAt: 4000, exitCode: 0 })]));
    const { getByTestId } = render(GenericToolCallRow, {
      props: { item: remoteTool('remote_search_log', { computer_id: 'far', request_id: REQUEST, query: 'FAIL' }) },
    });
    await waitFor(() => expect(getByTestId('tool-call-card-preview').textContent).toBe('"FAIL" in Go tests'));
    expect(list).toHaveBeenCalledWith(THREAD);
    expect(getByTestId('tool-call-card-where').textContent).toBe('(Macaroni-air)');
  });

  it('reads a returned remote_run as backgrounded while its job runs, then as the job\'s own outcome', async () => {
    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record({})]));
    const item = remoteRun({ computer_id: 'far', request_id: REQUEST, argv: ['go', 'test', './...'] }, {
      status: 'completed', createdAt: Date.now() - 60_000,
      payloadMeta: JSON.stringify({ durationMs: 300_000 }),
    });
    const { getByTestId, queryByText } = render(GenericToolCallRow, { props: { item } });
    // Before the listing answers, the row is the completed five-minute call it is.
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).toBe('5m 0s');
    const indicator = () => getByTestId('tool-call-card-status-slot').querySelector('[data-testid="indicator"]');
    await waitFor(() => expect(indicator()?.getAttribute('data-state')).toBe('backgrounded'));
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).toBe('');

    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record({ state: 'canceled', finishedAt: 4000 })]));
    emitWailsEvent('provider:background_tasks_changed', { threadId: THREAD });
    await waitFor(() => expect(indicator()?.getAttribute('data-state')).toBe('error'));
    expect(queryByText('Remote job stopped')).not.toBeNull();
    expect(getByTestId('tool-call-card-duration').textContent?.trim()).toBe('3.0s');

    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => [record({ state: 'failed', finishedAt: 4000, exitCode: 2, error: 'tests failed' })]));
    emitWailsEvent('provider:background_tasks_changed', { threadId: THREAD });
    await waitFor(() => expect(queryByText('tests failed')).not.toBeNull());
    expect(queryByText('exit 2')).not.toBeNull();
  });

  it('keeps a job in another thread\'s listing out of this row', async () => {
    setBindingMock('ListThreadRemoteCommands', vi.fn(async () => []));
    const item = remoteRun({ computer_id: 'far', request_id: REQUEST, argv: ['make'] }, { status: 'completed' });
    const { getByTestId } = render(GenericToolCallRow, { props: { item } });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(getByTestId('tool-call-card-status-slot').querySelector('[data-testid="indicator"]')).toBeNull();
  });

  it('opens to the full text the header clipped and the inputs it left out', async () => {
    stageBackend();
    const long = ['bash', '-lc', `for i in ${Array.from({ length: 40 }, (_, i) => `step${i}`).join(' ')}; do echo $i; done`];
    const item = remoteRun({ computer_id: 'laptop', project_id: 'proj-uuid', request_id: REQUEST, argv: long, timeout_seconds: 600 });
    const { getByTestId, queryByTestId, getAllByTestId } = render(GenericToolCallRow, { props: { item } });
    const toggle = getByTestId('tool-call-card-toggle');
    expect(toggle).not.toHaveAttribute('aria-disabled', 'true');
    expect(getByTestId('tool-call-card-preview').textContent?.endsWith('…')).toBe(true);
    await fireEvent.click(toggle);
    const full = getByTestId('tool-call-card-ao-full');
    expect(full.textContent).toContain('step39; do echo $i; done');
    const facts = getAllByTestId('tool-call-card-ao-fact').map((el) => [el.dataset.fact, el.textContent]);
    expect(facts).toEqual([['computer', 'Laptop'], ['timeout', '600']]);
    expect(queryByTestId('tool-call-card-body')).toBeNull();
  });

  it('renders a remote reply as its outcome and output rather than the JSON the model read', async () => {
    const reply = JSON.stringify({ id: REQUEST, computerId: 'far', state: 'succeeded', exitCode: 0, startedAt: 1000, finishedAt: 13500, output: 'ok  \tagent-overflow/internal/app\n' });
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: reply, size: reply.length, isComplete: true })));
    const item = remoteRun({ computer_id: 'far', request_id: REQUEST, argv: ['go', 'test'] }, { status: 'completed', payloadId: 'p-remote' });
    const { getByTestId, findByTestId } = render(GenericToolCallRow, { props: { item } });
    await fireEvent.click(getByTestId('tool-call-card-toggle'));
    expect((await findByTestId('tool-call-card-remote-outcome')).textContent).toBe('succeeded · exit 0 · 12.5s');
    expect(getByTestId('tool-call-card-output').textContent).toContain('agent-overflow/internal/app');
    expect(getByTestId('tool-call-card-output').textContent).not.toContain('"state"');
  });

  it('presents a browser tool by verb and target, naming a page by its label', () => {
    applyBrowserCompanionState({
      kind: 'state', threadId: THREAD,
      pages: [{ id: 'page-1', label: 'Checkout', url: 'https://shop.test/checkout', title: 'Cart', canGoBack: false, canGoForward: false }],
    });
    const click = makeItem({
      id: 'tool-click',
      threadId: THREAD,
      kind: 'tool_call',
      toolName: 'MCP/browser_click',
      meta: JSON.stringify({ mcp: { server: BROWSER_TOOLS_SERVER, tool: 'browser_click' }, input: { selector: '#submit' } }),
    });
    const clicked = render(GenericToolCallRow, { props: { item: click } });
    expect(clicked.getByTestId('tool-call-card').dataset.toolKind).toBe('globe');
    expect(clicked.getByTestId('tool-call-card-label').textContent).toBe('click');
    expect(clicked.getByTestId('tool-call-card-preview').textContent).toBe('#submit');
    clicked.unmount();
    const select = makeItem({
      id: 'tool-select',
      threadId: THREAD,
      kind: 'tool_call',
      toolName: 'MCP/browser_select_page',
      meta: JSON.stringify({ mcp: { server: BROWSER_TOOLS_SERVER, tool: 'browser_select_page' }, input: { page_id: 'page-1' } }),
    });
    const selected = render(GenericToolCallRow, { props: { item: select } });
    expect(selected.getByTestId('tool-call-card-preview').textContent).toBe('Checkout');
  });
});

describe('<GenericToolCallRow> SendMessage ack line', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('GetPayloadPreview', vi.fn(async () => ({ data: '', size: 0, isComplete: true })));
  });

  it('shows the CLI reply as the row error when the send was refused', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'SendMessage',
      status: 'errored',
      summary: 'SendMessage: A (error)',
      meta: JSON.stringify({
        input: { to: 'A', message: 'status?' },
        is_error: true,
        send_reply: 'No agent named "A" in this session.',
      }),
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('row-error-msg').textContent).toBe('No agent named "A" in this session.');
    expect(queryByTestId('tool-call-card-reply')).toBeNull();
  });

  it('shows the CLI reply as a muted line under a delivered send', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'SendMessage',
      status: 'completed',
      summary: 'SendMessage: ab487a02304913d06',
      meta: JSON.stringify({
        input: { to: 'ab487a02304913d06', message: 'status?' },
        send_reply: 'Message queued for delivery to ab487a02304913d06 at its next tool round.',
        recipient_description: 'Frontend fix',
      }),
    });
    const { getByTestId, queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(getByTestId('tool-call-card-reply').textContent).toBe(
      'Message queued for delivery to ab487a02304913d06 at its next tool round.',
    );
    expect(queryByTestId('row-error')).toBeNull();
  });

  it('renders no reply line for a non-SendMessage row that happens to carry one', () => {
    const item = makeItem({
      kind: 'tool_call',
      toolName: 'Bash',
      status: 'completed',
      summary: 'Bash: ls',
      meta: JSON.stringify({ input: { command: 'ls' }, send_reply: 'stray' }),
    });
    const { queryByTestId } = render(GenericToolCallRow, { props: { item } });
    expect(queryByTestId('tool-call-card-reply')).toBeNull();
  });
});
