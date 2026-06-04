import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { __resetParseJsonObjectCacheForTest } from '../../utils/parseJsonObject';
import {
  commandTextForItem,
  displayCommandForItem,
  isCommandToolName,
  splitShellWords,
  stripShellWrapper,
  terminalInteractionLabelFromSummary,
} from './commandDisplay';

describe('commandDisplay', () => {
  it('classifies Claude Bash and Codex command_execution as command tools', () => {
    expect(isCommandToolName('Bash')).toBe(true);
    expect(isCommandToolName('command_execution')).toBe(true);
    expect(isCommandToolName('exec_command')).toBe(true);
    expect(isCommandToolName('Read')).toBe(false);
  });

  it('strips bash and zsh -lc wrappers for display', () => {
    expect(stripShellWrapper("bash -lc 'echo hello'")).toBe('echo hello');
    expect(stripShellWrapper("/usr/bin/zsh -lc 'git status --short'")).toBe('git status --short');
  });

  it('strips shell wrappers from truncated command previews with unterminated quotes', () => {
    const truncated = "/usr/bin/zsh -lc 'uv run pytest tests/unit/db/test_migration_steps.py tests/uni…";

    expect(stripShellWrapper(truncated)).toBe('uv run pytest tests/unit/db/test_migration_steps.py tests/uni…');
  });

  it('strips shell wrappers from unquoted shell preview text', () => {
    expect(stripShellWrapper('/usr/bin/zsh -lc uv run pytest')).toBe('uv run pytest');
  });

  it('preserves quoted scripts when splitting shell words', () => {
    const words = splitShellWords(
      `/bin/zsh -lc 'python3 -c '"'"'print("Hello, world!")'"'"''`,
    );

    expect(words).toEqual([
      '/bin/zsh',
      '-lc',
      `python3 -c 'print("Hello, world!")'`,
    ]);
  });

  it('does not rewrite non-shell-wrapper commands', () => {
    const command = String.raw`C:\Program Files\Git\bin\bash.exe -lc "echo hi"`;
    expect(stripShellWrapper(command)).toBe(command);
  });

  it('uses payload meta before summary and strips the wrapper for display', () => {
    const item = makeItem({
      toolName: 'command_execution',
      summary: "Bash: /usr/bin/zsh -lc 'git status --short'",
    });
    const meta = {
      command: "/usr/bin/zsh -lc 'git log --oneline -1'",
      exitCode: 0,
      lineCount: 1,
      preview: '',
    };

    expect(commandTextForItem(item, meta)).toBe("/usr/bin/zsh -lc 'git log --oneline -1'");
    expect(displayCommandForItem(item, meta)).toBe('git log --oneline -1');
  });

  it('uses the shared JSON cache for repeated item-meta command extraction', () => {
    __resetParseJsonObjectCacheForTest();
    const item = makeItem({
      meta: JSON.stringify({ input: { command: 'pwd' } }),
      payloadMeta: JSON.stringify({ command: 'fallback' }),
      summary: 'Bash: echo fallback',
    });
    const parseSpy = vi.spyOn(JSON, 'parse');

    try {
      expect(commandTextForItem(item, null)).toBe('pwd');
      expect(commandTextForItem(item, null)).toBe('pwd');
      expect(parseSpy).toHaveBeenCalledTimes(1);
    } finally {
      parseSpy.mockRestore();
    }
  });

  it('strips summary outcome suffixes from fallback command text', () => {
    const item = makeItem({
      toolName: 'Bash',
      summary: 'Bash: sleep 10 -> exit 137',
    });

    expect(commandTextForItem(item, null)).toBe('sleep 10');
    expect(displayCommandForItem(item, null)).toBe('sleep 10');
  });

  it('extracts commands from terminal-interaction summaries without treating the base label as a command', () => {
    const waited = makeItem({
      kind: 'terminal_interaction',
      summary: 'Waited for background terminal: Bash: sleep 1; echo done',
    });
    const interacted = makeItem({
      kind: 'terminal_interaction',
      summary: 'Interacted with background terminal: Bash: cat <<EOF',
    });
    const empty = makeItem({
      kind: 'terminal_interaction',
      summary: 'Waited for background terminal',
    });

    expect(commandTextForItem(waited, null)).toBe('sleep 1; echo done');
    expect(displayCommandForItem(waited, null)).toBe('sleep 1; echo done');
    expect(commandTextForItem(interacted, null)).toBe('cat <<EOF');
    expect(terminalInteractionLabelFromSummary(interacted.summary)).toBe('Interacted with background terminal');
    expect(commandTextForItem(empty, null)).toBe('');
    expect(displayCommandForItem(empty, null)).toBe('');
  });
});
