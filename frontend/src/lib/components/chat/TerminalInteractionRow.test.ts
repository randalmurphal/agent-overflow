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
    const { getByTestId, queryByRole, container } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.textContent).toContain('Waited for background terminal');
    // Without an attached command_output payload there's no embedded
    // command summary and no embedded CommandOutput shell.
    expect(queryByRole('button', { name: /Toggle command output/i })).toBeNull();
    expect(container.querySelector('[data-testid="completion-badge"]')).toBeNull();
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

    expect(row.textContent?.trim()).toBe('Waiting for background terminal');
    expect(row.textContent).not.toContain('running');
  });

  it('uses the same compact wait-row treatment as wait_agent carriers', () => {
    const { getByTestId } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.className).not.toContain('italic');
    expect(row.className).toContain('text-fg-muted');
    expect(row.className).toContain('text-[12px]');
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

    expect(getByTestId('terminal-interaction-row').textContent?.trim()).toBe('Waited for background terminal');
    expect(getByTestId('command-output-row')).toBeInTheDocument();
    const toggle = getByRole('button', { name: /Toggle command output: sleep 1; echo done/i });
    expect(toggle.textContent).toContain('sleep 1; echo done');
    // exit-code text is gone; the unified completion badge carries the
    // success/failure signal now (success because exitCode === 0). It
    // sits in the stable header action slot, outside the toggle button
    // so future interactive actions cannot become nested controls.
    expect(getByTestId('completion-badge').getAttribute('data-status')).toBe('success');
  });

  it('does not render a duplicate command shell before a completion child attaches', () => {
    const item = makeItem({
      summary: 'Waited for background terminal: sleep 1; echo done',
    });
    const { getByTestId, queryByRole, queryByTestId } = render(TerminalInteractionRow, { props: { item } });

    expect(getByTestId('terminal-interaction-row').textContent?.trim()).toBe('Waited for background terminal');
    expect(queryByRole('button', { name: /Toggle command output/i })).toBeNull();
    expect(queryByTestId('completion-badge')).toBeNull();
  });

  it('keeps structured terminal command metadata out of the wait carrier body', () => {
    const item = makeItem({
      summary: 'Waited for background terminal',
      meta: JSON.stringify({ kind: 'terminal_interaction', command: 'sleep 1; echo done' }),
    });
    const { getByTestId, queryByRole } = render(TerminalInteractionRow, { props: { item } });

    expect(getByTestId('terminal-interaction-row').textContent?.trim()).toBe('Waited for background terminal');
    expect(queryByRole('button', { name: /sleep 1; echo done/i })).toBeNull();
  });
});
