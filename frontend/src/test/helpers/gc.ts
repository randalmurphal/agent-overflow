import { setImmediate as nextMacrotask } from 'node:timers/promises';
import { setFlagsFromString } from 'node:v8';
import { runInNewContext } from 'node:vm';

/**
 * Collect garbage now, so a test can assert through a WeakRef that nothing
 * still holds an object. A WeakRef keeps its target alive until the job
 * that made it ends, so this waits for a real macrotask first (Node's own
 * timer, which fake timers do not replace).
 */
export async function collectGarbage(): Promise<void> {
  setFlagsFromString('--expose-gc');
  const gc = runInNewContext('gc') as () => void;
  await nextMacrotask();
  gc();
}
