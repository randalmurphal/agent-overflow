import { describe, expect, it } from 'vitest';
import vectors from '../../test/fixtures/shellInvocationVectors.json';
import {
  MESSAGE_JOIN_SEPARATOR,
  formatShellDuration,
  isShellInput,
  shellCommandFromInput,
  shellDisplayText,
  shellInvocationRuns,
  shellOutcomeLabel,
  shellOutputTail,
  shellTerminalPrompt,
  textWithoutShellInvocations,
  type ShellOutputMeta,
} from './shellInvocation';

function shell(command: string, extra: Partial<ShellOutputMeta> = {}): ShellOutputMeta {
  return { command, exitCode: 0, durationMs: 10, ...extra };
}

describe('shell input', () => {
  it('is a command only when the first character is the prefix', () => {
    expect(shellCommandFromInput('!git status')).toBe('git status');
    expect(shellCommandFromInput('!  ls -la  \n')).toBe('ls -la');
    expect(shellCommandFromInput('!echo a\necho b')).toBe('echo a\necho b');
    expect(shellCommandFromInput(' !git status')).toBeNull();
    expect(shellCommandFromInput('run !this')).toBeNull();
    expect(shellCommandFromInput('')).toBeNull();
  });

  it('is shell mode before any command is typed, with no command to run', () => {
    expect(isShellInput('!')).toBe(true);
    expect(isShellInput('! ')).toBe(true);
    expect(shellCommandFromInput('!')).toBeNull();
    expect(shellCommandFromInput('!   ')).toBeNull();
    expect(isShellInput('hello!')).toBe(false);
  });

});

// The cases the Go rule runs too (internal/usermessage/shell_invocation_test.go
// keeps the two files byte-identical). Each command comes with the row
// metadata Go projects for it, which is all this side is given.
describe('the shared invocation vectors', () => {
  it('were written for this separator', () => {
    expect(vectors.separator).toBe(MESSAGE_JOIN_SEPARATOR);
    expect(vectors.cases.length).toBeGreaterThan(0);
  });

  it.each(vectors.cases)('$name', ({ text, commands, runs, without }) => {
    const shells = commands.map(({ meta }) => shell(meta.command, { commandLength: (meta as { commandLength?: number }).commandLength }));
    const want = runs.map((run) => (run ? { first: run[0], last: run[1] } : null));
    expect(shellInvocationRuns(text.split(MESSAGE_JOIN_SEPARATOR), shells)).toEqual(want);
    expect(textWithoutShellInvocations(text, shells)).toBe(without);
  });
});

describe('formatting', () => {
  it('formats durations at the precision a reader uses', () => {
    expect(formatShellDuration(0)).toBe('0ms');
    expect(formatShellDuration(340)).toBe('340ms');
    expect(formatShellDuration(1_240)).toBe('1.2s');
    expect(formatShellDuration(12_600)).toBe('13s');
    expect(formatShellDuration(125_000)).toBe('2m 05s');
    expect(formatShellDuration(3_725_000)).toBe('1h 02m');
    expect(formatShellDuration(-1)).toBe('');
    expect(formatShellDuration(Number.NaN)).toBe('');
  });

  it('labels how a command ended', () => {
    expect(shellOutcomeLabel({ exitCode: 0 })).toBe('ok');
    expect(shellOutcomeLabel({ exitCode: 2 })).toBe('exit 2');
    expect(shellOutcomeLabel({ exitCode: -1 })).toBe('killed');
    expect(shellOutcomeLabel({ exitCode: 124, timedOut: true })).toBe('timed out');
  });
});

describe('shellDisplayText', () => {
  it('returns plain output untouched', () => {
    const plain = 'a\nb\n';
    expect(shellDisplayText(plain)).toBe(plain);
  });

  it('drops cursor control and keeps color for the renderer', () => {
    expect(shellDisplayText('\x1b[2K\x1b[1Gdone\x1b[K\n')).toBe('done\n');
    expect(shellDisplayText('\x1b[?25l\x1b[31mred\x1b[0m\x1b[?25h')).toBe('\x1b[31mred\x1b[0m');
    expect(shellDisplayText('\x1b(B\x1b=ok\x1b>')).toBe('ok');
  });

  it('shows a redrawn line as its latest frame, on every line', () => {
    expect(shellDisplayText('10%\r50%\r100%\nnext\r\n')).toBe('100%\nnext\n');
    expect(shellDisplayText('a\rb\r')).toBe('b');
  });
});

describe('shellOutputTail', () => {
  it('keeps the last lines and ignores the final line break', () => {
    expect(shellOutputTail('a\nb\nc\nd\n', 2)).toBe('c\nd');
    expect(shellOutputTail('a\nb', 5)).toBe('a\nb');
    expect(shellOutputTail('', 5)).toBe('');
    expect(shellOutputTail('a\n', 0)).toBe('');
  });

  it('keeps an unfinished last line', () => {
    expect(shellOutputTail('done\nworking', 2)).toBe('done\nworking');
  });

  it('shows a redrawn line as its latest frame', () => {
    expect(shellOutputTail('start\n10%\r50%\r100%\ndone\n', 3)).toBe('start\n100%\ndone');
    expect(shellOutputTail('a\r\nb\r\n', 2)).toBe('a\nb');
    expect(shellOutputTail('10%\r20%\r', 1)).toBe('20%');
  });
});

describe('shellTerminalPrompt', () => {
  it('is the last lines of a terminal whose last line is open', () => {
    expect(shellTerminalPrompt('[sudo] password for me: ', 3)).toBe('[sudo] password for me: ');
    expect(shellTerminalPrompt('a\nb\nSorry, try again.\nPassword: ', 2)).toBe('Sorry, try again.\nPassword: ');
  });

  it('is empty once the terminal ends its line, and for an empty terminal', () => {
    // The line break after an answer, typed or written by the command.
    expect(shellTerminalPrompt('[sudo] password for me: \n', 3)).toBe('');
    expect(shellTerminalPrompt('Continue (yes/no)? yes\n', 3)).toBe('');
    expect(shellTerminalPrompt('', 3)).toBe('');
  });
});
