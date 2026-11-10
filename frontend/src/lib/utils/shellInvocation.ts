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
 * Each attachment claims the first unclaimed run of parts (between join
 * separators) that is exactly its invocation; the row shows a command block
 * for the attachment, so showing the typed line too would say it twice. A
 * command can hold the separator itself (a heredoc), so its invocation can
 * span several parts, as `turnAttachments.promptText` matches it. Text that
 * is not exactly an invocation is the person's own and stays.
 */
export function textWithoutShellInvocations(text: string, shells: readonly ShellOutputMeta[]): string {
  if (shells.length === 0 || !text.includes(SHELL_PREFIX)) return text;
  const parts = text.split(MESSAGE_JOIN_SEPARATOR);
  const claimed = new Set<number>();
  for (const shell of shells) {
    const invocation = shellInvocation(shell.command);
    // A row's metadata bounds the command, so a truncated one is matched by
    // its start, in the part that begins it.
    const span = shell.commandTruncated ? 1 : invocation.split(MESSAGE_JOIN_SEPARATOR).length;
    for (let at = 0; at + span <= parts.length; at++) {
      let free = true;
      for (let k = at; k < at + span; k++) free &&= !claimed.has(k);
      if (!free) continue;
      const typed = parts.slice(at, at + span).join(MESSAGE_JOIN_SEPARATOR).trim();
      if (shell.commandTruncated ? !typed.startsWith(invocation) : typed !== invocation) continue;
      for (let k = at; k < at + span; k++) claimed.add(k);
      break;
    }
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

// Terminal control that is not color: cursor movement and erasure (CSI with
// any final byte but `m`), character-set selection and keypad modes. Color
// (SGR) and OSC are left for AnsiText, which renders one and drops the other.
const ESC = String.fromCharCode(27);
const NON_COLOR_CONTROL = new RegExp(`${ESC}\\[[0-?]*[ -/]*[@-ln-~]|${ESC}[()][0-9A-Za-z]|${ESC}[=>]`, 'g');

/**
 * Command output as a terminal would leave it on screen: cursor control is
 * dropped and a carriage return restarts its line, so a progress bar reads
 * as its latest frame instead of every frame it drew.
 */
export function shellDisplayText(output: string): string {
  if (!output.includes(ESC) && !output.includes('\r')) return output;
  return output
    .replace(NON_COLOR_CONTROL, '')
    .split('\n')
    .map((line) => {
      const body = line.endsWith('\r') ? line.slice(0, -1) : line;
      const redraw = body.lastIndexOf('\r');
      return redraw >= 0 ? body.slice(redraw + 1) : body;
    })
    .join('\n');
}

/** The last `maxLines` lines of a live output stream, as shellDisplayText shows them. */
export function shellOutputTail(output: string, maxLines: number): string {
  if (output === '' || maxLines <= 0) return '';
  // A trailing newline ends the last line; it does not start an empty one.
  const text = output.endsWith('\n') ? output.slice(0, -1) : output;
  const lines = text.split('\n');
  return shellDisplayText(lines.slice(Math.max(0, lines.length - maxLines)).join('\n'));
}
