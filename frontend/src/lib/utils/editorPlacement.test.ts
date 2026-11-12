import { describe, expect, it, vi } from 'vitest';
import {
  fileIdentity,
  lineGuard,
  RelocationBudget,
  RelocationCancelled,
  relocateEditors,
  type EditorProof,
  type RelocationTarget,
} from './editorPlacement';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { mergePatchFilesByPath, parsePatchFiles } from './patchFiles';
import type { CommentAnchor } from './reviewRows';

const PATH = 'src/app.ts';

function section(opts: { path?: string; index?: string | null; meta?: string[]; hunk: string; lines: string[] }): string {
  const path = opts.path ?? PATH;
  return [
    `diff --git a/${path} b/${path}`,
    ...(opts.meta ?? []),
    ...(opts.index === null ? [] : [`index ${opts.index ?? '1111111..2222222'} 100644`]),
    `--- a/${path}`,
    `+++ b/${path}`,
    opts.hunk,
    ...opts.lines,
  ].join('\n');
}

function fileOf(patch: string, path = PATH): ReviewFile {
  const file = parseReviewFiles(patch).find((candidate) => candidate.path === path);
  if (!file) throw new Error(`no ${path}`);
  return file;
}

// Rows and bytes enough for every test file.
const budget = (): RelocationBudget => new RelocationBudget(1_000_000, 100_000_000);
const never = (): boolean => false;

function placedOn(file: ReviewFile, anchor: CommentAnchor, text: string): RelocationTarget {
  return {
    filePath: anchor.filePath,
    side: anchor.side,
    selectedText: text,
    guard: anchor.side === 'file' ? null : lineGuard(text),
    source: { file, anchor: { ...anchor, selectedText: text } },
    proof: { kind: 'none' },
    line: (anchor.side === 'old' ? anchor.oldLine : anchor.newLine) ?? 0,
  };
}

async function placeOne(target: RelocationTarget, next: readonly ReviewFile[]) {
  const out = await relocateEditors(new Map([['e', target]]), next, budget(), never);
  return out.get('e')!;
}

// Base: top / bottom. The old head adds a primary and a fallback handler.
const BASE_TO_A = section({
  index: 'aaaaaaa..bbbbbbb',
  hunk: '@@ -1,2 +1,8 @@',
  lines: [' top', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom'],
});

describe('fileIdentity', () => {
  it('reads the ids of the index line, a zero id as none', async () => {
    expect(await fileIdentity(fileOf(BASE_TO_A), budget())).toEqual({ oldBlob: 'aaaaaaa', newBlob: 'bbbbbbb', oldPath: null, lines: 'blob' });
    const added = fileOf(section({ index: '0000000..1111111', meta: ['new file mode 100644'], hunk: '@@ -0,0 +1,1 @@', lines: ['+x'] }));
    expect(await fileIdentity(added, budget())).toMatchObject({ oldBlob: null, newBlob: '1111111', lines: 'blob' });
  });

  it('proves no lines for a submodule or a binary section', async () => {
    const gitlink = fileOf(section({ index: 'abcdef0..1234567', hunk: '@@ -1 +1 @@', lines: ['-Subproject commit abcdef0', '+Subproject commit 1234567'] }).replace('100644', '160000'));
    expect((await fileIdentity(gitlink, budget())).lines).toBe('none');
    const binary = parseReviewFiles([
      `diff --git a/img.png b/img.png`,
      'index 1111111..2222222 100644',
      'Binary files a/img.png and b/img.png differ',
    ].join('\n'))[0];
    expect((await fileIdentity(binary, budget())).lines).toBe('none');
  });

  it('proves a section with no index line, and a merged one, by its bytes alone', async () => {
    const noIndex = fileOf(section({ index: null, hunk: '@@ -1,1 +1,1 @@', lines: ['-a', '+b'] }));
    expect((await fileIdentity(noIndex, budget())).lines).toBe('section');
    // Two edits of one path joined: the first section's ids describe
    // neither the second nor the whole.
    const merged = reviewFileFromPatchFile(mergePatchFilesByPath(parsePatchFiles([
      section({ index: '1111111..2222222', hunk: '@@ -1,1 +1,1 @@', lines: ['-a', '+b'] }),
      section({ index: '2222222..3333333', hunk: '@@ -9,1 +9,1 @@', lines: ['-c', '+d'] }),
    ].join('\n')))[0]);
    // The merge keeps the first section's header.
    expect((await fileIdentity(merged, budget())).lines).toBe('blob');
    const flagged = { ...merged, mergedSections: 2 };
    expect((await fileIdentity(flagged, budget())).lines).toBe('section');
  });

  it('reads the leading metadata and no line after it', async () => {
    const file = fileOf(section({ hunk: '@@ -1,400 +1,400 @@', lines: Array.from({ length: 400 }, (_, index) => ` line ${index}`) }));
    const kind = vi.spyOn(file.body, 'kind');
    const text = vi.spyOn(file.body, 'text');
    await fileIdentity(file, budget());
    // diff --git, index, ---, +++, then the hunk header ends the scan and
    // is checked to open a hunk.
    expect(kind.mock.calls.length).toBe(6);
    expect(text.mock.calls.length).toBe(4);
  });
});

describe('relocateEditors', () => {
  const report: CommentAnchor = { filePath: PATH, side: 'new', newLine: 3 };

  it('keeps an editor on its line in the same version of its file', async () => {
    const outcome = await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [fileOf(BASE_TO_A)]);
    expect(outcome.anchor).toMatchObject({ side: 'new', newLine: 3 });
    expect(outcome.proof).toMatchObject({ kind: 'blob', newBlob: 'bbbbbbb', newLine: 3 });
  });

  it('detaches the primary handler\'s editor when the head deletes it and the fallback now reports', async () => {
    // The base is unchanged, the regions are tiny and `report();` is on
    // one row each time: nothing but text says the fallback's line is
    // this one, and it is not.
    const next = fileOf(section({
      index: 'aaaaaaa..ccccccc',
      hunk: '@@ -1,2 +1,5 @@',
      lines: [' top', '+if (fallback) {', '+  report();', '+}', ' bottom'],
    }));
    expect(await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [next])).toMatchObject({ anchor: null, reason: 'line-changed' });
  });

  it('detaches an added line when lines are added above it, even with its text one row below', async () => {
    const next = fileOf(section({
      index: 'aaaaaaa..ddddddd',
      hunk: '@@ -1,2 +1,9 @@',
      lines: [' top', '+inserted', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom'],
    }));
    expect(await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [next])).toMatchObject({ anchor: null, reason: 'line-changed' });
  });

  it('follows an unchanged line by its base version and restamps both numbers', async () => {
    const before = fileOf(section({ index: '1111111..2222222', hunk: '@@ -1,3 +1,3 @@', lines: [' keep 1', '-old 2', '+new 2', ' keep 3'] }));
    const after = fileOf(section({ index: '1111111..3333333', hunk: '@@ -1,3 +1,4 @@', lines: [' keep 1', '-old 2', '+new 2', '+more', ' keep 3'] }));
    const keep3: CommentAnchor = { filePath: PATH, side: 'context', oldLine: 3, newLine: 3 };
    expect((await placeOne(placedOn(before, keep3, ' keep 3'), [after])).anchor).toMatchObject({ side: 'context', oldLine: 3, newLine: 4 });
    // Either half of the same row follows it too, with both numbers.
    const right: CommentAnchor = { filePath: PATH, side: 'new', oldLine: 3, newLine: 3 };
    expect((await placeOne(placedOn(before, right, ' keep 3'), [after])).anchor).toEqual({ filePath: PATH, side: 'new', oldLine: 3, newLine: 4, selectedText: ' keep 3' });
    const left: CommentAnchor = { filePath: PATH, side: 'old', oldLine: 3, newLine: 3 };
    expect((await placeOne(placedOn(before, left, ' keep 3'), [after])).anchor).toEqual({ filePath: PATH, side: 'old', oldLine: 3, newLine: 4, selectedText: ' keep 3' });
  });

  it('never places a right-side editor on a row its old number now deletes', async () => {
    const before = fileOf(section({ index: '1111111..2222222', hunk: '@@ -1,3 +1,3 @@', lines: [' keep 1', '-old 2', '+new 2', ' keep 3'] }));
    const after = fileOf(section({ index: '1111111..4444444', hunk: '@@ -1,3 +1,2 @@', lines: [' keep 1', '-old 2', '+new 2', '-keep 3'] }));
    for (const anchor of [
      { filePath: PATH, side: 'context', oldLine: 3, newLine: 3 },
      { filePath: PATH, side: 'new', oldLine: 3, newLine: 3 },
    ] as CommentAnchor[]) {
      expect(await placeOne(placedOn(before, anchor, ' keep 3'), [after]), anchor.side).toMatchObject({ anchor: null, reason: 'line-changed' });
    }
    // A left-half editor names the old line, which the base still has:
    // deleted now.
    const left: CommentAnchor = { filePath: PATH, side: 'old', oldLine: 3, newLine: 3 };
    expect((await placeOne(placedOn(before, left, ' keep 3'), [after])).anchor).toEqual({ filePath: PATH, side: 'old', oldLine: 3, selectedText: ' keep 3' });
  });

  it('says a proven line outside the diff\'s rows is not in the diff now', async () => {
    const before = fileOf(section({ index: '1111111..2222222', hunk: '@@ -3,1 +3,2 @@', lines: [' c', '+d'] }));
    const after = fileOf(section({ index: '9999999..2222222', hunk: '@@ -20,1 +20,1 @@', lines: ['-x', '+y'] }));
    const d: CommentAnchor = { filePath: PATH, side: 'new', newLine: 4 };
    expect(await placeOne(placedOn(before, d, 'd'), [after])).toMatchObject({ anchor: null, reason: 'not-in-diff', proof: { kind: 'blob', newBlob: '2222222', newLine: 4 } });
  });

  it('places a detached editor again when a later diff shows its version, and only then', async () => {
    const proof: EditorProof = { kind: 'blob', oldBlob: 'aaaaaaa', newBlob: 'bbbbbbb', oldLine: 0, newLine: 3, context: false };
    const target: RelocationTarget = { filePath: PATH, side: 'new', selectedText: '  report();', guard: lineGuard('  report();'), source: null, proof, line: 3 };
    expect((await placeOne(target, [fileOf(BASE_TO_A)])).anchor).toMatchObject({ newLine: 3 });
    // A longer abbreviation of the same id is the same id.
    expect((await placeOne(target, [fileOf(BASE_TO_A.replace('aaaaaaa..bbbbbbb', 'aaaaaaa1..bbbbbbb2'))])).anchor).toMatchObject({ newLine: 3 });
    // The guard: an id that collided with another version of the line.
    expect(await placeOne({ ...target, guard: lineGuard('retry();') }, [fileOf(BASE_TO_A)])).toMatchObject({ anchor: null, reason: 'line-changed' });
  });

  it('treats no id as equal to another, absent or zero', async () => {
    const proof: EditorProof = { kind: 'blob', oldBlob: null, newBlob: null, oldLine: 1, newLine: 1, context: true };
    const target: RelocationTarget = { filePath: PATH, side: 'context', selectedText: ' top', guard: lineGuard(' top'), source: null, proof, line: 1 };
    expect((await placeOne(target, [fileOf(BASE_TO_A)])).anchor).toBeNull();
    const short: EditorProof = { ...proof, oldBlob: 'aaa' };
    expect((await placeOne({ ...target, proof: short }, [fileOf(BASE_TO_A.replace('aaaaaaa..', 'aaa..'))])).anchor).toBeNull();
  });

  it('follows a rename with the same base version by the path it came from', async () => {
    const before = fileOf(section({ index: '1111111..2222222', hunk: '@@ -1,2 +1,2 @@', lines: [' keep 1', '-a', '+b'] }));
    const renamed = fileOf(section({
      path: 'src/moved.ts',
      meta: ['similarity index 80%', `rename from ${PATH}`, 'rename to src/moved.ts'],
      index: '1111111..5555555',
      hunk: '@@ -1,2 +1,2 @@',
      lines: [' keep 1', '-a', '+c'],
    }).replace(`--- a/src/moved.ts`, `--- a/${PATH}`), 'src/moved.ts');
    const keep: CommentAnchor = { filePath: PATH, side: 'context', oldLine: 1, newLine: 1 };
    const outcome = await placeOne(placedOn(before, keep, ' keep 1'), [renamed]);
    expect(outcome.filePath).toBe('src/moved.ts');
    expect(outcome.anchor).toMatchObject({ filePath: 'src/moved.ts', side: 'context', oldLine: 1, newLine: 1 });
  });

  it('says a file gone from the diff is gone, and keeps the proof', async () => {
    const outcome = await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), []);
    expect(outcome).toMatchObject({ anchor: null, reason: 'file-gone', proof: { kind: 'blob', newBlob: 'bbbbbbb' } });
  });

  it('proves no line an expansion supplied', async () => {
    const shown = fileOf(BASE_TO_A);
    const expanded: CommentAnchor = { filePath: PATH, side: 'context', oldLine: 40, newLine: 46 };
    expect(await placeOne(placedOn(shown, expanded, 'far'), [fileOf(BASE_TO_A)])).toMatchObject({ anchor: null, reason: 'line-changed', proof: { kind: 'none' } });
  });

  function merged(first: string[], second: string[], hunks: [string, string]): ReviewFile {
    return {
      ...reviewFileFromPatchFile(mergePatchFilesByPath(parsePatchFiles([
        section({ index: '1111111..2222222', hunk: hunks[0], lines: first }),
        section({ index: '2222222..3333333', hunk: hunks[1], lines: second }),
      ].join('\n')))[0]),
      mergedSections: 2,
    };
  }

  it('proves a merged file by its bytes and the row\'s place in them', async () => {
    const at = (second: string): ReviewFile => merged(['-a', '+b'], ['-c', second], ['@@ -1,1 +1,1 @@', '@@ -9,1 +9,1 @@']);
    const nine: CommentAnchor = { filePath: PATH, side: 'new', newLine: 9 };
    const target = placedOn(at('+d'), nine, 'd');
    const same = await placeOne(target, [at('+d')]);
    expect(same.anchor).toMatchObject({ side: 'new', newLine: 9 });
    expect(same.proof).toMatchObject({ kind: 'section' });
    // Another edit in the turn: other bytes, and the first section's ids
    // (unchanged) prove nothing.
    expect(await placeOne(target, [at('+e')])).toMatchObject({ anchor: null, reason: 'line-changed' });
  });

  it('proves no line whose number a merged file repeats across its sections', async () => {
    // Two edits of line 1, shown in edit order: line 1 is on a row of each.
    const overlapping = (): ReviewFile => merged(['-a', '+b'], ['-b', '+c'], ['@@ -1,1 +1,1 @@', '@@ -1,1 +1,1 @@']);
    const before = overlapping();
    const one: CommentAnchor = { filePath: PATH, side: 'new', newLine: 1 };
    let rowsOnOne = 0;
    for (let index = 0; index < before.body.lineCount; index += 1) if (before.body.text(index) === '+b' || before.body.text(index) === '+c') rowsOnOne += 1;
    expect(rowsOnOne).toBe(2);
    expect(await placeOne(placedOn(before, one, 'b'), [overlapping()])).toMatchObject({ anchor: null, proof: { kind: 'none' } });
  });

  it('scans each file once for any number of editors, and reads only the rows it found', async () => {
    const lines = Array.from({ length: 2000 }, (_, index) => `+line ${index + 1}`);
    const patch = section({ index: '0000000..1111111', meta: ['new file mode 100644'], hunk: '@@ -0,0 +1,2000 @@', lines });
    const before = fileOf(patch);
    const after = fileOf(patch);
    const targets = new Map(Array.from({ length: 25 }, (_, index) => {
      const line = index * 80 + 1;
      return [`e${index}`, placedOn(before, { filePath: PATH, side: 'new', newLine: line }, `line ${line}`)] as const;
    }));
    const kinds = [vi.spyOn(before.body, 'kind'), vi.spyOn(after.body, 'kind')];
    const text = vi.spyOn(after.body, 'text');
    const out = await relocateEditors(targets, [after], budget(), never);
    expect([...out.values()].every((outcome) => outcome.anchor !== null)).toBe(true);
    // One walk per file (plus its few header lines), not one per editor.
    for (const kind of kinds) expect(kind.mock.calls.length).toBeLessThan(2 * before.body.lineCount);
    // Header lines and the 25 found rows.
    expect(text.mock.calls.length).toBeLessThanOrEqual(5 + 25);
  });

  it('detaches what a spent budget did not reach', async () => {
    const out = await relocateEditors(
      new Map([['e', placedOn(fileOf(BASE_TO_A), report, '  report();')]]),
      [fileOf(BASE_TO_A)],
      new RelocationBudget(2, 100_000_000),
      never,
    );
    expect(out.get('e')).toMatchObject({ anchor: null, reason: 'line-changed' });
  });

  it('stops when cancelled across an await', async () => {
    let cancelled = false;
    const pending = relocateEditors(new Map([['e', placedOn(fileOf(BASE_TO_A), report, '  report();')]]), [fileOf(BASE_TO_A)], budget(), () => cancelled);
    cancelled = true;
    await expect(pending).rejects.toBeInstanceOf(RelocationCancelled);
  });

  it('places a file comment while its file is in the diff', async () => {
    const file = fileOf(BASE_TO_A);
    const target = placedOn(file, { filePath: PATH, side: 'file' }, '');
    expect((await placeOne(target, [fileOf(BASE_TO_A)])).anchor).toEqual({ filePath: PATH, side: 'file', selectedText: '' });
    expect(await placeOne(target, [])).toMatchObject({ anchor: null, reason: 'file-gone' });
  });

  describe('the suggested row of a detached editor', () => {
    // The added line moved one row down; its text is on one row.
    const moved = section({
      index: 'aaaaaaa..ddddddd',
      hunk: '@@ -1,2 +1,9 @@',
      lines: [' top', '+inserted', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom'],
    });

    it('is the one row on its side with its text', async () => {
      const outcome = await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [fileOf(moved)]);
      expect(outcome).toMatchObject({ anchor: null, reason: 'line-changed', suggested: { side: 'new', newLine: 4, selectedText: '  report();' } });
    });

    it('is the row with its line number when its text is on no row, or on several', async () => {
      const twice = section({ index: 'aaaaaaa..eeeeeee', hunk: '@@ -1,2 +1,6 @@', lines: [' top', '+  report();', '+x', '+  report();', '+y', ' bottom'] });
      expect((await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [fileOf(twice)])).suggested).toMatchObject({ newLine: 3, selectedText: 'x' });
      const none = section({ index: 'aaaaaaa..fffffff', hunk: '@@ -1,2 +1,5 @@', lines: [' top', '+a', '+b', '+c', ' bottom'] });
      expect((await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [fileOf(none)])).suggested).toMatchObject({ newLine: 3, selectedText: 'b' });
    });

    it('is the nearest row on its side by number when none has it', async () => {
      const short = section({ index: 'aaaaaaa..1212121', hunk: '@@ -1,1 +1,2 @@', lines: [' top', '+a'] });
      expect((await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [fileOf(short)])).suggested).toMatchObject({ newLine: 2, selectedText: 'a' });
      // A left-half editor looks at the old side only.
      const before = fileOf(section({ index: '1111111..2222222', hunk: '@@ -1,3 +1,3 @@', lines: [' keep 1', '-old 2', '+new 2', ' keep 3'] }));
      const after = fileOf(section({ index: '3333333..2222222', hunk: '@@ -1,2 +1,3 @@', lines: [' keep 1', '-gone', '+new 2', '+keep 3'] }));
      const left: CommentAnchor = { filePath: PATH, side: 'old', oldLine: 2 };
      expect((await placeOne(placedOn(before, left, 'old 2'), [after])).suggested).toEqual({ filePath: PATH, side: 'old', oldLine: 2, selectedText: 'gone' });
    });

    it('skips the text rule over budget and reads no line for it', async () => {
      // A budget with room for the walks and none for reading the file again.
      class Tight extends RelocationBudget {
        override trySpend(): boolean {
          return false;
        }
      }
      const file = fileOf(moved);
      const text = vi.spyOn(file.body, 'text');
      const out = await relocateEditors(new Map([['e', placedOn(fileOf(BASE_TO_A), report, '  report();')]]), [file], new Tight(1_000_000, 100_000_000), never);
      expect(out.get('e')?.suggested).toMatchObject({ newLine: 3, selectedText: 'if (primary) {' });
      // The header and the suggested row; no row read for the text rule.
      expect(text.mock.calls.length).toBeLessThanOrEqual(5);
    });

    it('spends only what fits', () => {
      const budget = new RelocationBudget(10, 100);
      expect(budget.trySpend(11)).toBe(false);
      expect(budget.trySpend(5, 101)).toBe(false);
      expect(budget.trySpend(10, 100)).toBe(true);
      expect(budget.trySpend(1)).toBe(false);
      expect(budget.trySpend(0, 1)).toBe(false);
    });

    it('has none for a file gone', async () => {
      expect((await placeOne(placedOn(fileOf(BASE_TO_A), report, '  report();'), [])).suggested).toBeNull();
    });

    // A file of `rows` added lines `line N`, and a version of it with one
    // line inserted on top, so every line moved.
    function longFile(rows: number, shift: number): ReviewFile {
      const lines = Array.from({ length: rows }, (_, index) => `+line ${index + 1}`);
      const top = Array.from({ length: shift }, (_, index) => `+inserted ${index + 1}`);
      return fileOf(section({ index: `0000000..${String(shift).padStart(7, '1')}`, meta: ['new file mode 100644'], hunk: `@@ -0,0 +1,${rows + shift} @@`, lines: [...top, ...lines] }));
    }

    function editorsOn(file: ReviewFile, count: number): Map<string, RelocationTarget> {
      return new Map(Array.from({ length: count }, (_, index) => {
        const line = index * 7 + 1;
        return [`e${index}`, placedOn(file, { filePath: PATH, side: 'new', newLine: line }, `line ${line}`)] as const;
      }));
    }

    it('serves every editor of a file from one pass over its rows', async () => {
      const after = longFile(500, 1);
      const text = vi.spyOn(after.body, 'text');
      const out = await relocateEditors(editorsOn(longFile(500, 0), 10), [after], budget(), never);
      // Each moved down one row, found by its text.
      expect([...out.values()].map((outcome) => outcome.suggested?.newLine)).toEqual(Array.from({ length: 10 }, (_, index) => index * 7 + 2));
      // One read of each row for the text rule, not one per editor.
      expect(text.mock.calls.length).toBeLessThanOrEqual(after.body.lineCount + 20);
    });

    it('reads no text and awaits no residency for the text rule over its cap', async () => {
      // Past the text rule's cap of 20 000 rows.
      const after = longFile(20_010, 1);
      const text = vi.spyOn(after.body, 'text');
      const resident = vi.spyOn(after.body, 'whenResident');
      const out = await relocateEditors(editorsOn(longFile(20_010, 0), 3), [after], budget(), never);
      // The number rule: the row with its old number.
      expect([...out.values()].map((outcome) => outcome.suggested?.newLine)).toEqual([1, 8, 15]);
      expect(resident).not.toHaveBeenCalled();
      // The header lines and the three suggested rows.
      expect(text.mock.calls.length).toBeLessThanOrEqual(5 + 3);
    });

    it('decides by rows and bytes, never the clock, the same every run', async () => {
      const now = vi.spyOn(performance, 'now');
      const run = () => relocateEditors(editorsOn(longFile(300, 0), 5), [longFile(300, 1)], new RelocationBudget(1_000, 1_000_000), never);
      const first = await run();
      expect(await run()).toEqual(first);
      expect(now).not.toHaveBeenCalled();
    });
  });

  describe('the file an editor is in', () => {
    it('is gone when its file left the diff, even with the budget spent before the lookup', async () => {
      const file = fileOf(BASE_TO_A);
      const other = fileOf(section({ path: 'src/other.ts', hunk: '@@ -1,1 +1,1 @@', lines: ['-x', '+y'] }), 'src/other.ts');
      const out = await relocateEditors(new Map([['e', placedOn(file, report, '  report();')]]), [other], new RelocationBudget(1, 1), never);
      expect(out.get('e')).toMatchObject({ filePath: PATH, anchor: null, reason: 'file-gone', suggested: null });
    });

    it('is in the diff when its file is, even with the budget spent', async () => {
      const out = await relocateEditors(new Map([['e', placedOn(fileOf(BASE_TO_A), report, '  report();')]]), [fileOf(BASE_TO_A)], new RelocationBudget(1, 1), never);
      expect(out.get('e')).toMatchObject({ anchor: null, reason: 'line-changed' });
    });
  });
});
