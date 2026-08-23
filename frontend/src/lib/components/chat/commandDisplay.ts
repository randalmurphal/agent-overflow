import type { Item, CommandOutputMeta } from '../../types/models';
import { parseJsonObject } from '../../utils/parseJsonObject';

export interface CommandRowError {
  code?: string;
  msg: string;
}

export const COMMAND_TOOL_NAMES = new Set(['Bash', 'command_execution', 'commandExecution', 'exec_command', 'shell']);

export function isCommandToolName(toolName: string | undefined | null): boolean {
  const raw = (toolName ?? '').trim();
  return COMMAND_TOOL_NAMES.has(raw);
}

export function commandTextForItem(
  item: Item,
  meta: CommandOutputMeta | null | undefined,
): string {
  const metaCommand = meta?.command?.trim();
  if (metaCommand) return metaCommand;

  const itemMetaCommand = commandFromJSON(item.meta);
  if (itemMetaCommand) return itemMetaCommand;

  const payloadMetaCommand = commandFromJSON(item.payloadMeta);
  if (payloadMetaCommand) return payloadMetaCommand;

  return commandFromSummary(item.summary);
}

export function commandErrorForItem(
  meta: CommandOutputMeta | null | undefined,
  itemMeta: Record<string, unknown> | null,
  payloadMeta: Record<string, unknown> | null,
): CommandRowError {
  // New command_output rows should arrive with normalized errorMessage
  // in payload meta. The provider-specific probes below are legacy
  // fallbacks for older persisted Claude/Codex rows with no command
  // payload metadata.
  const code =
    readCommandExitCode(payloadMeta) ??
    readCommandExitCode(itemMeta) ??
    meta?.exitCode;
  const message =
    compactErrorMessage(readString(payloadMeta, 'errorMessage')) ||
    compactErrorMessage(readString(itemMeta, 'errorMessage')) ||
    compactErrorMessage(readString(payloadMeta, 'err_msg')) ||
    compactErrorMessage(readString(itemMeta, 'err_msg')) ||
    compactErrorMessage(readNestedString(itemMeta, ['tool_use_result', 'stderr'])) ||
    compactErrorMessage(readNestedString(itemMeta, ['tool_use_result', 'stdout'])) ||
    compactErrorMessage(readNestedString(itemMeta, ['tool_result', 'content'])) ||
    compactErrorMessage(meta?.errorMessage) ||
    'command failed';
  const codeLabel = typeof code === 'number' && Number.isFinite(code) ? `exit ${code}` : undefined;
  return { code: codeLabel, msg: message };
}

export function terminalInteractionLabelFromSummary(summary: string | undefined): string {
  const raw = (summary ?? '').trim();
  if (!raw) return 'Waited for background terminal';
  return splitTerminalInteractionSummary(raw)?.label ?? raw;
}

function commandFromJSON(raw: string | undefined): string {
  const parsed = parseJsonObject(raw);
  return parsed ? commandFromRecord(parsed) : '';
}

function readCommandExitCode(record: Record<string, unknown> | null): number | null {
  if (!record) return null;
  const camel = record.exitCode;
  if (typeof camel === 'number' && Number.isFinite(camel)) return camel;
  const snake = record.exit_code;
  if (typeof snake === 'number' && Number.isFinite(snake)) return snake;
  return null;
}

function readString(record: Record<string, unknown> | null, key: string): string {
  if (!record) return '';
  const value = record[key];
  return typeof value === 'string' ? value : '';
}

function readNestedString(record: Record<string, unknown> | null, path: string[]): string {
  let current: unknown = record;
  for (const key of path) {
    if (!current || typeof current !== 'object') return '';
    current = (current as Record<string, unknown>)[key];
  }
  if (typeof current === 'string') return current;
  if (Array.isArray(current)) {
    return current
      .map((entry) => {
        if (!entry || typeof entry !== 'object') return '';
        const text = (entry as Record<string, unknown>).text;
        return typeof text === 'string' ? text : '';
      })
      .filter(Boolean)
      .join('\n');
  }
  return '';
}

function compactErrorMessage(value: string | undefined): string {
  if (!value) return '';
  const cleaned = value
    .replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, '')
    .split('\n')
    .map((line) => line.replace(/\s+/g, ' ').trim())
    .filter(Boolean);
  if (cleaned.length === 0) return '';
  const message = cleaned.slice(-2).join(' ');
  return message.length > 240 ? `${message.slice(0, 239).trim()}…` : message;
}

function commandFromRecord(record: Record<string, unknown>): string {
  const command = record.command;
  if (typeof command === 'string' && command.trim()) return command.trim();

  const input = record.input;
  if (input && typeof input === 'object') {
    const inputCommand = (input as Record<string, unknown>).command;
    if (typeof inputCommand === 'string' && inputCommand.trim()) {
      return inputCommand.trim();
    }
  }

  return '';
}

function commandFromSummary(summary: string | undefined): string {
  const raw = (summary ?? '').trim();
  if (!raw) return '';
  const terminal = splitTerminalInteractionSummary(raw);
  if (terminal) {
    return terminal.commandSummary ? commandFromSummary(terminal.commandSummary) : '';
  }
  const colonIndex = raw.indexOf(':');
  const withoutToolPrefix =
    colonIndex >= 0 && isCommandToolName(raw.slice(0, colonIndex))
      ? raw.slice(colonIndex + 1).trimStart()
      : raw;
  return withoutToolPrefix
    .replace(/\s+->\s+(?:done|failed|killed|interrupted|error|exit\s+-?\d+)$/i, '')
    .replace(/\s+\((?:exit\s+-?\d+|error|failed|killed|declined)\)$/i, '')
    .trim();
}

const TERMINAL_INTERACTION_PREFIXES = [
  'Waited for background terminal',
  'Interacted with background terminal',
];

function splitTerminalInteractionSummary(raw: string): { label: string; commandSummary: string } | null {
  for (const prefix of TERMINAL_INTERACTION_PREFIXES) {
    if (raw === prefix) return { label: prefix, commandSummary: '' };
    if (raw.startsWith(`${prefix}:`)) {
      return {
        label: prefix,
        commandSummary: raw.slice(prefix.length + 1).trimStart(),
      };
    }
  }
  return null;
}

export function stripShellWrapper(command: string): string {
  const raw = command.trim();
  if (!raw) return raw;

  const words = splitShellWords(raw);
  if (!words) return stripTruncatedShellWrapper(raw);
  if (words.length < 3) return raw;

  const shell = basename(words[0]);
  if ((shell === 'bash' || shell === 'zsh') && words[1] === '-lc') {
    return words.slice(2).join(' ') || raw;
  }
  return raw;
}

function stripTruncatedShellWrapper(raw: string): string {
  const match = raw.match(/^(\S*[/\\])?(?:bash|zsh)\s+-lc\s+(.+)$/);
  if (!match) return raw;
  const script = (match[2] ?? '').trimStart();
  if (script.startsWith("'") || script.startsWith('"')) {
    return script.slice(1);
  }
  return script;
}

function basename(path: string): string {
  const normalized = path.replace(/\\/g, '/');
  const idx = normalized.lastIndexOf('/');
  return idx >= 0 ? normalized.slice(idx + 1) : normalized;
}

// Splits on unquoted whitespace with POSIX-ish quoting: single quotes
// are literal, double quotes honor backslash escapes, a bare backslash
// escapes the next character. Returns null for an unterminated quote or
// a trailing escape (a truncated preview), so the caller can fall back.
//
// The word under construction is a run of SLICES of the input, cut only
// at the characters the splitter consumes (quotes, escapes, separators);
// the previous per-character `current += char` minted a cons string per
// character, which was the single largest V8 allocation site while a
// command streamed (5MB/min, 2026-08-23 profile).
export function splitShellWords(input: string): string[] | null {
  const words: string[] = [];
  let current = '';
  // Start of the slice of `input` not yet appended to `current`.
  let segmentStart = 0;
  let quote: "'" | '"' | null = null;
  let escaped = false;
  let sawToken = false;

  const consume = (index: number): void => {
    if (index > segmentStart) current += input.slice(segmentStart, index);
    segmentStart = index + 1;
  };

  for (let i = 0; i < input.length; i += 1) {
    const char = input[i];
    if (escaped) {
      // The escaped character stays in the word: the segment simply
      // continues through it.
      sawToken = true;
      escaped = false;
      continue;
    }

    if (quote === "'") {
      if (char === "'") {
        consume(i);
        quote = null;
      } else {
        sawToken = true;
      }
      continue;
    }

    if (quote === '"') {
      if (char === '"') {
        consume(i);
        quote = null;
      } else if (char === '\\') {
        consume(i);
        escaped = true;
      } else {
        sawToken = true;
      }
      continue;
    }

    if (char === '\\') {
      consume(i);
      escaped = true;
      sawToken = true;
      continue;
    }
    if (char === "'" || char === '"') {
      consume(i);
      quote = char;
      sawToken = true;
      continue;
    }
    if (/\s/.test(char)) {
      consume(i);
      if (sawToken) {
        words.push(current);
        current = '';
        sawToken = false;
      }
      continue;
    }
    sawToken = true;
  }

  if (escaped || quote !== null) return null;
  consume(input.length);
  if (sawToken) words.push(current);
  return words;
}
