import { describe, expect, it } from 'vitest';
import { runNodeWorkload } from '../../test/helpers/nodeWorkload';
import { parseBlockRetentionWorkload } from './parseBlockRetention.workload';

// The workload runs in a bare `node --expose-gc` subprocess, because the
// measurement is a whole-heap delta and vitest's own heap is not quiet.

function retainedHeapBytes(detach: boolean, chainedSource = false, fullWindow = false): number {
  const output = runNodeWorkload(parseBlockRetentionWorkload, {
    AO_DETACH_PARSE_BLOCKS: detach ? '1' : '0',
    AO_CHAIN_PARSE_SOURCE: chainedSource ? '1' : '0',
    AO_FULL_PARSE_WINDOW: fullWindow ? '1' : '0',
  });
  const bytes = Number(output);
  if (!Number.isFinite(bytes)) {
    throw new Error(`parse-block retention subprocess returned ${JSON.stringify(output)}`);
  }
  return bytes;
}

describe('parse block backing retention', () => {
  it('does not retain one full parser checkpoint per completed block', () => {
    const retained = retainedHeapBytes(false, false, true);
    const detached = retainedHeapBytes(true);

    // Disable source-window trimming in the negative control: marked then
    // returns SlicedStrings backed by the full input, as before windowing.
    // The control retains about 35 MB for this 73 KB document. Detaching the
    // raws keeps only the final source and the 800 small block strings.
    expect(retained).toBeGreaterThan(20 * 1024 * 1024);
    expect(detached).toBeLessThan(3 * 1024 * 1024);
    expect(detached).toBeLessThan(retained / 8);
    // Bounded source windows also protect callers that do not materialize raws.
    expect(retainedHeapBytes(false)).toBeLessThan(3 * 1024 * 1024);
  });

  it('keeps completed blocks detached when the parser source is an append rope', () => {
    const retained = retainedHeapBytes(true, true);

    // The streaming boundary splitter normally keeps marked on bounded source
    // pieces. This stronger direct-parse case proves that even a growing
    // append rope cannot recreate the historical-checkpoint retention once
    // completed block raws are detached.
    expect(retained).toBeLessThan(3 * 1024 * 1024);
  });
});
