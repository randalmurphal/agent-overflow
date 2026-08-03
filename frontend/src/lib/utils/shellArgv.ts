// One command line ⇄ one argv.
//
// Worktree setup commands are stored and executed as argv — no shell, nothing
// expanded (see internal/worktreesetup). Typing an argv array into a settings
// form is miserable, so the editor accepts a single command-line string per row
// and converts here. The conversion is deliberately narrow: it understands
// quoting well enough to write a command the way you would type it, and it
// refuses anything it cannot represent instead of guessing.
//
// What is supported: whitespace separation, '…' (fully literal), "…"
// (backslash escapes \" \\ \$ \` and a line-continuation \newline), and bare
// backslash escaping outside quotes. What is NOT: variable expansion, globbing,
// operators (| && ; > <), subshells. Those are shell features, and a recipe
// that wants a shell asks for one explicitly — `sh -c '…'` — which this
// tokenizer handles as three arguments, exactly as intended.

/** Thrown for input the argv form cannot represent. */
export class CommandLineError extends Error {}

const DOUBLE_QUOTE_ESCAPABLE = new Set(['"', '\\', '$', '`']);

/**
 * Split a command line into argv. Throws CommandLineError on an unterminated
 * quote or a trailing escape. Returns [] for blank input.
 */
export function tokenizeCommandLine(line: string): string[] {
  const argv: string[] = [];
  let current = '';
  let started = false;
  let index = 0;

  const push = () => {
    if (started) argv.push(current);
    current = '';
    started = false;
  };

  while (index < line.length) {
    const char = line[index];

    if (char === ' ' || char === '\t' || char === '\n' || char === '\r') {
      push();
      index += 1;
      continue;
    }

    if (char === "'") {
      const close = line.indexOf("'", index + 1);
      if (close === -1) throw new CommandLineError('Unterminated single quote.');
      current += line.slice(index + 1, close);
      started = true;
      index = close + 1;
      continue;
    }

    if (char === '"') {
      index += 1;
      started = true;
      let closed = false;
      while (index < line.length) {
        const inner = line[index];
        if (inner === '"') {
          closed = true;
          index += 1;
          break;
        }
        if (inner === '\\') {
          const next = line[index + 1];
          if (next === undefined) throw new CommandLineError('Trailing backslash.');
          // Inside double quotes a backslash is literal unless it escapes one
          // of the four characters the shell lets it escape there.
          if (next === '\n') {
            index += 2;
            continue;
          }
          current += DOUBLE_QUOTE_ESCAPABLE.has(next) ? next : '\\' + next;
          index += 2;
          continue;
        }
        current += inner;
        index += 1;
      }
      if (!closed) throw new CommandLineError('Unterminated double quote.');
      continue;
    }

    if (char === '\\') {
      const next = line[index + 1];
      if (next === undefined) throw new CommandLineError('Trailing backslash.');
      if (next !== '\n') {
        current += next;
        started = true;
      }
      index += 2;
      continue;
    }

    current += char;
    started = true;
    index += 1;
  }

  push();
  return argv;
}

// Bare words: everything a POSIX shell would leave alone. Anything else gets
// single-quoted, which is the only fully literal form.
const SAFE_WORD = /^[A-Za-z0-9_@%+=:,./-]+$/;

/** Render one argument the way it would be typed. */
export function quoteArgument(argument: string): string {
  if (argument === '') return "''";
  if (SAFE_WORD.test(argument)) return argument;
  return "'" + argument.replaceAll("'", `'\\''`) + "'";
}

/**
 * Render an argv as one command line. `tokenizeCommandLine(formatArgv(a))`
 * equals `a` for every argv.
 */
export function formatArgv(argv: readonly string[]): string {
  return argv.map(quoteArgument).join(' ');
}
