import { describe, it, expect } from 'vitest';
import {
  tokenizeCommandLine,
  formatArgv,
  quoteArgument,
  CommandLineError,
} from './shellArgv';

describe('tokenizeCommandLine', () => {
  it('splits on runs of whitespace', () => {
    expect(tokenizeCommandLine('pnpm install --frozen-lockfile')).toEqual([
      'pnpm',
      'install',
      '--frozen-lockfile',
    ]);
    expect(tokenizeCommandLine('  make \t install \n')).toEqual(['make', 'install']);
    expect(tokenizeCommandLine('')).toEqual([]);
    expect(tokenizeCommandLine('   ')).toEqual([]);
  });

  it('keeps single quotes fully literal', () => {
    expect(tokenizeCommandLine(`sh -c 'ln -s "$A/.env" "$B/.env"'`)).toEqual([
      'sh',
      '-c',
      'ln -s "$A/.env" "$B/.env"',
    ]);
    expect(tokenizeCommandLine(`echo '\\n'`)).toEqual(['echo', '\\n']);
  });

  it('honours the four escapes double quotes recognise and leaves the rest alone', () => {
    expect(tokenizeCommandLine('echo "a \\"b\\" c"')).toEqual(['echo', 'a "b" c']);
    expect(tokenizeCommandLine('echo "back\\\\slash"')).toEqual(['echo', 'back\\slash']);
    expect(tokenizeCommandLine('echo "\\$HOME"')).toEqual(['echo', '$HOME']);
    // \n inside double quotes is not an escape in a shell either.
    expect(tokenizeCommandLine('echo "a\\nb"')).toEqual(['echo', 'a\\nb']);
  });

  it('escapes outside quotes, including whitespace', () => {
    expect(tokenizeCommandLine('cp a\\ b c')).toEqual(['cp', 'a b', 'c']);
    expect(tokenizeCommandLine('echo \\"quoted\\"')).toEqual(['echo', '"quoted"']);
  });

  it('joins adjacent quoted and bare runs into one argument', () => {
    expect(tokenizeCommandLine(`--flag="a b"'c'd`)).toEqual(['--flag=a bcd']);
  });

  it('produces an empty argument for an empty quote pair', () => {
    expect(tokenizeCommandLine(`sh -c ''`)).toEqual(['sh', '-c', '']);
    expect(tokenizeCommandLine('sh -c ""')).toEqual(['sh', '-c', '']);
  });

  it('drops line continuations', () => {
    expect(tokenizeCommandLine('make \\\ninstall')).toEqual(['make', 'install']);
    expect(tokenizeCommandLine('echo "a\\\nb"')).toEqual(['echo', 'ab']);
  });

  it('refuses input the argv form cannot represent', () => {
    expect(() => tokenizeCommandLine(`sh -c 'unterminated`)).toThrow(CommandLineError);
    expect(() => tokenizeCommandLine('sh -c "unterminated')).toThrow(CommandLineError);
    expect(() => tokenizeCommandLine('make install \\')).toThrow(CommandLineError);
    expect(() => tokenizeCommandLine('echo "trailing \\')).toThrow(CommandLineError);
  });
});

describe('formatArgv', () => {
  it('leaves shell-safe words bare', () => {
    expect(formatArgv(['pnpm', 'install', '--frozen-lockfile'])).toBe(
      'pnpm install --frozen-lockfile',
    );
    expect(quoteArgument('/usr/bin/env')).toBe('/usr/bin/env');
  });

  it('single-quotes anything else, including embedded single quotes', () => {
    expect(quoteArgument('a b')).toBe("'a b'");
    expect(quoteArgument('')).toBe("''");
    expect(quoteArgument("it's")).toBe(`'it'\\''s'`);
    expect(quoteArgument('$HOME')).toBe("'$HOME'");
  });

  it('round-trips every argv through tokenize', () => {
    const cases: string[][] = [
      ['pnpm', 'install', '--frozen-lockfile'],
      ['sh', '-c', 'ln -s "$AO_PROJECT_ROOT/.env" "$AO_WORKTREE_PATH/.env"'],
      ['echo', "it's", 'a b', '', '\\', '"quoted"', '$HOME', 'tab\there'],
      ['a'],
      [],
    ];
    for (const argv of cases) {
      expect(tokenizeCommandLine(formatArgv(argv))).toEqual(argv);
    }
  });
});
