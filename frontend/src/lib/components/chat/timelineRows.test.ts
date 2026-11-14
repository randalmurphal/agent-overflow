import { describe, expect, it } from 'vitest';
import type { Item } from '../../types/models';
import { makeItem } from '../../../test/helpers/chat';
import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
import { timelineRowDecorations } from './timelineRows';

function leaf(overrides: Partial<Item>): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

// The pane's projection wraps every rail row in a run; the decorations see
// runs, not bare tool rows.
let runSeq = 0;
function run(...children: TimelineNode[]): ActivityRunNode {
  runSeq += 1;
  return {
    kind: 'activity_run',
    runId: `run-${runSeq}`,
    threadId: 'thread-1',
    children,
    collapsed: true,
    live: false,
    atTail: false,
    mountedFrom: 0,
    mountedRows: children.length,
    membershipEpoch: 0,
    memberItemIds: [],
    summaryItemIds: [],
    memberCount: children.length,
    unshippedBefore: 0,
    unshippedAfter: 0,
    loadedFirstItemId: '',
    loadedLastItemId: '',
  };
}

const bash = (overrides: Partial<Item> = {}) =>
  leaf({ id: 'tool', kind: 'tool_call', toolName: 'Bash', summary: 'ls', ...overrides });

describe('timelineRowDecorations', () => {
  it('marks tool-to-assistant response dividers and response pill labels', () => {
    const nodes = [
      run(bash()),
      leaf({ id: 'answer', itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set([1]));
    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set([1]));
  });

  it('treats a forked command answer as a final sourced result after activity', () => {
    const nodes = [
      run(leaf({ id: 'skill', kind: 'tool_call', toolName: 'Skill', summary: 'code-review' })),
      leaf({
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
    ];

    const decorations = timelineRowDecorations(nodes, null);
    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set([1]));
    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set([1]));
  });

  // The turn is wire-complete (no active key) while the reveal gate still
  // withholds its tail: the rows on screen end at an intermediate
  // observation. The pill belongs to the final text and waits for it.
  it('decides the final response on the whole structure, not the revealed rows', () => {
    const structure = [
      run(bash()),
      leaf({ id: 'observation', itemIndex: 1, kind: 'assistant_text', summary: 'looking' }),
      run(bash({ id: 'tool-2', itemIndex: 2, summary: 'cat' })),
      leaf({ id: 'answer', itemIndex: 3, kind: 'assistant_text', summary: 'done' }),
    ];
    const revealed = structure.slice(0, 2);

    const withheld = timelineRowDecorations(revealed, null, undefined, structure);
    expect(withheld.responseDividerIndexes).toEqual(new Set([1]));
    expect(withheld.responsePillIndexes).toEqual(new Set());

    const released = timelineRowDecorations(structure, null, undefined, structure);
    expect(released.responseDividerIndexes).toEqual(new Set([1, 3]));
    expect(released.responsePillIndexes).toEqual(new Set([3]));
  });

  it('does not render a response divider for direct user-to-assistant text', () => {
    const nodes = [
      leaf({ id: 'user', kind: 'user_text', role: 'user' }),
      leaf({ id: 'answer', itemIndex: 1, kind: 'assistant_text', summary: 'reply' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.toolTextBoundaryIndexes).toEqual(new Set());
    expect(decorations.responseDividerIndexes).toEqual(new Set());
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });

  it('renders only the first assistant response divider after tool activity', () => {
    const nodes = [
      run(bash()),
      leaf({ id: 'mid', itemIndex: 1, kind: 'assistant_text', summary: 'first' }),
      leaf({ id: 'final', itemIndex: 2, kind: 'assistant_text', summary: 'second' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });

  it('keys the pill on the surface-supplied turnKeyOf, not item.turnIndex', () => {
    // The agent pane's scoped window: a subagent's rows are written at the
    // main thread's write head across several provider turns. Keyed on
    // `item.turnIndex` the main thread settling turn 5 would label the prose
    // while the agent still runs. The facade keys the whole window as ONE
    // turn. The divider itself does not read turns.
    const nodes = [
      run(bash({ turnIndex: 3 })),
      leaf({ id: 'answer', turnIndex: 5, itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
    ];
    const oneKey = () => 0;

    const byTurnIndex = timelineRowDecorations(nodes, null);
    expect(byTurnIndex.responseDividerIndexes).toEqual(new Set([1]));
    expect(byTurnIndex.responsePillIndexes).toEqual(new Set([1]));

    const whileAgentRuns = timelineRowDecorations(nodes, 0, oneKey);
    expect(whileAgentRuns.responseDividerIndexes).toEqual(new Set([1]));
    expect(whileAgentRuns.responsePillIndexes).toEqual(new Set());

    const afterAgentSettles = timelineRowDecorations(nodes, null, oneKey);
    expect(afterAgentSettles.responseDividerIndexes).toEqual(new Set([1]));
    expect(afterAgentSettles.responsePillIndexes).toEqual(new Set([1]));
  });

  // A background-task wake opens a turn with no user row; the completion
  // that woke it stays at the end of the previous turn.
  it('divides a wake turn reply from the completion above it in the previous turn', () => {
    const nodes = [
      run(bash({ turnIndex: 194, summary: 'git diff' })),
      leaf({ id: 'answer', turnIndex: 194, itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
      run(leaf({
        id: 'completion',
        turnIndex: 194,
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'SendMessage',
        isBackground: true,
        completionOf: 'launch',
        status: 'parked',
        summary: 'Agent: Run validation stage 1 -> parked',
      })),
      leaf({ id: 'wake-reply', turnIndex: 195, kind: 'assistant_text', summary: 'still going' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.responseDividerIndexes).toEqual(new Set([1, 3]));
    expect(decorations.responsePillIndexes).toEqual(new Set([1, 3]));
  });

  it('divides prose only from a run directly above it', () => {
    const nodes = [
      run(bash()),
      leaf({ id: 'notice', itemIndex: 1, kind: 'notification', role: 'system', summary: 'compacted' }),
      leaf({ id: 'answer', itemIndex: 2, kind: 'assistant_text', summary: 'done' }),
      leaf({ id: 'plan', itemIndex: 3, kind: 'tool_call', toolName: 'ExitPlanMode', payloadKind: 'proposed_plan' }),
      leaf({ id: 'after-plan', itemIndex: 4, kind: 'assistant_text', summary: 'approved' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.responseDividerIndexes).toEqual(new Set());
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });

  it('does not divide a reply from activity before the user message it answers', () => {
    const nodes = [
      run(bash({ turnIndex: 1 })),
      leaf({ id: 'user', turnIndex: 2, kind: 'user_text', role: 'user' }),
      leaf({ id: 'answer', turnIndex: 2, itemIndex: 1, kind: 'assistant_text', summary: 'reply' }),
    ];

    const decorations = timelineRowDecorations(nodes, null);

    expect(decorations.responseDividerIndexes).toEqual(new Set());
  });

  it('keeps active-turn response dividers unlabeled until the turn settles', () => {
    const nodes = [
      run(bash({ turnIndex: 1 })),
      leaf({ id: 'answer', turnIndex: 1, itemIndex: 1, kind: 'assistant_text', summary: 'streaming' }),
    ];

    const decorations = timelineRowDecorations(nodes, 1);

    expect(decorations.responseDividerIndexes).toEqual(new Set([1]));
    expect(decorations.responsePillIndexes).toEqual(new Set());
  });
});
