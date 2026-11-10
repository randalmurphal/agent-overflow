// Composer shell commands as text. A message that starts with `!` runs the
// rest as a command in the thread's workspace; the stored message keeps what
// was typed, and its output travels as a `file` attachment that records the
// command. The Go twin of the invocation rule is `shellInvocation` and
// `turnAttachments.promptText` (internal/app).

/** What a shell output attachment says about its command. */
export interface ShellOutputMeta {
  command: string;
  /** `command` is the start of a longer one (timeline rows only). */
  commandTruncated?: boolean;
  exitCode: number;
  durationMs: number;
  /** The output outgrew the capture and was cut in the middle. */
  truncated?: boolean;
  /** The time limit ended the command. */
  timedOut?: boolean;
  /** The first lines of the output. */
  preview?: string;
  /** The output file holds more than `preview` (timeline rows only). */
  more?: boolean;
}

/** Mirrors `usermessage.JoinSeparator`: the rule between joined queued messages. */
export const MESSAGE_JOIN_SEPARATOR = '\n\n---\n\n';

const SHELL_PREFIX = '!';

/**
 * The command a composer text runs, or null when the text is a message. The
 * `!` must be the first character, so prose that merely contains one, or is
 * indented, is never run.
 */
export function shellCommandFromInput(text: string): string | null {
  if (!text.startsWith(SHELL_PREFIX)) return null;
  const command = text.slice(SHELL_PREFIX.length).trim();
  return command === '' ? null : command;
}

/** True while the composer text is in shell mode, before any command is typed. */
export function isShellInput(text: string): boolean {
  return text.startsWith(SHELL_PREFIX);
}

/** The message text a command is stored as. */
export function shellInvocation(command: string): string {
  return SHELL_PREFIX + command;
}

/**
 * A message's text without the invocations its shell attachments stand for.
 * Each attachment claims the first unclaimed part (between join separators)
 * that is exactly its invocation; the row shows a command block for the
 * attachment, so showing the typed line too would say it twice. Text that is
 * not exactly an invocation is the person's own and stays.
 */
export function textWithoutShellInvocations(text: string, shells: readonly ShellOutputMeta[]): string {
  if (shells.length === 0 || !text.includes(SHELL_PREFIX)) return text;
  const parts = text.split(MESSAGE_JOIN_SEPARATOR);
  const claimed = new Set<number>();
  for (const shell of shells) {
    const invocation = shellInvocation(shell.command);
    const at = parts.findIndex((part, index) => {
      if (claimed.has(index)) return false;
      const typed = part.trim();
      // A row's metadata bounds the command; the part still holds all of it.
      return shell.commandTruncated ? typed.startsWith(invocation) : typed === invocation;
    });
    if (at >= 0) claimed.add(at);
  }
  if (claimed.size === 0) return text;
  return parts.filter((_, index) => !claimed.has(index)).join(MESSAGE_JOIN_SEPARATOR);
}

/** `1.2s`, `340ms`, `2m 05s`: how long a command took. */
export function formatShellDuration(durationMs: number): string {
  if (!Number.isFinite(durationMs) || durationMs < 0) return '';
  if (durationMs < 1000) return `${Math.round(durationMs)}ms`;
  const seconds = durationMs / 1000;
  if (seconds < 60) return `${seconds < 10 ? seconds.toFixed(1) : Math.round(seconds)}s`;
  const whole = Math.round(seconds);
  const minutes = Math.floor(whole / 60);
  if (minutes < 60) return `${minutes}m ${String(whole % 60).padStart(2, '0')}s`;
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}

/** The outcome label of a finished command. */
export function shellOutcomeLabel(shell: Pick<ShellOutputMeta, 'exitCode' | 'timedOut'>): string {
  if (shell.timedOut) return 'timed out';
  if (shell.exitCode === 0) return 'ok';
  if (shell.exitCode < 0) return 'killed';
  return `exit ${shell.exitCode}`;
}

/**
 * The last `maxLines` lines of a live output stream as a terminal would show
 * them: a carriage return restarts its line, so a progress bar reads as its
 * latest frame instead of every frame it drew.
 */
export function shellOutputTail(output: string, maxLines: number): string {
  if (output === '' || maxLines <= 0) return '';
  // A trailing newline ends the last line; it does not start an empty one.
  const text = output.endsWith('\n') ? output.slice(0, -1) : output;
  const lines = text.split('\n');
  const kept = lines.slice(Math.max(0, lines.length - maxLines));
  return kept
    .map((line) => {
      const body = line.endsWith('\r') ? line.slice(0, -1) : line;
      const redraw = body.lastIndexOf('\r');
      return redraw >= 0 ? body.slice(redraw + 1) : body;
    })
    .join('\n');
}
