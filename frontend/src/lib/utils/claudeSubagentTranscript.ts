export type ClaudeSubagentTranscriptEntry =
  | {
      kind: 'text';
      role: 'assistant' | 'user';
      text: string;
    }
  | {
      kind: 'tool_use';
      toolName: string;
      summary: string;
    }
  | {
      kind: 'tool_result';
      toolName: string;
      text: string;
      isError: boolean;
    };

interface ClaudeTranscriptEnvelope {
  isSidechain?: boolean;
  agentId?: string;
  type?: string;
  toolUseResult?: unknown;
  message?: {
    role?: string;
    content?: unknown;
  };
}

interface ToolUseContent {
  type?: string;
  id?: string;
  name?: string;
  input?: Record<string, unknown>;
}

interface ToolResultContent {
  type?: string;
  tool_use_id?: string;
  content?: unknown;
  is_error?: boolean;
}

export function parseClaudeSubagentTranscript(raw: string): ClaudeSubagentTranscriptEntry[] | null {
  const lines = raw
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line.length > 0);
  if (lines.length === 0) return null;

  const entries: ClaudeSubagentTranscriptEntry[] = [];
  const toolNameById = new Map<string, string>();
  let sawSidechainEnvelope = false;
  let sawParseFailure = false;

  for (const line of lines) {
    let envelope: ClaudeTranscriptEnvelope;
    try {
      envelope = JSON.parse(line) as ClaudeTranscriptEnvelope;
    } catch {
      sawParseFailure = true;
      continue;
    }
    if (envelope.isSidechain === true || typeof envelope.agentId === 'string') {
      sawSidechainEnvelope = true;
    }

    const role = envelope.message?.role === 'assistant' ? 'assistant' : 'user';
    const content = envelope.message?.content;
    if (typeof content === 'string') {
      const text = content.trim();
      if (text) entries.push({ kind: 'text', role, text });
      continue;
    }
    if (!Array.isArray(content)) continue;

    for (const part of content) {
      if (!part || typeof part !== 'object') continue;
      const typed = part as ToolUseContent | ToolResultContent | { type?: string; text?: unknown };
      if (typed.type === 'text') {
        const textPart = typed as { text?: unknown };
        const text = typeof textPart.text === 'string' ? textPart.text.trim() : '';
        if (text) entries.push({ kind: 'text', role: 'assistant', text });
        continue;
      }
      if (typed.type === 'tool_use') {
        const tool = typed as ToolUseContent;
        const toolName = typeof tool.name === 'string' && tool.name.trim() ? tool.name.trim() : 'Tool';
        if (typeof tool.id === 'string' && tool.id.trim()) {
          toolNameById.set(tool.id.trim(), toolName);
        }
        entries.push({
          kind: 'tool_use',
          toolName,
          summary: summarizeToolInput(tool.input),
        });
        continue;
      }
      if (typed.type === 'tool_result') {
        const result = typed as ToolResultContent;
        const toolUseId = typeof result.tool_use_id === 'string' ? result.tool_use_id.trim() : '';
        const toolName = toolNameById.get(toolUseId) ?? 'Tool result';
        const text = toolResultText(result.content);
        if (text) {
          entries.push({
            kind: 'tool_result',
            toolName,
            text,
            isError: result.is_error === true,
          });
        }
      }
    }
  }

  if (!sawSidechainEnvelope || entries.length === 0) {
    return null;
  }
  if (sawParseFailure && entries.length === 0) {
    return null;
  }
  return entries;
}

function summarizeToolInput(input: Record<string, unknown> | undefined): string {
  if (!input) return '';
  for (const key of ['command', 'file_path', 'path', 'description', 'prompt']) {
    const value = input[key];
    if (typeof value === 'string' && value.trim()) {
      return value.trim();
    }
  }
  return JSON.stringify(input);
}

function toolResultText(content: unknown): string {
  if (typeof content === 'string') return content.trim();
  if (Array.isArray(content)) {
    return content
      .map((part) => {
        if (typeof part === 'string') return part;
        if (part && typeof part === 'object' && typeof (part as { text?: unknown }).text === 'string') {
          return (part as { text: string }).text;
        }
        return '';
      })
      .filter((text) => text.trim().length > 0)
      .join('\n')
      .trim();
  }
  return '';
}
