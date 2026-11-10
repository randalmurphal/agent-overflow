// Composer shell commands as text. A message that starts with `!` runs the
// rest as a command in the thread's workspace; the stored message keeps what
// was typed, and its output travels as a `file` attachment that records the
// command. Which parts of a message stand for which attachment is one rule,
// owned by `internal/usermessage/shell_invocation.go`; this is its copy, and
// both run the shared vectors (test/fixtures/shellInvocationVectors.json).
import { isWordSeparator } from './commandWords';

/** What a shell output attachment says about its command. */
export interface ShellOutputMeta {
  command: string;
  /**
   * Set when `command` is only the start of a longer one (timeline rows
   * only): the whole command's length in UTF-16 code units.
   */
  commandLength?: number;
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

/** A run of a message's joined parts, first to last inclusive. */
export interface PartRun {
  first: number;
  last: number;
}

/** `text` without surrounding white space, as Go's `strings.TrimSpace` reads it. */
function trimSpace(text: string): string {
  let start = 0;
  let end = text.length;
  while (start < end && isWordSeparator(text[start])) start++;
  while (end > start && isWordSeparator(text[end - 1])) end--;
  return text.slice(start, end);
}

/**
 * For each shell attachment in order, the parts of one message that are its
 * typed invocation, or null. `parts` is the message split on
 * MESSAGE_JOIN_SEPARATOR. Mirrors `usermessage.ShellInvocationRuns`:
 *
 * - an invocation is whole parts: the run's text, joined by the separator
 *   and trimmed of surrounding white space, is exactly `!command`. A command
 *   that holds the separator itself, as a heredoc can, is a run of several;
 * - attachments claim in order, each the first run no earlier one took.
 *
 * A row's metadata bounds the command, so a longer one is known here by its
 * start and its whole length, which together say where its run ends.
 */
export function shellInvocationRuns(
  parts: readonly string[],
  shells: readonly Pick<ShellOutputMeta, 'command' | 'commandLength'>[],
): (PartRun | null)[] {
  const claimed = new Set<number>();
  return shells.map((shell) => {
    const start = SHELL_PREFIX + shell.command;
    const length = SHELL_PREFIX.length + (shell.commandLength ?? shell.command.length);
    for (let first = 0; first < parts.length; first++) {
      const last = invocationEnd(parts, claimed, first, start, length);
      if (last < 0) continue;
      for (let part = first; part <= last; part++) claimed.add(part);
      return { first, last };
    }
    return null;
  });
}

/** The last part of the run starting at `first` whose text is the invocation, or -1. */
function invocationEnd(
  parts: readonly string[],
  claimed: ReadonlySet<number>,
  first: number,
  start: string,
  length: number,
): number {
  for (let last = first; last < parts.length && !claimed.has(last); last++) {
    const typed = trimSpace(parts.slice(first, last + 1).join(MESSAGE_JOIN_SEPARATOR));
    if (typed.length < length) continue;
    return typed.length === length && typed.startsWith(start) ? last : -1;
  }
  return -1;
}

/**
 * A message's text without the invocations its shell attachments stand for.
 * The row shows a command block for each attachment, so showing the typed
 * line too would say it twice. Text that is not exactly an invocation is the
 * person's own and stays.
 */
export function textWithoutShellInvocations(text: string, shells: readonly ShellOutputMeta[]): string {
  if (shells.length === 0 || !text.includes(SHELL_PREFIX)) return text;
  const parts = text.split(MESSAGE_JOIN_SEPARATOR);
  const claimed = new Set<number>();
  for (const run of shellInvocationRuns(parts, shells)) {
    for (let part = run?.first ?? 0; run && part <= run.last; part++) claimed.add(part);
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
