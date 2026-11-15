import { PatchParser, type ReviewFile } from '../../lib/utils/patchParser';
import { seededRandom } from './seededRandom';

// Patches the patch storage tests share: one with every header and line
// shape, random ones, and one with text past ASCII in every place a line
// can hold it.

export const MIXED_PATCH = [
  'preamble line before any file',
  'diff --git a/src/app.ts b/src/app.ts',
  'index 1111111..2222222 100644',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -10,6 +10,7 @@ export function app() {',
  ' const a = 1;',
  '-const b = 2;',
  '-const c = 3;',
  '+const b = 20;',
  '+const c = 30;',
  '+const d = 40;',
  ' const e = 5;',
  '@@ -40,3 +41,3 @@',
  ' tail();',
  '--- removed "-- note" reads like a header',
  '+++ added "++i" reads like a header',
  '-old();',
  '+new();',
  '\\ No newline at end of file',
  'diff --git a/docs/readme.md b/docs/renamed.md',
  'similarity index 90%',
  'rename from docs/readme.md',
  'rename to docs/renamed.md',
  'index 3333333..4444444 100644',
  '--- a/docs/readme.md',
  '+++ b/docs/renamed.md',
  '@@ -1,3 +1,3 @@',
  ' # Title',
  '-héllo wörld',
  '+hello world ✓',
  ' end',
  'diff --git a/img.png b/img.png',
  'index 5555555..6666666 100644',
  'Binary files a/img.png and b/img.png differ',
  'diff --git a/new.txt b/new.txt',
  'new file mode 100644',
  'index 0000000..7777777',
  '--- /dev/null',
  '+++ b/new.txt',
  '@@ -0,0 +1,2 @@',
  '+one\r',
  '+two',
  'diff --git a/gone.txt b/gone.txt',
  'deleted file mode 100644',
  'index 8888888..0000000',
  '--- a/gone.txt',
  '+++ /dev/null',
  '@@ -1,2 +0,0 @@',
  '-first',
  '-second',
  'diff --git a/link b/link',
  'deleted file mode 100644',
  'index 9999999..0000000',
  '--- a/link',
  '+++ /dev/null',
  '@@ -1 +0,0 @@',
  '-target',
  '\\ No newline at end of file',
  'diff --git a/link b/link',
  'new file mode 120000',
  'index 0000000..aaaaaaa',
  '--- /dev/null',
  '+++ b/link',
  '@@ -0,0 +1 @@',
  '+other-target',
  'diff --git a/mode.sh b/mode.sh',
  'old mode 100644',
  'new mode 100755',
  'diff --git a/z.go b/z.go',
  '--- a/z.go',
  '+++ b/z.go',
  '@@ -1,2 +1,2 @@',
  '-package z',
  '+package zz',
  ' ',
  '',
  '@@ -20,2 +20,3 @@ func Z() {',
  ' 	return',
  '+	// more',
  ' }',
].join('\n') + '\n';

const LONG_WIDE = `${'é'.repeat(40)}x😀${'✓'.repeat(30)}`;

export const UNICODE_PATCH = [
  '\ufeffpreamble with a BOM',
  'diff --git a/docs/à côté.md b/docs/à côté.md',
  'index 1111111..2222222 100644',
  '--- a/docs/à côté.md',
  '+++ b/docs/à côté.md',
  '@@ -1,6 +1,7 @@ fn ünïcode() {',
  ' ascii context',
  '-héllo wörld\r',
  '+hello world ✓\r',
  '+astral 😀 𝄞 and \ufffd',
  '',
  `-${LONG_WIDE}`,
  `+${LONG_WIDE}y`,
  ' end',
  'diff --git a/naïve.txt b/日本.txt',
  'similarity index 90%',
  'rename from naïve.txt',
  'rename to 日本.txt',
  'index 3333333..4444444 100644',
  '--- a/naïve.txt',
  '+++ b/日本.txt',
  '@@ -1 +1 @@',
  '-a',
  '+ü',
  // Its path is only in the header line; `à` is C3 A0 in UTF-8, and
  // U+00A0 is a space to a regex.
  'diff --git a/à.png b/à.png',
  'index 5555555..6666666 100644',
  'Binary files a/à.png and b/à.png differ',
  // A pure rename: its path is only in `rename to`.
  'diff --git a/x y.txt b/ü z.txt',
  'similarity index 100%',
  'rename from x y.txt',
  'rename to ü z.txt',
  'diff --git a/plain.go b/plain.go',
  '--- a/plain.go',
  '+++ b/plain.go',
  '@@ -1,2 +1,2 @@',
  '-package a',
  '+package b',
  ' 😀',
].join('\n');

export function randomPatch(seed: number): string {
  const rand = seededRandom(seed);
  const out: string[] = [];
  const fileCount = 1 + rand(6);
  for (let file = 0; file < fileCount; file += 1) {
    const path = `dir${rand(3)}/file${file}.${['ts', 'go', 'md'][rand(3)]}`;
    out.push(`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`);
    let oldLine = 1 + rand(5);
    let newLine = oldLine;
    const hunks = 1 + rand(4);
    for (let hunk = 0; hunk < hunks; hunk += 1) {
      const body: string[] = [];
      let oldCount = 0;
      let newCount = 0;
      const lines = 1 + rand(40);
      for (let line = 0; line < lines; line += 1) {
        const pick = rand(4);
        // Some lines' text starts with "--" or "++", so the line reads
        // like a file header with its prefix.
        const lead = ['', '', '', '--', '++'][rand(5)];
        const text = `${lead}line ${rand(1000)} ${'x'.repeat(rand(30))}`;
        if (pick === 0) {
          body.push(`-${text}`);
          oldCount += 1;
        } else if (pick === 1) {
          body.push(`+${text}`);
          newCount += 1;
        } else {
          body.push(` ${text}`);
          oldCount += 1;
          newCount += 1;
        }
      }
      out.push(`@@ -${oldLine},${oldCount} +${newLine},${newCount} @@`, ...body);
      const between = rand(8);
      oldLine += oldCount + between;
      newLine += newCount + between;
    }
  }
  return out.join('\n') + (rand(2) === 0 ? '\n' : '');
}

/** Parses text appended in pieces of `sizes(index)` UTF-16 units, cut
 * anywhere between characters: a piece that would end inside a pair
 * takes the whole pair. */
export function parseInChunks(patch: string, sizes: (index: number) => number): { parser: PatchParser; files: ReviewFile[] } {
  const parser = new PatchParser();
  let pos = 0;
  for (let index = 0; pos < patch.length; index += 1) {
    let end = pos + Math.max(1, sizes(index));
    if ((patch.charCodeAt(end - 1) & 0xfc00) === 0xd800) end += 1;
    parser.append(patch.slice(pos, end));
    pos = end;
  }
  parser.end();
  return { parser, files: parser.files };
}
