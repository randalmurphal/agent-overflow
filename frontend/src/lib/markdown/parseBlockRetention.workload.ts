// The heap workload of parseBlockRetention.test.ts. It runs in a bare
// `node --expose-gc` child (src/test/helpers/nodeWorkload.ts), because the
// measurement is a whole-heap delta; the test imports this module for its
// descriptor only.
import type { NodeWorkload } from '../../test/helpers/nodeWorkload';
import {
  createMaterializedProvenAppend,
  createParseBlocksCache,
  createProvenAppend,
  parseBlocks,
  updateParseBlockStringMaterialization,
} from './parser/index';

declare const gc: () => void;

/** Heap bytes retained by streaming 800 paragraphs through the parser, with
 * the options the AO_* environment variables set. */
export function measureParseBlockRetention(): string {
  for (let index = 0; index < 4; index++) gc();
  const before = process.memoryUsage().heapUsed;
  const cache = createParseBlocksCache();
  if (process.env.AO_FULL_PARSE_WINDOW === '1') cache.source.retainFrom = () => {};
  let source = '';
  for (let index = 0; index < 800; index++) {
    const marker = index === 0 ? 'AO_PARSE_BLOCK_RETENTION ' : '';
    const delta = marker
      + 'Paragraph ' + String(index).padStart(4, '0')
      + ' keeps enough parser content to produce a non-trivial backing string while streaming.\n\n';
    const append = process.env.AO_CHAIN_PARSE_SOURCE === '1'
      ? createProvenAppend(source, delta)
      : createMaterializedProvenAppend(source, [delta]);
    source = append.next;
    parseBlocks(source, [], cache, append);
    if (process.env.AO_DETACH_PARSE_BLOCKS === '1') {
      updateParseBlockStringMaterialization(cache, true);
    }
  }
  (globalThis as { __aoParseBlockRetention?: unknown }).__aoParseBlockRetention = { cache, source };
  for (let index = 0; index < 4; index++) gc();
  // Read before touching process.stdout, whose first use builds the stream.
  return String(process.memoryUsage().heapUsed - before);
}

export const parseBlockRetentionWorkload: NodeWorkload = { url: import.meta.url, entry: 'measureParseBlockRetention' };
