// The state line under a standing agent's sidebar row: one line from the
// agent's live loops and watches (docs/specs/standing-agents.md, "Sidebar").
// The supervisor supplies the next fire and the missed count; the agent's
// own labels say what each row is. Pure: the row passes the clock in.

import type { ThreadWakeRow } from '../stores/bindings';
import { formatCountdownSpan } from './format';

export interface AgentStateLine {
  /** The one-line text, built from the row that fires soonest. */
  text: string;
  /** Every live row, one per line, for the tooltip. */
  title: string;
}

/** The loops and watches among a thread's wake rows; reminders are not state. */
export function agentWakeRows(rows: readonly ThreadWakeRow[]): ThreadWakeRow[] {
  return rows.filter((row) => row.kind === 'loop' || row.kind === 'watch');
}

function rowLine(row: ThreadWakeRow, nowMs: number): string {
  const parts = [`${row.kind} ${row.label}`];
  if (row.state === 'running') parts.push('running');
  else if (row.longRunning) parts.push('stopped');
  else if (row.nextAt && row.nextAt > 0) parts.push(`next ${formatCountdownSpan(row.nextAt, nowMs)}`);
  if (row.missed && row.missed > 0) parts.push(`${row.missed} missed`);
  return parts.join(' · ');
}

/**
 * Order for the line: a running row first, then by next fire, soonest
 * first; a row with no fire (a stopped long-running watch) last.
 */
function compareRows(a: ThreadWakeRow, b: ThreadWakeRow): number {
  const aRunning = a.state === 'running';
  const bRunning = b.state === 'running';
  if (aRunning !== bRunning) return aRunning ? -1 : 1;
  const aNext = a.nextAt && a.nextAt > 0 ? a.nextAt : Number.POSITIVE_INFINITY;
  const bNext = b.nextAt && b.nextAt > 0 ? b.nextAt : Number.POSITIVE_INFINITY;
  if (aNext !== bNext) return aNext < bNext ? -1 : 1;
  return a.label < b.label ? -1 : a.label > b.label ? 1 : 0;
}

/** Null when the agent has no live loop or watch: the row shows no line. */
export function agentStateLine(rows: readonly ThreadWakeRow[], nowMs: number): AgentStateLine | null {
  const live = agentWakeRows(rows).sort(compareRows);
  if (live.length === 0) return null;
  const lines = live.map((row) => rowLine(row, nowMs));
  let text = lines[0];
  if (live.length > 1) text += ` · +${live.length - 1} more`;
  return { text, title: lines.join('\n') };
}
