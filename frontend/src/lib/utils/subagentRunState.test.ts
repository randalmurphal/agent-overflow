import { describe, expect, it } from 'vitest';
import { SUBAGENT_RUN_STATE_META, subagentRunStateFromMeta } from './subagentRunState';

const meta = (fields: Record<string, unknown>) => JSON.stringify({ task_id: 'A1', ...fields });

describe('subagentRunStateFromMeta', () => {
  it('reads a parked agent’s commands and report', () => {
    expect(subagentRunStateFromMeta(meta({
      [SUBAGENT_RUN_STATE_META.state]: 'parked',
      [SUBAGENT_RUN_STATE_META.parkedCommands]: 2,
      [SUBAGENT_RUN_STATE_META.parkedReportId]: 'report-1',
      [SUBAGENT_RUN_STATE_META.parkedReportPreview]: 'Found the race.',
    }))).toEqual({ state: 'parked', waitingOn: 2, report: { id: 'report-1', preview: 'Found the race.' } });
  });

  it('reads a parked agent with no report yet, and the transient parked-on-zero read', () => {
    expect(subagentRunStateFromMeta(meta({ [SUBAGENT_RUN_STATE_META.state]: 'parked', [SUBAGENT_RUN_STATE_META.parkedCommands]: 0 })))
      .toEqual({ state: 'parked', waitingOn: 0, report: null });
  });

  it('reads the other states without parked fields', () => {
    for (const state of ['running', 'done', 'ended'] as const) {
      expect(subagentRunStateFromMeta(meta({
        [SUBAGENT_RUN_STATE_META.state]: state,
        [SUBAGENT_RUN_STATE_META.parkedCommands]: 3,
        [SUBAGENT_RUN_STATE_META.parkedReportId]: 'stale',
      }))).toEqual({ state, waitingOn: 0, report: null });
    }
  });

  it('is null for a row the list did not decorate, an unknown state, or malformed meta', () => {
    expect(subagentRunStateFromMeta(undefined)).toBeNull();
    expect(subagentRunStateFromMeta(meta({}))).toBeNull();
    expect(subagentRunStateFromMeta(meta({ [SUBAGENT_RUN_STATE_META.state]: 'sleeping' }))).toBeNull();
    expect(subagentRunStateFromMeta('{"subagentRunState": ')).toBeNull();
  });

  it('drops a malformed count or report id', () => {
    expect(subagentRunStateFromMeta(meta({
      [SUBAGENT_RUN_STATE_META.state]: 'parked',
      [SUBAGENT_RUN_STATE_META.parkedCommands]: '2',
      [SUBAGENT_RUN_STATE_META.parkedReportId]: '',
      [SUBAGENT_RUN_STATE_META.parkedReportPreview]: 'orphan',
    }))).toEqual({ state: 'parked', waitingOn: 0, report: null });
  });
});
