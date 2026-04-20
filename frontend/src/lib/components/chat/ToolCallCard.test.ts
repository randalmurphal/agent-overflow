import { beforeEach, describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import ToolCallCard from './ToolCallCard.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

beforeEach(() => {
  resetBindingMocks();
  // Payload fetches are triggered when the user expands the body; the tests
  // below never click the toggle, so the mocks just need to exist for the
  // chevron path to not blow up if something races.
  setBindingMock('GetPayloadPreview', async () => ({
    data: '',
    totalSize: 0,
    isComplete: true,
  }));
  setBindingMock('GetPayloadData', async () => '');
});

describe('<ToolCallCard> header dispatcher', () => {
  it('renders a terminal icon + "Bash" label for a Bash tool call', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'ls -la',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    const card = getByTestId('tool-call-card');
    expect(card.getAttribute('data-tool-kind')).toBe('terminal');
    expect(getByTestId('tool-call-card-label').textContent).toBe('Bash');
    expect(getByTestId('tool-call-card-preview').textContent).toContain('ls -la');
  });

  it('renders a file icon for Edit/Write/MultiEdit tools', async () => {
    const pane = await buildPane();
    for (const toolName of ['Edit', 'Write', 'MultiEdit']) {
      const item = makeItem({
        id: `tool-${toolName}`,
        kind: 'tool_call',
        status: 'running',
        toolName,
        summary: 'foo.ts',
      });

      const { getByTestId, unmount } = render(ToolCallCard, { props: { pane, item } });

      expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('file');
      expect(getByTestId('tool-call-card-label').textContent).toBe(toolName);
      unmount();
    }
  });

  it('renders an eye icon for Read', async () => {
    const pane = await buildPane();
    const item = makeItem({ id: 'r', kind: 'tool_call', status: 'running', toolName: 'Read' });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('eye');
    expect(getByTestId('tool-call-card-label').textContent).toBe('Read');
  });

  it('renders a search icon for Grep and Glob', async () => {
    const pane = await buildPane();
    for (const toolName of ['Grep', 'Glob']) {
      const item = makeItem({ id: toolName, kind: 'tool_call', status: 'running', toolName });
      const { getByTestId, unmount } = render(ToolCallCard, { props: { pane, item } });
      expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('search');
      expect(getByTestId('tool-call-card-label').textContent).toBe(toolName);
      unmount();
    }
  });

  it('renders a globe icon for WebFetch and WebSearch', async () => {
    const pane = await buildPane();
    for (const toolName of ['WebFetch', 'WebSearch']) {
      const item = makeItem({ id: toolName, kind: 'tool_call', status: 'running', toolName });
      const { getByTestId, unmount } = render(ToolCallCard, { props: { pane, item } });
      expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('globe');
      unmount();
    }
  });

  it('renders a robot icon + "Subagent" label for Task and collab_agent', async () => {
    const pane = await buildPane();
    for (const toolName of ['Task', 'collab_agent']) {
      const item = makeItem({ id: toolName, kind: 'tool_call', status: 'running', toolName });
      const { getByTestId, unmount } = render(ToolCallCard, { props: { pane, item } });
      expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('robot');
      expect(getByTestId('tool-call-card-label').textContent).toBe('Subagent');
      unmount();
    }
  });

  it('renders a speech-bubble icon for send_input', async () => {
    const pane = await buildPane();
    const item = makeItem({ id: 's', kind: 'tool_call', status: 'running', toolName: 'send_input' });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('speech-bubble');
  });

  it('renders a checklist icon for Plan / ExitPlanMode', async () => {
    const pane = await buildPane();
    for (const toolName of ['Plan', 'ExitPlanMode']) {
      const item = makeItem({ id: toolName, kind: 'tool_call', status: 'running', toolName });
      const { getByTestId, unmount } = render(ToolCallCard, { props: { pane, item } });
      expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('checklist');
      unmount();
    }
  });

  it('renders a puzzle icon for MCP/<tool> and preserves the suffix as preview', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'mcp-1',
      kind: 'tool_call',
      status: 'running',
      toolName: 'MCP/browser_snapshot',
      summary: '',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('puzzle');
    expect(getByTestId('tool-call-card-label').textContent).toBe('MCP');
    expect(getByTestId('tool-call-card-preview').textContent).toContain('browser_snapshot');
  });

  it('falls back to the generic icon + "Tool" label for an unknown tool', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'unk',
      kind: 'tool_call',
      status: 'running',
      toolName: 'CompletelyNovelTool',
      summary: 'doing something',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('generic');
    expect(getByTestId('tool-call-card-label').textContent).toBe('Tool');
    expect(getByTestId('tool-call-card-preview').textContent).toContain('doing something');
  });

  it('delegates to ProposedPlanCard when payloadKind=proposed_plan', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'plan',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'ExitPlanMode',
      payloadId: 'payload-plan',
      payloadKind: 'proposed_plan',
      payloadMeta: JSON.stringify({
        title: 'Deploy thing',
        lineCount: 3,
        charCount: 120,
        preview: '# plan',
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, { props: { pane, item } });

    // The generic fallback card must NOT render when a structured payload
    // renderer takes over.
    expect(queryByTestId('tool-call-card')).toBeNull();
    // ProposedPlanCard puts the title in a heading; sanity-check a fragment.
    expect(container.textContent).toContain('Deploy thing');
  });

  it('delegates to CommandOutput when payloadKind=command_output', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'cmd',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Bash',
      payloadId: 'payload-cmd',
      payloadKind: 'command_output',
      payloadMeta: JSON.stringify({
        command: 'ls',
        exitCode: 0,
        lineCount: 1,
        preview: 'file.txt',
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, { props: { pane, item } });

    expect(queryByTestId('tool-call-card')).toBeNull();
    // CommandOutput's button surfaces the command text.
    expect(container.textContent).toContain('ls');
  });

  it('delegates to DiffPreview when payloadKind=diff', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'diff',
      kind: 'tool_call',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'payload-diff',
      payloadKind: 'diff',
      payloadMeta: JSON.stringify({
        filePath: 'foo.ts',
        changeKind: 'modified',
        insertions: 1,
        deletions: 1,
        preview: '@@ -1 +1 @@',
      }),
    });

    const { queryByTestId, container } = render(ToolCallCard, { props: { pane, item } });

    expect(queryByTestId('tool-call-card')).toBeNull();
    expect(container.textContent).toContain('foo.ts');
  });

  it('delegates to ToolResultCard when payloadKind=tool_result', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'tr',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Edit',
      payloadId: 'payload-tr',
      payloadKind: 'tool_result',
      payloadMeta: JSON.stringify({ itemType: 'file_change', title: 'Edit applied' }),
    });

    const { queryByTestId, container } = render(ToolCallCard, { props: { pane, item } });

    expect(queryByTestId('tool-call-card')).toBeNull();
    expect(container.textContent).toContain('Edit applied');
  });

  it('falls through to the generic header when payloadKind is unknown', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 'unpay',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      // payloadKind set to something we don't special-case, with no structured meta.
      payloadKind: 'tool_call_result',
      summary: 'echo hi',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card').getAttribute('data-tool-kind')).toBe('terminal');
    expect(getByTestId('tool-call-card-preview').textContent).toContain('echo hi');
  });
});

describe('<ToolCallCard> status dispatch', () => {
  it('shows "running" for streaming/running tool calls', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 't',
      kind: 'tool_call',
      status: 'running',
      toolName: 'Bash',
      summary: 'sleep 10',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card-status').textContent).toBe('running');
    expect(getByTestId('tool-call-card-status').getAttribute('data-status')).toBe('running');
  });

  it('shows "failed" for errored tool calls', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 't',
      kind: 'tool_completion',
      status: 'errored',
      toolName: 'Bash',
      summary: 'oops',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card-status').textContent).toBe('failed');
  });

  it('shows "done" for completed tool calls', async () => {
    const pane = await buildPane();
    const item = makeItem({
      id: 't',
      kind: 'tool_completion',
      status: 'completed',
      toolName: 'Bash',
      summary: 'ok',
    });

    const { getByTestId } = render(ToolCallCard, { props: { pane, item } });

    expect(getByTestId('tool-call-card-status').textContent).toBe('done');
  });
});
