import { parseJsonObject } from './parseJsonObject';

// A Claude background agent's run state (claude-wire.md §E6b), served on
// the rows of a `ListLiveBackgroundTasks` read and never stored or pushed.
// Keep these values mirrored with internal/triage/agent_run_state.go;
// mirror_pins_test.go enforces the cross-language contract.
export const SUBAGENT_RUN_STATE_META = {
  state: 'subagentRunState',
  parkedCommands: 'subagentParkedCommands',
  parkedReportId: 'subagentParkedReportId',
  parkedReportPreview: 'subagentParkedReportPreview',
} as const;

export const SUBAGENT_RUN_STATES = ['running', 'parked', 'done', 'ended'] as const;
export type SubagentRunStateName = (typeof SUBAGENT_RUN_STATES)[number];

export interface SubagentRunState {
  /**
   * `parked`: the agent reported and waits on background commands it
   * started. `ended`: a session death settled it.
   */
  state: SubagentRunStateName;
  /** Parked: the live background commands at the agent's transcript root. */
  waitingOn: number;
  /**
   * Parked: the run's report, by row id for the full row, with the head the
   * parked stop recorded (the preview its card shows). Null when the run
   * wrote no report.
   */
  report: { id: string; preview: string } | null;
}

function isRunStateName(value: unknown): value is SubagentRunStateName {
  return typeof value === 'string' && (SUBAGENT_RUN_STATES as readonly string[]).includes(value);
}

/** Reads a served run state from a live-list row's meta; null when absent. */
export function subagentRunStateFromMeta(meta: string | undefined): SubagentRunState | null {
  if (!meta?.includes(SUBAGENT_RUN_STATE_META.state)) return null;
  const parsed = parseJsonObject(meta);
  const state = parsed?.[SUBAGENT_RUN_STATE_META.state];
  if (!isRunStateName(state)) return null;
  if (state !== 'parked') return { state, waitingOn: 0, report: null };
  const waitingOn = parsed?.[SUBAGENT_RUN_STATE_META.parkedCommands];
  const id = parsed?.[SUBAGENT_RUN_STATE_META.parkedReportId];
  const preview = parsed?.[SUBAGENT_RUN_STATE_META.parkedReportPreview];
  return {
    state,
    waitingOn: typeof waitingOn === 'number' && waitingOn > 0 ? waitingOn : 0,
    report: typeof id === 'string' && id
      ? { id, preview: typeof preview === 'string' ? preview : '' }
      : null,
  };
}
