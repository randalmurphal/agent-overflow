import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import CommandResultRow from './CommandResultRow.svelte';
import TimelineLeaf from './TimelineLeaf.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Item } from '../../types/models';

const INLINE_TEXT = 'Current session\n  Tokens:  12,345 in / 6,789 out\n  Cost:    $0.00';
const PREVIEW_TEXT = 'Context usage\n  System prompt   3.1k';
const FULL_TEXT = `${PREVIEW_TEXT}\n  MCP tools       9.4k\n  Free space      812k`;

function inlineItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'command-result:msg-1',
    kind: 'command_result',
    role: 'system',
    status: 'completed',
    summary: INLINE_TEXT,
    meta: JSON.stringify({ kind: 'command_result', preview: INLINE_TEXT }),
    ...overrides,
  });
}

function truncatedItem(overrides: Partial<Item> = {}): Item {
  return makeItem({
    id: 'command-result:msg-2',
    kind: 'command_result',
    role: 'system',
    status: 'completed',
    summary: PREVIEW_TEXT,
    payloadId: 'command-result-payload',
    payloadKind: 'command_result',
    meta: JSON.stringify({
      kind: 'command_result',
      preview: PREVIEW_TEXT,
      truncated: true,
      totalBytes: 12_800,
    }),
    ...overrides,
  });
}

function agentResultItem(overrides: Partial<Item> = {}): Item {
  const markdown = '## Findings\n\n- Review `internal/triage/command_result.go:1`.';
  return inlineItem({
    id: 'command-result:skill-1',
    summary: markdown,
    meta: JSON.stringify({
      kind: 'command_result',
      preview: markdown,
      agentResult: {
        launchId: 'claude-command:cmd-1',
        sourceKind: 'skill',
        sourceName: 'code-review',
      },
    }),
    ...overrides,
  });
}

describe('<CommandResultRow>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders the whole output inline, monospaced, with no fetch affordance', () => {
    const { getByTestId, queryByTestId, container } = render(CommandResultRow, {
      props: { item: inlineItem() },
    });

    const output = getByTestId('command-result-output');
    expect(output.textContent).toBe(INLINE_TEXT);
    expect(output.className).toContain('font-mono');
    expect(output.className).toContain('whitespace-pre-wrap');
    // Nothing to load: no size chip, no button, no empty footer strip.
    expect(queryByTestId('command-result-show-full')).toBeNull();
    expect(queryByTestId('command-result-size')).toBeNull();
    // Terminal icon + "command" label carry the attribution — this is not a
    // message from the model.
    expect(container.querySelector('svg[data-icon]')?.getAttribute('data-icon')).toBe('terminal');
    expect(getByTestId('command-result-label').textContent).toBe('command');
  });

  it('renders a forked Skill answer as sourced Markdown instead of terminal output', async () => {
    const { getByTestId, queryByTestId, container } = render(CommandResultRow, {
      props: { item: agentResultItem() },
    });

    expect(getByTestId('command-result-row')).toHaveAttribute('data-agent-result', 'true');
    expect(getByTestId('agent-result-source').textContent).toContain('code-review · skill result');
    expect(getByTestId('agent-result-body')).not.toHaveClass('font-mono');
    expect(queryByTestId('command-result-output')).toBeNull();
    await waitFor(() => expect(container.querySelector('h2')?.textContent).toBe('Findings'));
    expect(container.querySelector('li')?.textContent).toContain('Review');
  });

  it('shows the preview plus a sized load affordance while truncated', () => {
    const { getByTestId } = render(CommandResultRow, { props: { item: truncatedItem() } });

    expect(getByTestId('command-result-output').textContent).toBe(PREVIEW_TEXT);
    expect(getByTestId('command-result-size').textContent).toBe('12.5 KB');
    const button = getByTestId('command-result-show-full');
    expect(button.textContent).toContain('Show full output (12.5 KB)');
    expect(button.getAttribute('aria-controls')).toBe(
      getByTestId('command-result-output').getAttribute('id'),
    );
  });

  it('fetches the payload on expand and grows the block in place', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: FULL_TEXT,
      nextOffset: FULL_TEXT.length,
      totalSize: FULL_TEXT.length,
      isComplete: true,
    }));

    const { getByTestId, queryByTestId } = render(CommandResultRow, {
      props: { item: truncatedItem() },
    });

    const toggle = getByTestId('command-result-show-full');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    await fireEvent.click(toggle);

    await waitFor(() => {
      expect(getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });
    // The control the reader clicked stays put and reports the new state.
    expect(getByTestId('command-result-show-full').getAttribute('aria-expanded')).toBe('true');
    expect(getByTestId('command-result-show-full').textContent).toContain('Show less');
    expect(queryByTestId('command-result-show-more')).toBeNull();
  });

  it('collapses back to the inline preview', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: FULL_TEXT,
      nextOffset: FULL_TEXT.length,
      totalSize: FULL_TEXT.length,
      isComplete: true,
    }));

    const { getByTestId } = render(CommandResultRow, { props: { item: truncatedItem() } });

    await fireEvent.click(getByTestId('command-result-show-full'));
    await waitFor(() => {
      expect(getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });

    await fireEvent.click(getByTestId('command-result-show-full'));
    await waitFor(() => {
      expect(getByTestId('command-result-output').textContent).toBe(PREVIEW_TEXT);
    });
    expect(getByTestId('command-result-show-full').textContent).toContain('Show full output');
  });

  it('surfaces a failed fetch with a retry that succeeds', async () => {
    let attempt = 0;
    setBindingMock('GetPayloadPreview', async () => {
      attempt += 1;
      if (attempt === 1) throw new Error('payload gone');
      return {
        data: FULL_TEXT,
        nextOffset: FULL_TEXT.length,
        totalSize: FULL_TEXT.length,
        isComplete: true,
      };
    });

    const { getByTestId, getByRole } = render(CommandResultRow, {
      props: { item: truncatedItem() },
    });

    await fireEvent.click(getByTestId('command-result-show-full'));
    await waitFor(() => {
      expect(getByRole('alert').textContent).toContain('payload gone');
    });
    // The preview never blanks — the reader keeps the head they had.
    expect(getByTestId('command-result-output').textContent).toBe(PREVIEW_TEXT);

    await fireEvent.click(getByTestId('command-result-retry'));
    await waitFor(() => {
      expect(getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });
  });

  it('offers "load more" when the preview did not cover the payload', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: PREVIEW_TEXT,
      nextOffset: PREVIEW_TEXT.length,
      totalSize: FULL_TEXT.length,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: FULL_TEXT.slice(PREVIEW_TEXT.length),
      nextOffset: FULL_TEXT.length,
      totalSize: FULL_TEXT.length,
      isComplete: true,
    }));

    const { getByTestId } = render(CommandResultRow, { props: { item: truncatedItem() } });

    await fireEvent.click(getByTestId('command-result-show-full'));
    await waitFor(() => expect(getByTestId('command-result-show-more')).toBeTruthy());

    await fireEvent.click(getByTestId('command-result-show-more'));
    await waitFor(() => {
      expect(getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });
  });

  it('keeps the reader\'s expansion in the pane registry across a remount', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: FULL_TEXT,
      nextOffset: FULL_TEXT.length,
      totalSize: FULL_TEXT.length,
      isComplete: true,
    }));
    const item = truncatedItem();
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [item]);

    const first = render(CommandResultRow, { props: { pane, item } });
    await fireEvent.click(first.getByTestId('command-result-show-full'));
    await waitFor(() => {
      expect(first.getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });
    first.unmount();

    // A windowing remount must find the row already open: the intent lives on
    // the pane, not in row-local state.
    const second = render(CommandResultRow, { props: { pane, item } });
    await waitFor(() => {
      expect(second.getByTestId('command-result-output').textContent).toBe(FULL_TEXT);
    });
    expect(second.getByTestId('command-result-show-full').getAttribute('aria-expanded')).toBe(
      'true',
    );
  });
});

describe('<TimelineLeaf> command_result dispatch', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('renders the command row, never the assistant fallback', async () => {
    const item = inlineItem();
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [item]);

    const { getByTestId, container } = render(TimelineLeaf, { props: { pane, item } });

    expect(getByTestId('command-result-row')).toBeTruthy();
    // TimelineLeaf owns the data-item-id contract for every leaf kind.
    expect(container.querySelector('[data-item-id]')?.getAttribute('data-item-id')).toBe(item.id);
    expect(container.querySelector('[data-testid="assistant-message-body"]')).toBeNull();
  });
});
