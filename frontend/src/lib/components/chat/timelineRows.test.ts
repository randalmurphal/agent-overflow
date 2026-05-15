import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import { timelineRowDecorations } from './timelineRows';

describe('timelineRowDecorations', () => {
  it('marks tool-to-assistant response dividers and response pill labels', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'tool', kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
      makeItem({ id: 'answer', itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
    ]);

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set([1]));
    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set([1]));
  });

  it('does not render a response divider for direct user-to-assistant text', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'user', kind: 'user_text', role: 'user' }),
      makeItem({ id: 'answer', itemIndex: 1, kind: 'assistant_text', summary: 'reply' }),
    ]);

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set());
    expect(decorations.responseDividerIndexes).toEqual(new Set());
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });

  it('renders only the first assistant response divider after tool activity', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'tool', kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
      makeItem({ id: 'mid', itemIndex: 1, kind: 'assistant_text', summary: 'first' }),
      makeItem({ id: 'final', itemIndex: 2, kind: 'assistant_text', summary: 'second' }),
    ]);

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });

  it('keeps active-turn response dividers unlabeled until the turn settles', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'tool', turnIndex: 1, kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
      makeItem({ id: 'answer', turnIndex: 1, itemIndex: 1, kind: 'assistant_text', summary: 'streaming' }),
    ]);

    const decorations = timelineRowDecorations(nodes, 1);

    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });
});
