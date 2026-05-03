import { describe, expect, it } from 'vitest';
import { parseClaudeSubagentTranscript } from './claudeSubagentTranscript';

describe('parseClaudeSubagentTranscript', () => {
  it('renders Claude sidechain JSONL into transcript entries', () => {
    const raw = [
      JSON.stringify({
        isSidechain: true,
        agentId: 'agent-1',
        type: 'user',
        message: { role: 'user', content: 'Run the command' },
      }),
      JSON.stringify({
        isSidechain: true,
        agentId: 'agent-1',
        type: 'assistant',
        message: {
          role: 'assistant',
          content: [
            {
              type: 'tool_use',
              id: 'tool-1',
              name: 'Bash',
              input: { command: 'echo done', description: 'Print done' },
            },
          ],
        },
      }),
      JSON.stringify({
        isSidechain: true,
        agentId: 'agent-1',
        type: 'user',
        message: {
          role: 'user',
          content: [
            {
              type: 'tool_result',
              tool_use_id: 'tool-1',
              content: 'done',
              is_error: false,
            },
          ],
        },
      }),
      JSON.stringify({
        isSidechain: true,
        agentId: 'agent-1',
        type: 'assistant',
        message: {
          role: 'assistant',
          content: [{ type: 'text', text: 'Finished.' }],
        },
      }),
    ].join('\n');

    expect(parseClaudeSubagentTranscript(raw)).toEqual([
      { kind: 'text', role: 'user', text: 'Run the command' },
      { kind: 'tool_use', toolName: 'Bash', summary: 'echo done' },
      { kind: 'tool_result', toolName: 'Bash', text: 'done', isError: false },
      { kind: 'text', role: 'assistant', text: 'Finished.' },
    ]);
  });

  it('returns null for plain command output', () => {
    expect(parseClaudeSubagentTranscript('line 1\nline 2\n')).toBeNull();
  });
});
