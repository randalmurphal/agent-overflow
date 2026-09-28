import { vi, type Mock } from 'vitest';
import { PatchParser, type PatchTextSource, type ReviewFile } from '../../lib/utils/patchStore';

// A patch streamed into compact storage the way ReviewDiffSource streams
// it: chunk by chunk into a store that can read an evicted chunk again
// from its source. Offsets are characters. Set the budget with
// setPatchTextBudgetForTest before streaming, and dispose the store.

export interface StreamedPatch {
  parser: PatchParser;
  files: ReviewFile[];
  source: PatchTextSource & { read: Mock<PatchTextSource['read']>; release: Mock<PatchTextSource['release']> };
  spans: { offset: number; nextOffset: number }[];
}

export function streamPatch(patch: string, size: number | ((index: number) => number)): StreamedPatch {
  const spans: { offset: number; nextOffset: number }[] = [];
  for (let offset = 0, index = 0; offset < patch.length; index += 1) {
    const next = Math.min(patch.length, offset + (typeof size === 'number' ? size : size(index)));
    spans.push({ offset, nextOffset: next });
    offset = next;
  }
  const source = {
    read: vi.fn<PatchTextSource['read']>(async (offset, nextOffset) => {
      const span = spans.find((candidate) => candidate.offset === offset);
      if (!span || span.nextOffset !== nextOffset) throw new Error(`unexpected read ${offset}-${nextOffset}`);
      return { data: patch.slice(offset, nextOffset), nextOffset };
    }),
    release: vi.fn<PatchTextSource['release']>(),
  };
  const parser = new PatchParser();
  parser.store.attachSource(source);
  for (const span of spans) parser.append(patch.slice(span.offset, span.nextOffset), span);
  parser.end();
  return { parser, files: parser.files, source, spans };
}
