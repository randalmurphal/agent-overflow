import { render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewLineBlockRow from './ReviewLineBlockRow.svelte';
import { parseReviewFiles } from '../../utils/patchStore';
import type { PatchDisplayRow } from '../../utils/patchFiles';

const file = parseReviewFiles([
  'diff --git a/src/a.ts b/src/a.ts',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1,0 +1,2 @@',
  '+one',
  '+two',
].join('\n'))[0];

function row(id: string, newLine: number, content: string, pending = false): PatchDisplayRow {
  const out: PatchDisplayRow = { id, line: { content, type: 'add' }, oldLine: 0, newLine, side: 'new', lineIndex: newLine + 3 };
  if (pending) out.pending = true;
  return out;
}

describe('<ReviewLineBlockRow>', () => {
  it('offers no comment on a row whose text is still being read', () => {
    const onAddComment = vi.fn();
    const view = render(ReviewLineBlockRow, {
      rows: [row('r1', 1, '+one'), row('r2', 2, '', true)],
      file,
      path: 'src/a.ts',
      subjectId: 'thread-1',
      wordWrap: false,
      gutterCh: 2,
      onAddComment,
    });

    expect(view.getAllByTestId('review-add-comment')).toHaveLength(1);
  });
});
