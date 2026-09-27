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

  it('treats a forked command answer as a final sourced result after activity', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'skill', kind: 'tool_call', toolName: 'Skill', summary: 'code-review' }),
      makeItem({
        id: 'result',
        itemIndex: 1,
        kind: 'command_result',
        role: 'system',
        summary: 'No findings.',
        meta: JSON.stringify({
          kind: 'command_result',
          preview: 'No findings.',
          agentResult: {
            launchId: 'skill', sourceKind: 'skill', sourceName: 'code-review',
          },
        }),
      }),
    ]);

    const decorations = timelineRowDecorations(nodes, null);
    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set([1]));
    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set([1]));
  });

  // The turn is wire-complete (no active key) while the reveal gate still
  // withholds its tail: the rows on screen end at an intermediate
  // observation. The pill belongs to the final text and waits for it.
  it('decides the final response on the whole structure, not the revealed rows', () => {
    const structure = groupItemsBySubagent([
      makeItem({ id: 'tool', kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
      makeItem({ id: 'observation', itemIndex: 1, kind: 'assistant_text', summary: 'looking' }),
      makeItem({ id: 'tool-2', itemIndex: 2, kind: 'tool_call', toolName: 'Bash', summary: 'cat' }),
      makeItem({ id: 'answer', itemIndex: 3, kind: 'assistant_text', summary: 'done' }),
    ]);
    const revealed = structure.slice(0, 2);

    const withheld = timelineRowDecorations(revealed, null, undefined, structure);
    expect(withheld.responseDividerIndexes).toEqual(new Set([1]));
    expect(withheld.responsePillIndexes).toEqual(new Set());

    const released = timelineRowDecorations(structure, null, undefined, structure);
    expect(released.responseDividerIndexes).toEqual(new Set([1, 3]));
    expect(released.responsePillIndexes).toEqual(new Set([3]));
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

  it('keys turn identity on the surface-supplied turnKeyOf, not item.turnIndex', () => {
    // The agent pane's scoped window: a subagent's rows are written at the
    // main thread's write head across several provider turns. Keyed on
    // `item.turnIndex` the tool (turn 3) and the prose (turn 5) never meet,
    // so the prose gets no divider — and worse, the main thread settling
    // turn 3 would label it. The facade keys the whole window as ONE turn.
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'tool', turnIndex: 3, kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
      makeItem({ id: 'answer', turnIndex: 5, itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
    ]);
    const oneKey = () => 0;

    const byTurnIndex = timelineRowDecorations(nodes, null);
    expect(byTurnIndex.responseDividerIndexes).toEqual(new Set());

    const whileAgentRuns = timelineRowDecorations(nodes, 0, oneKey);
    expect(whileAgentRuns.responseDividerIndexes).toEqual(new Set([1]));
    expect(whileAgentRuns.responsePillIndexes).toEqual(new Set());

    const afterAgentSettles = timelineRowDecorations(nodes, null, oneKey);
    expect(afterAgentSettles.responseDividerIndexes).toEqual(new Set([1]));
    expect(afterAgentSettles.responsePillIndexes).toEqual(new Set([1]));
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
