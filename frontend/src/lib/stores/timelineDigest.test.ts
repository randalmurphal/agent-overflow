// The live half of a Codex card's execution membership: a row that another
// execution of the agent started (tagged codex_execution_turn_id) is not in
// the card's body even inside its bounds, mirroring the store's
// codexExecutionMemberSQL.
import { describe, expect, it } from 'vitest';
import { includesDigestItem, withinDigestExecution } from './timelineDigest';
import { makeItem } from '../../test/helpers/chat';

const digest = {
  after: { turnIndex: 0, itemIndex: 4, itemId: '' },
  before: { turnIndex: 0, itemIndex: 20, itemId: '' },
  promptId: '',
  answerId: '',
  executionTurnId: 'B',
};

function commandRow(meta: Record<string, unknown>) {
  return makeItem({ id: 'cmd', kind: 'tool_call', turnIndex: 0, itemIndex: 10, meta: JSON.stringify(meta) });
}

describe('withinDigestExecution', () => {
  it('keeps untagged rows and rows of the card’s own execution', () => {
    expect(withinDigestExecution(commandRow({ command: 'ls' }), digest)).toBe(true);
    expect(includesDigestItem(commandRow({ codex_execution_turn_id: 'B' }), digest)).toBe(true);
  });

  it('drops a row an earlier execution started', () => {
    expect(withinDigestExecution(commandRow({ codex_execution_turn_id: 'A' }), digest)).toBe(false);
    expect(includesDigestItem(commandRow({ codex_execution_turn_id: 'A' }), digest)).toBe(false);
  });

  it('keeps tagged rows on a card that names no execution', () => {
    expect(withinDigestExecution(commandRow({ codex_execution_turn_id: 'A' }), { ...digest, executionTurnId: undefined })).toBe(true);
  });
});
