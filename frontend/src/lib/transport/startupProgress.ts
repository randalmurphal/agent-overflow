// The report a starting backend answers /bootstrap.json with: a 503 whose
// JSON body has `reason: "starting"`. Mirrors internal/startupprogress,
// which owns the wire shape; the backend, the attached-backend hop and the
// `--connect` stub all serve it.

/** What a starting backend reported. Times are the backend's Unix millis. */
export interface StartupProgress {
  /** The boot phase id (the backend's `boot: phase=` log name). */
  phase: string;
  /** The sentence a person reads for the phase. */
  detail: string;
  /** Sub-steps inside the phase, such as pending migrations; 0 when none. */
  step: number;
  steps: number;
  startedAt: number;
  /** The last observed progress. */
  updatedAt: number;
  /** The last heartbeat, whether or not anything progressed. */
  aliveAt: number;
  /** The version an in-app update is being finished to; '' on a plain start. */
  updatingTo: string;
}

/** What the transport status carries while a backend starts. */
export interface TransportStartup {
  phase: string;
  detail: string;
  step: number;
  steps: number;
  /** How long the backend has been starting, by its own clock. */
  elapsedMs: number;
  updatingTo: string;
}

// The manifest fetch reads a 503's body only to find this report, and the
// report is a few hundred bytes. Anything larger is not one.
export const STARTUP_REPORT_LIMIT_BYTES = 16 << 10;
// Report strings are rendered as text. Bound them so a malformed report
// cannot grow a status line without limit.
const MAX_FIELD_CHARS = 200;

/**
 * The manifest fetch met a backend that is starting. Not a failure: the
 * backend answered and said how far it has got. The transport reports it
 * as the 'starting' status and asks again shortly.
 */
export class BackendStartingError extends Error {
  readonly progress: StartupProgress;
  constructor(progress: StartupProgress) {
    // The whole sentence a toast shows for a call made meanwhile, through
    // the DisconnectedError that carries this as its cause.
    super('the computer is still starting');
    this.name = 'BackendStartingError';
    this.progress = progress;
  }
}

function text(value: unknown): string {
  return typeof value === 'string' ? value.slice(0, MAX_FIELD_CHARS) : '';
}

function count(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? Math.floor(value) : 0;
}

/** A starting report from a parsed 503 body, or null when it is not one. */
export function parseStartupProgress(body: unknown): StartupProgress | null {
  if (body === null || typeof body !== 'object') return null;
  const report = body as Record<string, unknown>;
  if (report.reason !== 'starting') return null;
  return {
    phase: text(report.phase),
    detail: text(report.detail),
    step: count(report.step),
    steps: count(report.steps),
    startedAt: count(report.startedAt),
    updatedAt: count(report.updatedAt),
    aliveAt: count(report.aliveAt),
    updatingTo: text(report.updatingTo),
  };
}

/**
 * Read a 503 answer's body as a starting report. The body is read up to
 * STARTUP_REPORT_LIMIT_BYTES and released either way; a body that is not
 * JSON, is too large or is not a report answers null.
 */
export async function readStartupProgress(resp: Response): Promise<StartupProgress | null> {
  const body = resp.body;
  if (body === null) return null;
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > STARTUP_REPORT_LIMIT_BYTES) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(new TextDecoder().decode(bytes));
  } catch {
    return null;
  }
  return parseStartupProgress(parsed);
}

/**
 * The transport status fields for a report. Elapsed time runs to the
 * report's latest time, so it keeps counting through a step that is
 * working without visible progress.
 */
export function transportStartup(progress: StartupProgress): TransportStartup {
  const latest = Math.max(progress.updatedAt, progress.aliveAt);
  return {
    phase: progress.phase,
    detail: progress.detail,
    step: progress.step,
    steps: progress.steps,
    elapsedMs: progress.startedAt > 0 && latest > progress.startedAt ? latest - progress.startedAt : 0,
    updatingTo: progress.updatingTo,
  };
}

/** Whether two startup snapshots say the same thing. */
export function sameTransportStartup(a: TransportStartup | undefined, b: TransportStartup | undefined): boolean {
  if (a === b) return true;
  if (a === undefined || b === undefined) return false;
  return a.phase === b.phase && a.detail === b.detail && a.step === b.step && a.steps === b.steps
    && a.elapsedMs === b.elapsedMs && a.updatingTo === b.updatingTo;
}

/** A version the way the app shows it, with one leading "v". */
export function displayVersion(version: string): string {
  return version.startsWith('v') ? version : `v${version}`;
}

function isUpper(c: string): boolean {
  return c !== '' && c === c.toUpperCase() && c !== c.toLowerCase();
}

// Lowercase a leading capital that starts an ordinary word, leaving an
// acronym such as "WSL" intact.
function lowerFirst(value: string): string {
  if (!isUpper(value.charAt(0)) || isUpper(value.charAt(1))) return value;
  return value.charAt(0).toLowerCase() + value.slice(1);
}

/**
 * The sentence a person reads for a starting backend: the detail, prefixed
 * with the update being finished when there is one, as in "Finishing
 * update to v1.2.3: applying migration 3 of 7 add_index". The same
 * sentence internal/startupprogress Progress.Status writes.
 */
export function startupStatusText(startup: Pick<TransportStartup, 'detail' | 'updatingTo'>): string {
  const detail = startup.detail || 'Starting';
  if (!startup.updatingTo) return detail;
  return `Finishing update to ${displayVersion(startup.updatingTo)}: ${lowerFirst(detail)}`;
}

/** The step, as "Step 3 of 7", or '' when the phase reports no steps. */
export function startupStepText(startup: Pick<TransportStartup, 'step' | 'steps'>): string {
  return startup.steps > 0 ? `Step ${startup.step} of ${startup.steps}` : '';
}

/**
 * The step and elapsed time, as "Step 3 of 7 · 0:12 elapsed", or only the
 * parts the report has. The Windows launcher's loading page writes the
 * same line (cmd/agent-overflow-windows/picker.go loadingScript).
 */
export function startupMetaText(startup: Pick<TransportStartup, 'step' | 'steps' | 'elapsedMs'>): string {
  const parts: string[] = [];
  const step = startupStepText(startup);
  if (step) parts.push(step);
  if (startup.elapsedMs >= 1000) {
    const seconds = Math.floor(startup.elapsedMs / 1000);
    parts.push(`${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')} elapsed`);
  }
  return parts.join(' · ');
}
