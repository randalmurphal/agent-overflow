import { parseJsonObject } from './parseJsonObject';

// A parked background agent's bell (claude-wire.md §E6b): the notification
// row triage writes at a stop that is a pause, naming the commands the
// agent waits on and the round's report row. Stored history, written once.
// Keep these values mirrored with internal/triage/agent_run_state.go;
// mirror_pins_test.go enforces the cross-language contract.
export const PARKED_AGENT_BELL_KIND = 'parked_agent';

export const PARKED_AGENT_BELL_META = {
  commands: 'parked_commands',
  reportItemId: 'parked_report_item_id',
  reportPreview: 'parked_report_preview',
} as const;

export interface ParkedAgentBell {
  /** The live background commands the agent waited on at the stop. */
  waitingOn: number;
  /**
   * The round's report: the transcript root's newest assistant_text at the
   * stop, by row id for the full row, with the head of its text. Null when
   * the agent had written no report yet.
   */
  report: { id: string; preview: string } | null;
}

/** Reads a parked bell's link off its meta; null for any other notification. */
export function parkedAgentBellFromMeta(meta: string | undefined): ParkedAgentBell | null {
  if (!meta?.includes(PARKED_AGENT_BELL_KIND)) return null;
  const parsed = parseJsonObject(meta);
  if (parsed?.kind !== PARKED_AGENT_BELL_KIND) return null;
  const waitingOn = parsed[PARKED_AGENT_BELL_META.commands];
  const id = parsed[PARKED_AGENT_BELL_META.reportItemId];
  const preview = parsed[PARKED_AGENT_BELL_META.reportPreview];
  return {
    waitingOn: typeof waitingOn === 'number' && waitingOn > 0 ? waitingOn : 0,
    report: typeof id === 'string' && id
      ? { id, preview: typeof preview === 'string' ? preview : '' }
      : null,
  };
}
