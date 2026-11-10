import { describe, expect, it } from 'vitest';
import {
  MESSAGE_JOIN_SEPARATOR,
  formatShellDuration,
  isShellInput,
  shellCommandFromInput,
  shellInvocation,
  shellOutcomeLabel,
  shellOutputTail,
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

  it('round-trips through the stored invocation', () => {
    expect(shellCommandFromInput(shellInvocation('make test'))).toBe('make test');
  });
});

describe('textWithoutShellInvocations', () => {
  const sep = MESSAGE_JOIN_SEPARATOR;

  it('drops the typed line a command block stands for', () => {
    expect(textWithoutShellInvocations('!ls', [shell('ls')])).toBe('');
    expect(textWithoutShellInvocations(' !ls \n', [shell('ls')])).toBe('');
  });

  it('keeps the other parts of a joined message in order', () => {
    const text = `!make test${sep}fix those${sep}!ls`;
    expect(textWithoutShellInvocations(text, [shell('make test'), shell('ls')])).toBe('fix those');
    expect(textWithoutShellInvocations(text, [shell('ls')])).toBe(`!make test${sep}fix those`);
  });

  it('claims one part per attachment, so a repeated command is dropped once each', () => {
    const text = `!ls${sep}!ls${sep}!ls`;
    expect(textWithoutShellInvocations(text, [shell('ls'), shell('ls')])).toBe('!ls');
  });

  it('keeps text that is not exactly the invocation', () => {
    expect(textWithoutShellInvocations('!ls and explain', [shell('ls')])).toBe('!ls and explain');
    expect(textWithoutShellInvocations('ls', [shell('ls')])).toBe('ls');
    expect(textWithoutShellInvocations('!ls', [])).toBe('!ls');
  });

  it('matches a bounded command by its start', () => {
    const long = 'x'.repeat(600);
    const bounded = shell(long.slice(0, 512), { commandTruncated: true });
    expect(textWithoutShellInvocations(`!${long}`, [bounded])).toBe('');
    expect(textWithoutShellInvocations(`!${long}${sep}after`, [bounded])).toBe('after');
    // Without the flag the same prefix is a different, shorter command.
    expect(textWithoutShellInvocations(`!${long}`, [shell(long.slice(0, 512))])).toBe(`!${long}`);
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
