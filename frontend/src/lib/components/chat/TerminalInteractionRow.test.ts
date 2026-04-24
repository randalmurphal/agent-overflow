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
    highlightedContent: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('<TerminalInteractionRow>', () => {
  it('renders the "Waited for background terminal" label from item.summary', () => {
    const { getByTestId } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.textContent).toContain('Waited for background terminal');
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

  it('uses the muted italic text-subtle treatment (not a primary-row style)', () => {
    // This row is a low-signal marker — it must NOT look like an
    // assistant message, tool card, or error. The italic + text-subtle
    // classes encode that UX intent; pinning them keeps a future
    // restyle from accidentally promoting the row to primary content.
    const { getByTestId } = render(TerminalInteractionRow, { props: { item: makeItem() } });
    const row = getByTestId('terminal-interaction-row');
    expect(row.className).toContain('italic');
    expect(row.className).toContain('text-fg-subtle');
  });

  it('renders attached command output when the wait row carries a command payload', () => {
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
    const { getByRole } = render(TerminalInteractionRow, { props: { item } });

    const toggle = getByRole('button', { name: /Toggle command output: sleep 1; echo done/i });
    expect(toggle.textContent).toContain('sleep 1; echo done');
    expect(toggle.textContent).toContain('exit 0');
  });
});
