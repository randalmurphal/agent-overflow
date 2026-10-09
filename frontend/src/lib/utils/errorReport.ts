// An error as the person who hit it can use it: a short reason to read, and
// the facts behind it to expand or copy into an agent. Every error surface
// builds one from whatever it caught; the report renders what exists and
// omits the rest, so a client-side failure carries no invented reference.

import { TransportError } from '../transport/wsClient';
import type { ErrorDetail } from '../transport/errorDetail';
import { userFacingError } from './userFacingError';

export interface ErrorReport {
  /** What the person was doing when it failed, phrased by the call site. */
  readonly context?: string;
  /** Why, in one short sentence. */
  readonly reason: string;
  /** The wire error code, when the failure came from a backend call. */
  readonly code?: string;
  /** The backend's record of the failure: reference, method, chain. */
  readonly detail?: ErrorDetail;
  /** The error's layers, outermost first, when the backend sent none. */
  readonly chain: readonly string[];
  /** A client-side exception's stack. */
  readonly stack?: string;
  /** Identifiers the call site knows: the thread, the project. */
  readonly facts: readonly (readonly [string, string])[];
  /** When the report was built, in Unix milliseconds. */
  readonly at: number;
}

export interface ErrorReportContext {
  context?: string;
  facts?: readonly (readonly [string, string])[];
}

const MAX_CAUSE_DEPTH = 8;

export function errorReport(err: unknown, { context, facts = [] }: ErrorReportContext = {}): ErrorReport {
  const detail = err instanceof TransportError ? err.detail : undefined;
  return {
    context: context?.trim() || undefined,
    reason: userFacingError(err),
    code: err instanceof TransportError ? err.code : undefined,
    detail,
    chain: detail && detail.chain.length > 0 ? [] : causeChain(err),
    stack: err instanceof Error && !(err instanceof TransportError) ? err.stack : undefined,
    facts,
    at: Date.now(),
  };
}

/** The headline a surface shows: the call site's context, else the reason. */
export function errorHeadline(report: ErrorReport): string {
  return report.context ?? report.reason;
}

/**
 * Whether Details would add anything to the headline a surface already
 * shows: a backend record, or layers beyond that one sentence.
 */
export function errorHasDetails(report: ErrorReport): boolean {
  if (report.detail) return true;
  const layers = errorLayers(report);
  const headline = errorHeadline(report);
  return layers.length > 1 || (layers.length === 1 && !saysAlready(headline, layers[0]));
}

// Whether text already carries sentence, ignoring the capital and full stop
// userFacingError adds.
function saysAlready(text: string, sentence: string): boolean {
  const core = sentence.trim().replace(/[.!]+$/, '').toLowerCase();
  return core === '' || text.toLowerCase().includes(core);
}

/** The layers a surface lists under Details. */
export function errorLayers(report: ErrorReport): readonly string[] {
  return report.detail && report.detail.chain.length > 0 ? report.detail.chain : report.chain;
}

function causeChain(err: unknown): string[] {
  const layers: string[] = [];
  let current: unknown = err;
  for (let depth = 0; depth < MAX_CAUSE_DEPTH && current !== undefined && current !== null; depth++) {
    if (current instanceof Error) {
      if (current.message) layers.push(current.message);
      current = current.cause;
    } else {
      const text = typeof current === 'string' ? current : safeString(current);
      if (text) layers.push(text);
      break;
    }
  }
  return layers;
}

function safeString(value: unknown): string {
  if (typeof value === 'object') {
    const message = (value as { message?: unknown }).message;
    if (typeof message === 'string') return message;
    try { return JSON.stringify(value); } catch { return ''; }
  }
  return String(value);
}

export interface ErrorReportEnvironment {
  appVersion?: string;
  platform?: string;
  /** The backend log lines leading up to the failure, when readable. */
  backendLog?: { lines: readonly string[]; found: boolean } | { unavailable: string };
}

/** The report as plain text for a person or an agent to read. */
export function errorReportText(report: ErrorReport, env: ErrorReportEnvironment = {}): string {
  const out: string[] = [`Agent Overflow error: ${errorHeadline(report)}`];
  if (report.context && !saysAlready(report.context, report.reason)) out.push(`Reason: ${report.reason}`);
  out.push('');
  if (report.code) out.push(`- code: ${report.code}`);
  if (report.detail) {
    out.push(`- method: ${report.detail.method}`);
    out.push(`- ref: ${report.detail.ref}`);
  }
  out.push(`- time: ${new Date(report.detail?.at ?? report.at).toISOString()}`);
  for (const [name, value] of report.facts) out.push(`- ${name}: ${value}`);
  if (env.appVersion) out.push(`- app: ${env.appVersion}${env.platform ? ` (${env.platform})` : ''}`);
  const layers = errorLayers(report);
  if (layers.length > 0) {
    out.push('', 'Error chain (outermost first):');
    layers.forEach((layer, index) => out.push(`${index + 1}. ${layer}`));
  }
  if (report.stack) out.push('', 'Stack:', '```', report.stack, '```');
  const log = env.backendLog;
  if (log && 'unavailable' in log) {
    out.push('', `Backend log: ${log.unavailable}`);
  } else if (log) {
    out.push('', log.found
      ? 'Backend log leading up to it:'
      : 'Backend log: the lines for this failure are no longer retained in memory.');
    if (log.found) out.push('```', ...log.lines, '```');
  } else if (report.detail) {
    out.push('', `Backend log: on the backend's computer, under ref ${report.detail.ref}.`);
  }
  return out.join('\n');
}
