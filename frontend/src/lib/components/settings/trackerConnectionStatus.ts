// What a tracker connection's row says about it, read from the row
// TrackerConnections answered and the clock. One function per question,
// so the Connections page and any later surface word a connection alike.

import type { TrackerConnection } from '../../stores/bindings';
import { formatCalendarDate, formatCountdownSpan } from '../../utils/format';

/** How long before a token's recorded expiry its row warns. */
export const TOKEN_WARNING_DAYS = 14;

const DAY_MS = 24 * 60 * 60 * 1000;

export type StatusTone = 'muted' | 'ok' | 'warning' | 'error';

export interface TrackerConnectionStatus {
  tone: StatusTone;
  /** A short label: "Signed in as User 1", "Needs setup", "Off". */
  label: string;
  /** The sentence under it, or '' when the label says it all. */
  detail: string;
  /** The row shows the steps that put a working token in place. */
  needsSetup: boolean;
}

export interface TokenExpiry {
  tone: StatusTone;
  text: string;
  /** The token runs out soon or has: the row offers Renew. */
  renew: boolean;
}

const KIND_NAMES: Record<string, string> = { jira: 'Jira', linear: 'Linear', tempo: 'Tempo' };

/** "Jira", "Linear" or "Tempo". */
export function trackerKindName(kind: string): string {
  return KIND_NAMES[kind] ?? kind;
}

/** The name a row goes by: its label, or what it connects to. */
export function trackerConnectionName(connection: Pick<TrackerConnection, 'label' | 'kind' | 'site'>): string {
  if (connection.label) return connection.label;
  switch (connection.kind) {
    case 'jira':
      return connection.site.replace(/^https:\/\//, '');
    case 'linear':
      return `Linear ${connection.site}`;
    case 'tempo':
      return 'Tempo time logging';
    default:
      return trackerKindName(connection.kind);
  }
}

const PROBLEM_LABELS: Record<string, { label: string; tone: StatusTone }> = {
  setup: { label: 'Needs setup', tone: 'error' },
  forbidden: { label: 'Not allowed', tone: 'error' },
  transient: { label: 'Cannot reach the tracker', tone: 'warning' },
  rate_limited: { label: 'Rate limited', tone: 'warning' },
  tracker: { label: 'Tracker error', tone: 'error' },
  not_found: { label: 'Not found', tone: 'error' },
  internal: { label: 'This computer failed', tone: 'error' },
};

/** The key file's state as a sentence. */
export function keyFileSentence(connection: TrackerConnection): string {
  switch (connection.keyFileState) {
    case 'present':
      return 'The key file holds a token.';
    case 'missing':
      return 'There is no key file yet.';
    case 'empty':
      return 'The key file is empty.';
    case 'refused':
      return connection.keyFileProblem || 'The key file cannot be used.';
    default:
      return 'The key file cannot be read.';
  }
}

/** Who the last check found the token belongs to; '' for a check that names nobody (Tempo's). */
function signedInAs(connection: TrackerConnection): string {
  const name = connection.verifiedName || connection.verifiedAccountId;
  if (!name) return '';
  if (connection.kind === 'jira' && connection.account && connection.account !== name) {
    return `${name} (${connection.account})`;
  }
  return name;
}

/**
 * What a row says about its token, and whether it shows the setup steps,
 * from what the computer answered: whether the key file holds the token
 * the last check verified (`checkedTokenInFile`) and whether the last
 * failure is about the token in the file now (`problem.current`). The
 * computer holds the tokens and makes both comparisons; this compares
 * nothing.
 */
export function trackerConnectionStatus(connection: TrackerConnection): TrackerConnectionStatus {
  if (!connection.enabled) {
    return {
      tone: 'muted',
      label: 'Off',
      detail: 'Nothing syncs, and this computer keeps no copy of its boards. Turning it on reads them again.',
      needsSetup: false,
    };
  }
  if (connection.keyFileState !== 'present') {
    return { tone: 'warning', label: 'Needs a token', detail: keyFileSentence(connection), needsSetup: true };
  }
  const who = signedInAs(connection);
  const lastChecked = connection.verifiedAt > 0 && who ? ` It was last checked as ${who}.` : '';
  const problem = connection.problem;
  if (problem?.current) {
    const known = PROBLEM_LABELS[problem.kind] ?? { label: 'Failed', tone: 'error' as const };
    return {
      tone: known.tone,
      label: known.label,
      detail: `${sentence(problem.message)}${lastChecked}`,
      needsSetup: problem.kind === 'setup' || !connection.checkedTokenInFile,
    };
  }
  if (connection.checkedTokenInFile) {
    return { tone: 'ok', label: who ? `Signed in as ${who}` : 'Token checked', detail: '', needsSetup: false };
  }
  if (problem) {
    return {
      tone: 'muted',
      label: 'Key file changed since the last failure',
      detail: `The token in it now has not been checked. Before the change: ${sentence(problem.message)}${lastChecked}`,
      needsSetup: true,
    };
  }
  if (connection.verifiedAt === 0) {
    return {
      tone: 'muted',
      label: 'Not checked',
      detail: 'The key file holds a token. Check the connection to see whose it is.',
      needsSetup: true,
    };
  }
  return {
    tone: 'muted',
    label: 'Token not checked',
    detail: who
      ? `It was last checked as ${who}. The key file holds another token now; check the connection to see whose.`
      : 'The key file holds another token than the one last checked; check the connection to test it.',
    needsSetup: true,
  };
}

/** A sentence ending in its own stop. */
function sentence(text: string): string {
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

/** What the row says about the token's recorded expiry; null when none is recorded. */
export function tokenExpiry(expiresAt: number, now: number): TokenExpiry | null {
  if (expiresAt <= 0) return null;
  if (expiresAt <= now) return { tone: 'error', text: `Token expired on ${formatCalendarDate(expiresAt)}.`, renew: true };
  if (expiresAt - now <= TOKEN_WARNING_DAYS * DAY_MS) {
    return { tone: 'warning', text: `Token runs out in ${formatCountdownSpan(expiresAt, now)}.`, renew: true };
  }
  return { tone: 'muted', text: `Token good until ${formatCalendarDate(expiresAt)}.`, renew: false };
}

/** A date input's value ("2027-03-14") as the start of that day here, in unix milliseconds. */
export function startOfLocalDay(value: string): number {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  if (!match) throw new Error(`"${value}" is not a date`);
  return new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3])).getTime();
}

/** A unix-millisecond time as a date input's value for that day here. */
export function localDateValue(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number): string => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Tone to text color. */
export const TONE_CLASS: Record<StatusTone, string> = {
  muted: 'text-fg-muted',
  ok: 'text-success',
  warning: 'text-warning',
  error: 'text-error',
};
