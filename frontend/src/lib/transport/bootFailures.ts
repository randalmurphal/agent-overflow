// The boot phases a backend reports failed without stopping its boot, such
// as a sweep of the previous instance's residue. Mirrors BootFailure in
// internal/transport; every hello of that backend process names them.

/** One boot phase that failed without stopping the boot. */
export interface BootFailure {
  /** The boot phase id (the backend's `boot: phase=` log name). */
  phase: string;
  /** The sentence a person reads for the phase. */
  detail: string;
  error: string;
}

// A boot has a few dozen phases at most and each fails once. Past these
// bounds a report is not one this build understands, and the strings are
// rendered as text.
const MAX_BOOT_FAILURES = 16;
const MAX_FIELD_CHARS = 600;

function text(value: unknown): string {
  return typeof value === 'string' ? value.slice(0, MAX_FIELD_CHARS) : '';
}

/** The failures a hello reports. Remote input: a field this build cannot
 *  read is neutralised, and an entry that is not an object is dropped. */
export function parseBootFailures(value: unknown): BootFailure[] {
  if (!Array.isArray(value)) return [];
  const failures: BootFailure[] = [];
  for (const entry of value.slice(0, MAX_BOOT_FAILURES)) {
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) continue;
    const failure = entry as Record<string, unknown>;
    failures.push({ phase: text(failure.phase), detail: text(failure.detail), error: text(failure.error) });
  }
  return failures;
}

export function sameBootFailures(a: readonly BootFailure[], b: readonly BootFailure[]): boolean {
  return a.length === b.length
    && a.every((failure, i) => failure.phase === b[i]!.phase && failure.detail === b[i]!.detail && failure.error === b[i]!.error);
}

/** The sentence the transport strip shows, or '' when nothing failed. */
export function bootFailureText(failures: readonly BootFailure[]): string {
  if (failures.length === 0) return '';
  const sentences = failures.map(({ detail, error }) => {
    const subject = detail ? `${detail} failed at startup` : 'A startup step failed';
    const sentence = error ? `${subject}: ${error}` : subject;
    return /[.!?]$/.test(sentence) ? sentence : `${sentence}.`;
  });
  return `${sentences.join(' ')} Retrying on next start.`;
}
