import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import type { Item } from '../../types/models';
import TerminalInteractionRow from './TerminalInteractionRow.svelte';

function makeItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'waited:pid-42:0:0',
    threadId: 't-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'terminal_interaction',
    role: 'assistant',
    status: 'completed',
    summary: 'Waited for background terminal',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('<TerminalInteractionRow>', () => {
  it('renders the "Waited for background terminal" label from item.summary', () => {
    const { getByTestId, queryByRole } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.textContent).toContain('Waited for background terminal');
    // Without an attached command_output payload there's no embedded
    // command summary and no embedded CommandOutput shell.
    expect(queryByRole('button', { name: /Toggle command output/i })).toBeNull();
  });

  it('falls back to the canonical label when summary is empty', () => {
    // A triage bug (or a manual row) could land with a blank summary;
    // the component still renders the canonical label rather than
    // showing nothing.
    const item = makeItem({ summary: '' });
    const { getByTestId } = render(TerminalInteractionRow, { props: { item } });
    expect(getByTestId('terminal-interaction-row').textContent).toContain(
      'Waited for background terminal',
    );
  });

  it('renders running polls as waiting without a status badge', () => {
    const item = makeItem({ status: 'running' });
    const { getByTestId } = render(TerminalInteractionRow, { props: { item } });
    const row = getByTestId('terminal-interaction-row');

    expect(row.textContent).toContain('terminal');
    expect(row.textContent).toContain('Waiting for background terminal');
    expect(getByTestId('indicator').getAttribute('data-state')).toBe('running');
    expect(row.textContent).not.toContain('running');
  });

  it('renders the right-edge clock time from item.createdAt', () => {
    const createdAt = new Date(2026, 5, 10, 20, 5, 0).getTime();
    const { getByTestId } = render(TerminalInteractionRow, {
      props: { item: makeItem({ createdAt }) },
    });
    expect(getByTestId('terminal-interaction-time').getAttribute('datetime')).toBe(
      new Date(createdAt).toISOString(),
    );
  });

  it('uses the same compact wait-row treatment as wait_agent carriers', () => {
    const { getByTestId } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.className).not.toContain('italic');
    expect(row.className).toContain('text-fg-muted');
    expect(row.className).toContain('text-[0.75rem]');
  });

  it('renders the clock icon with a `wait` gutter label', () => {
    // The gutter label is fixed-width; "terminal" overflowed visibly
    // ("termin…") on rows next to the body's own "Waited for background
    // terminal" phrase. The clock icon + "wait" label keeps the gutter
    // compact and reads as "this row is the agent waiting for the PTY"
    // rather than another bash invocation.
    const { getByTestId } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    expect(getByTestId('terminal-interaction-label').textContent).toBe('wait');
    const icon = getByTestId('terminal-interaction-row').querySelector('svg[data-icon]');
    expect(icon?.getAttribute('data-icon')).toBe('clock');
    expect(icon?.getAttribute('aria-label')).toBe('wait');
  });

  it('renders attached command output underneath the wait carrier', () => {
    const item = makeItem({
      payloadKind: 'command_output',
      payloadId: 'command-output:waited:pid-42:0:0',
      payloadMeta: JSON.stringify({
        command: 'sleep 1; echo done',
        exitCode: 0,
        lineCount: 1,
        preview: 'done\n',
      }),
    });
    const { getByRole, getByTestId } = render(TerminalInteractionRow, { props: { item } });

    expect(getByTestId('terminal-interaction-row').textContent).toContain('Waited for background terminal');
    expect(getByTestId('command-output-row')).toBeInTheDocument();
    const toggle = getByRole('button', { name: /Toggle command output: sleep 1; echo done/i });
    expect(toggle.textContent).toContain('sleep 1; echo done');
    expect(getByTestId('command-output-status-slot').querySelector('[data-testid="indicator"]')).toBeNull();
  });

  it('does not render a duplicate command shell before a completion child attaches', () => {
    const item = makeItem({
      summary: 'Waited for background terminal: sleep 1; echo done',
    });
    const { getByTestId, queryByRole } = render(TerminalInteractionRow, { props: { item } });

    expect(getByTestId('terminal-interaction-row').textContent).toContain('Waited for background terminal');
    expect(queryByRole('button', { name: /Toggle command output/i })).toBeNull();
  });

  it('keeps structured terminal command metadata out of the wait carrier body', () => {
    const item = makeItem({
      summary: 'Waited for background terminal',
      meta: JSON.stringify({ kind: 'terminal_interaction', command: 'sleep 1; echo done' }),
    });
    const { getByTestId, queryByRole } = render(TerminalInteractionRow, { props: { item } });

    expect(getByTestId('terminal-interaction-row').textContent).toContain('Waited for background terminal');
    expect(queryByRole('button', { name: /sleep 1; echo done/i })).toBeNull();
  });
});
