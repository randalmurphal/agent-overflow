import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

// A background agent's parked stop (claude-wire.md §E6b): the
// completion-shaped sibling, status `parked`, that a run writes when it
// reports and stops while background commands it started still run. Its
// card renders at the sibling like any other stop's. Stored history,
// written once. Keep these values mirrored with internal/store/agent_stops.go;
// mirror_pins_test.go enforces the cross-language contract. The card reads
// no run start: its duration runs from the launch (subagentCardStatus.ts).
export const PARKED_STOP_STATUS = 'parked';

export const PARKED_STOP_META = {
  commands: 'parked_commands',
  reportItemId: 'parked_report_item_id',
  runStartedAt: 'run_started_at',
  runWoke: 'run_woke',
} as const;

/**
 * Whether a row is a completion that ENDS its launch: every completion but
 * a parked stop, which pauses a background agent's run. Any completion
 * pairs with its launch for an activity run's counts; only an ending one
 * supersedes the launch's status, so a parked or woken agent's launch is
 * still the run's running member (internal/store/activity_runs.go
 * `endsLaunch`).
 */
export function completionEndsLaunch(row: { kind: string; completionOf?: string; status?: string }): boolean {
  return row.kind === 'tool_completion' && !!row.completionOf && row.status !== PARKED_STOP_STATUS;
}

export interface ParkedStop {
  /** The live background commands the run waited on at the stop. */
  waitingOn: number;
  /** The run's report: the transcript root's newest assistant_text row at the stop, or '' when the run wrote none. */
  reportItemId: string;
  /** A wake started the run: the agent reported before. */
  woke: boolean;
}

/** Reads a parked stop off its sibling; null for any other row. */
export function parkedStopFromItem(item: Pick<Item, 'status' | 'meta'> | null | undefined): ParkedStop | null {
  if (item?.status !== PARKED_STOP_STATUS) return null;
  const meta = parseJsonObject(item.meta);
  const waitingOn = meta?.[PARKED_STOP_META.commands];
  const reportItemId = meta?.[PARKED_STOP_META.reportItemId];
  return {
    waitingOn: typeof waitingOn === 'number' && waitingOn > 0 ? waitingOn : 0,
    reportItemId: typeof reportItemId === 'string' ? reportItemId : '',
    woke: meta?.[PARKED_STOP_META.runWoke] === true,
  };
}

/** What a parked stop's card says the agent did: "Reported, waiting on 1 background command". */
export function parkedStopSentence(stop: ParkedStop): string {
  const reported = stop.woke ? 'Reported again' : 'Reported';
  if (stop.waitingOn === 0) return `${reported}, waiting on background commands`;
  return `${reported}, waiting on ${stop.waitingOn} background ${stop.waitingOn === 1 ? 'command' : 'commands'}`;
}
