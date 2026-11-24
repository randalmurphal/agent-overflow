// A workload run in a bare `node --expose-gc` process, for a measurement
// the test worker's own heap is too noisy for (a whole-heap delta).
//
// A workload is an export of a real module, which the test imports for its
// descriptor: the import is what lets `--changed` select the test when the
// workload or anything it loads changes. The child imports the same module
// and calls the export, which measures and returns what to report.
//
// Node strips the types itself, but it resolves specifiers the way Node
// does: the app's imports are extensionless (`moduleResolution: bundler`),
// so the child registers a resolve hook that tries a relative specifier as
// written, then with `.ts`, then with `/index.ts`. The retry does not test
// the string for an extension: that cannot tell `./x.ts` from module names
// that carry a dot (`./parseBlocks.cache`). As written goes first because
// most relative specifiers a child resolves are a package's own, which
// resolve as written.
import { execFileSync } from 'node:child_process';

/** An export of a workload module: `url` is the module's `import.meta.url`,
 * `entry` names an export that takes no argument and returns a string. */
export type NodeWorkload = { readonly url: string; readonly entry: string };

function childSource(workload: NodeWorkload): string {
  return `
import nodeModule from 'node:module';
nodeModule.registerHooks({
  resolve(specifier, context, next) {
    if (specifier.startsWith('.')) {
      try { return next(specifier, context); } catch {}
      try { return next(specifier + '.ts', context); } catch {}
      return next(specifier + '/index.ts', context);
    }
    return next(specifier, context);
  },
});
// Dynamic, because a static import is hoisted above the hook registration.
const workload = await import(${JSON.stringify(workload.url)});
const result = await workload[${JSON.stringify(workload.entry)}]();
process.stdout.write(result);
`;
}

/** Runs `workload` in a fresh `node --expose-gc` with `env` added, and
 * returns the string it returned. */
export function runNodeWorkload(workload: NodeWorkload, env: Record<string, string>): string {
  return execFileSync(
    process.execPath,
    ['--expose-gc', '--input-type=module', '--eval', childSource(workload)],
    { encoding: 'utf8', env: { ...process.env, ...env }, timeout: 120_000 },
  );
}
