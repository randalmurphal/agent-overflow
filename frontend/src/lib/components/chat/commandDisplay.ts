import type { Item, CommandOutputMeta } from '../../types/models';

export const COMMAND_TOOL_NAMES = new Set(['Bash', 'command_execution', 'commandExecution']);

export function isCommandToolName(toolName: string | undefined | null): boolean {
  const raw = (toolName ?? '').trim();
  return COMMAND_TOOL_NAMES.has(raw);
}

export function commandLabelForStatus(status: string | undefined): string {
  return status === 'running' || status === 'streaming' ? 'Running' : 'Ran';
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

export function displayCommandForItem(
  item: Item,
  meta: CommandOutputMeta | null | undefined,
): string {
  return stripShellWrapper(commandTextForItem(item, meta));
}

function commandFromJSON(raw: string | undefined): string {
  if (!raw) return '';
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object') return '';
    return commandFromRecord(parsed as Record<string, unknown>);
  } catch {
    return '';
  }
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

export function stripShellWrapper(command: string): string {
  const raw = command.trim();
  if (!raw) return raw;

  const words = splitShellWords(raw);
  if (!words || words.length < 3) return raw;

  const shell = basename(words[0]);
  if ((shell === 'bash' || shell === 'zsh') && words[1] === '-lc') {
    return words[2] ?? raw;
  }
  return raw;
}

function basename(path: string): string {
  const normalized = path.replace(/\\/g, '/');
  const idx = normalized.lastIndexOf('/');
  return idx >= 0 ? normalized.slice(idx + 1) : normalized;
}

export function splitShellWords(input: string): string[] | null {
  const words: string[] = [];
  let current = '';
  let quote: "'" | '"' | null = null;
  let escaped = false;
  let sawToken = false;

  for (const char of input) {
    if (escaped) {
      current += char;
      sawToken = true;
      escaped = false;
      continue;
    }

    if (quote === "'") {
      if (char === "'") {
        quote = null;
      } else {
        current += char;
        sawToken = true;
      }
      continue;
    }

    if (quote === '"') {
      if (char === '"') {
        quote = null;
      } else if (char === '\\') {
        escaped = true;
      } else {
        current += char;
        sawToken = true;
      }
      continue;
    }

    if (char === '\\') {
      escaped = true;
      sawToken = true;
      continue;
    }
    if (char === "'" || char === '"') {
      quote = char;
      sawToken = true;
      continue;
    }
    if (/\s/.test(char)) {
      if (sawToken) {
        words.push(current);
        current = '';
        sawToken = false;
      }
      continue;
    }
    current += char;
    sawToken = true;
  }

  if (escaped || quote !== null) return null;
  if (sawToken) words.push(current);
  return words;
}
