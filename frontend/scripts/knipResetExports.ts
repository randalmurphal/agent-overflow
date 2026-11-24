// knip preprocessor (knip.config.ts): drops the unused-export findings for
// exports the between-test resets call by name rather than by import
// (src/test/config/testResets.ts): every reset-named export but those in
// NOT_BETWEEN_TESTS, and each export a POSTURE or FIRST step names.
import type { Preprocessor } from 'knip';
import { relative } from 'node:path';
import { NOT_BETWEEN_TESTS, RESET_NAME, stepExports } from '../src/test/config/testResets.ts';

const stepped = stepExports();

function calledByName(file: string, symbol: string): boolean {
  if (stepped.get(file)?.has(symbol)) return true;
  return RESET_NAME.test(symbol) && !(`${file}#${symbol}` in NOT_BETWEEN_TESTS);
}

const dropResetExports: Preprocessor = (options) => {
  const { issues, counters } = options;
  for (const [filePath, symbols] of Object.entries(issues.exports)) {
    const file = relative(options.cwd, filePath);
    for (const symbol of Object.keys(symbols)) {
      if (!calledByName(file, symbol)) continue;
      delete symbols[symbol];
      counters.exports -= 1;
    }
    if (Object.keys(symbols).length === 0) delete issues.exports[filePath];
  }
  return options;
};

export default dropResetExports;
