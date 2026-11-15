import { afterEach, describe, expect, it, vi } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { FIND_FILTER_CODE_POINTS, findFilterPattern, findNeedle, foldFindText, LONG_TEXT } from './findMatch';
import { setPatchTextBudgetForTest } from './patchMemory.svelte';
import { PatchBody } from './patchBody';
import { PatchParser, parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchParser';
import { PatchStore } from './patchStore';
import { BLOCK_LINES, CLOCK_ROWS, scanReviewFind, type ReviewFindContent } from './reviewFindScan';
import { collectScan, findContent, findDraft as draft, findThread as thread } from '../../test/helpers/reviewFindContent';
import { seededRandom as random } from '../../test/helpers/seededRandom';
import { fakeDiff, streamPatch, type PatchForm } from '../../test/helpers/streamedPatch';
import type { DiffReviewComment, ReviewThread } from '../types/models';

// The scan with the filter off (findFilterPattern answering null, as for a
// needle it cannot spell) reads every row's text, so it is the oracle the
// filtered scan must match hit for hit, in order.
const filter = vi.hoisted(() => ({ off: false }));
vi.mock('./findMatch', async (importOriginal) => {
  const real = await importOriginal<typeof import('./findMatch')>();
  return { ...real, findFilterPattern: (needle: string) => (filter.off ? null : real.findFilterPattern(needle)) };
});

async function unfilteredScan(content: ReviewFindContent, query: string, limit: number): ReturnType<typeof collectScan> {
  filter.off = true;
  try {
    return await collectScan(content, query, limit);
  } finally {
    filter.off = false;
  }
}

const content = (over: Partial<ReviewFindContent> & { files: readonly ReviewFile[] }): ReviewFindContent => findContent(over.files, over);

// Capitals, letters past ASCII with and without a fold, the Kelvin sign,
// dotted and dotless i, Deseret, regex syntax, the prefix characters, and
// characters whose plain form is a packed spelling (Ã©).
const SCAN_ALPHABET = [
  'a', 'b', 'x', 'A', 'B', 'k', 'K', '\u212a', 'i', 'I', 'İ', 'ı', 'é', 'É', 'Ã', '©', 'σ', 'Σ', 'ς',
  '中', '\u{10400}', '\u{10428}', '\ufffd', '+', '-', ' ', '.', '[', '\\', '(', 'Needle', 'needle', 'NeedleNeedle',
];

const SCAN_QUERIES = [
  '+', '-', ' ', 'a', 'A', 'ab', 'k', 'K', '\u212a', 'i', 'İ', 'ı', 'é', 'É', 'Ã©', 'σ', 'Σ', '\u{10400}', '\ud801',
  '\ufffd', '+a', '-b', ' a', '.', '[', '\\', 'needle', 'NEEDLE', 'x\nx', 'a.b', 'École', '<', '|', '=', '>', 'xxN',
  // Longer than the spelled prefix (FIND_FILTER_CODE_POINTS), which lines
  // hold without the rest.
  'needleNeedleneedleX', 'NEEDLENEEDLEab', 'needleneedle needle',
];

function randomWord(next: (n: number) => number, length: number, alphabet: readonly string[] = SCAN_ALPHABET): string {
  let out = '';
  for (let i = 0; i < length; i += 1) out += alphabet[next(alphabet.length)];
  return out;
}

const ASCII_ALPHABET = SCAN_ALPHABET.filter((part) => /^[ -~]*$/.test(part));

// A line of `length` characters with a match of `word` straddling each
// LONG_TEXT edge of the code text.
function longLine(length: number, word: string): string {
  let out = '';
  for (let edge = LONG_TEXT; out.length < length; edge += LONG_TEXT) {
    out += 'x'.repeat(Math.max(0, edge - 2 - out.length)) + word;
  }
  return out;
}

const SCAN_PATHS = ['src/a.ts', 'src/École.ts', 'K/needle.ts', 'c.ts'];

// `sparse`: one row in 20 past ASCII, as a diff the backend packs has.
function randomPatch(next: (n: number) => number, long: boolean, sparse = false): string {
  const out: string[] = [];
  for (const path of SCAN_PATHS.slice(0, 1 + next(SCAN_PATHS.length))) {
    out.push(`diff --git a/${path} b/${path}`, 'index 1..2 100644', `--- a/${path}`, `+++ b/${path}`);
    let old = 1 + next(5);
    let neu = old;
    for (let hunk = 1 + next(3); hunk > 0; hunk -= 1) {
      const rows: string[] = [];
      let oldCount = 0;
      let newCount = 0;
      for (let row = 1 + next(25); row > 0; row -= 1) {
        const prefix = [' ', '+', '-'][next(3)];
        if (prefix !== '+') oldCount += 1;
        if (prefix !== '-') newCount += 1;
        rows.push(prefix + randomWord(next, next(10), sparse && next(20) !== 0 ? ASCII_ALPHABET : SCAN_ALPHABET));
      }
      if (long && hunk === 1) {
        // Inside a block, and over the block size.
        rows.push(`+${longLine(40_000, 'Needle')}`, `+${longLine(70_000, 'aB')}`);
        newCount += 2;
      }
      out.push(`@@ -${old},${oldCount} +${neu},${newCount} @@ needle ${randomWord(next, 3, sparse ? ASCII_ALPHABET : SCAN_ALPHABET)}`, ...rows);
      const gap = next(2) === 0 ? 0 : 3 + next(10);
      old += oldCount + gap;
      neu += newCount + gap;
    }
  }
  const rowAlphabet = sparse ? ASCII_ALPHABET : SCAN_ALPHABET;
  // A type change: one file over two segments of the store.
  out.push(
    'diff --git a/t.ts b/t.ts', 'deleted file mode 100644', '--- a/t.ts', '+++ /dev/null', '@@ -1,2 +0,0 @@',
    `-${randomWord(next, 6, rowAlphabet)}`, `-${randomWord(next, 6, rowAlphabet)}`,
    'diff --git a/t.ts b/t.ts', 'new file mode 120000', '--- /dev/null', '+++ b/t.ts', '@@ -0,0 +1,2 @@',
    `+${randomWord(next, 6, rowAlphabet)}`, `+${randomWord(next, 6, rowAlphabet)}`,
  );
  return out.join('\n') + '\n';
}

// A conflict file with folds, as the review pane shows one collapsed.
function randomConflict(next: (n: number) => number): { file: ReviewFile; folds: Map<number, readonly string[]> } {
  const run = (count: number) => Array.from({ length: count }, () => randomWord(next, next(8)));
  const merged = [
    ...run(2 + next(10)), '<<<<<<< ours', ...run(1 + next(3)), '=======', ...run(1 + next(3)), '>>>>>>> theirs',
    ...run(next(12)), '<<<<<<< ours', ...run(1 + next(2)), '||||||| base', ...run(next(2)), '=======', ...run(next(2)), '>>>>>>> theirs', '',
  ].join('\n');
  const folds = new Map<number, readonly string[]>();
  const file = reviewFileFromPatchFile(conflictPatchFile('m.go', merged, { onFold: (id, lines) => folds.set(id, lines) }));
  return { file, folds };
}

function randomComments(next: (n: number) => number, files: readonly ReviewFile[]): { drafts: DiffReviewComment[]; prThreads: ReviewThread[] } {
  const drafts: DiffReviewComment[] = [];
  const prThreads: ReviewThread[] = [];
  for (let i = next(6); i > 0; i -= 1) {
    const path = files[next(files.length)].path;
    drafts.push({ ...draft(`d${i}`, 1 + next(30), randomWord(next, 1 + next(6))), filePath: path });
  }
  for (let i = next(4); i > 0; i -= 1) {
    const path = files[next(files.length)].path;
    prThreads.push({ ...thread(`p${i}`, next(4) === 0 ? null : 1 + next(30), Array.from({ length: 1 + next(3) }, () => randomWord(next, 1 + next(6)))), path });
  }
  return { drafts, prThreads };
}

describe('scanReviewFind filtered by the stored text', () => {
  const disposers: (() => void)[] = [];
  afterEach(() => {
    for (const dispose of disposers.splice(0)) dispose();
    setPatchTextBudgetForTest(null);
  });

  it('the oracle reads every row, the filtered scan only those that can match', async () => {
    const rows = Array.from({ length: 300 }, (_, i) => `+row ${i}${i === 7 ? ' needle' : ''}`);
    const source = content({ files: parseReviewFiles(['diff --git a/o b/o', '--- a/o', '+++ b/o', `@@ -0,0 +1,${rows.length} @@`, ...rows, ''].join('\n')) });
    const text = vi.spyOn(PatchBody.prototype, 'text');
    try {
      expect((await unfilteredScan(source, 'needle', 1000)).hits).toHaveLength(1);
      expect(text).toHaveBeenCalledTimes(rows.length);
      text.mockClear();
      expect((await collectScan(source, 'needle', 1000)).hits).toHaveLength(1);
      expect(text).toHaveBeenCalledTimes(1);
    } finally {
      text.mockRestore();
    }
  });

  // The scan's one flags array holds the first file's flagged lines when
  // the second's filter starts.
  it('a file\'s filter never answers from the file before it', async () => {
    const patch = (path: string, count: number, at: number) => {
      const rows = Array.from({ length: count }, (_, i) => `+row ${i}${i === at ? ' needle' : ''}`);
      return [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`, `@@ -0,0 +1,${count} @@`, ...rows].join('\n');
    };
    const source = content({ files: parseReviewFiles(`${patch('a', 300, 7)}\n${patch('b', 40, 20)}\n`) });
    const { hits } = await collectScan(source, 'needle', 1000);
    expect(hits.map((hit) => [hit.path, hit.line])).toEqual([['a', 11], ['b', 24]]);
  });

  it('emits what the rule on every row emits, over random diffs in every form', async () => {
    const forms: (PatchForm | 'resident')[] = ['resident', 'plain', 'packed'];
    let compared = 0;
    let prefixOnly = 0;
    let packedChunks = 0;
    for (let round = 0; round < 48; round += 1) {
      const next = random(1000 + round);
      const form = forms[round % forms.length];
      const patch = randomPatch(next, round % 8 === 0, form === 'packed');
      let files: ReviewFile[];
      if (form === 'resident') {
        files = parseReviewFiles(patch);
      } else {
        setPatchTextBudgetForTest(300 + next(600));
        // A packed chunk needs room for its wide lines to be sparse.
        const streamed = streamPatch(patch, () => (form === 'packed' ? 400 + next(1600) : 40 + next(300)), form);
        disposers.push(() => streamed.parser.store.dispose());
        packedChunks += streamed.spans.filter((span) => span.wide !== undefined).length;
        files = streamed.files;
      }
      const conflict = randomConflict(next);
      files = [...files, conflict.file];
      const { drafts, prThreads } = randomComments(next, files);
      const source = content({ files, drafts, prThreads, foldLines: (path) => (path === 'm.go' ? conflict.folds : null) });
      const lines = patch.split('\n');
      const queries = [...SCAN_QUERIES, ...Array.from({ length: 6 }, () => {
        const line = [...lines[next(lines.length)]];
        const at = next(line.length + 1);
        return line.slice(at, at + 1 + next(4)).join('');
      })];
      for (const query of queries) {
        const limit = next(4) === 0 ? 1 + next(20) : 100_000;
        const want = await unfilteredScan(source, query, limit);
        const got = await collectScan(source, query, limit);
        expect(got, `round ${round} (${form}) ${JSON.stringify(query)} limit ${limit}`).toEqual(want);
        if (want.hits.some((hit) => hit.place.kind === 'line')) compared += 1;
        const needle = findNeedle(query);
        if ([...needle].length > FIND_FILTER_CODE_POINTS) {
          const prefix = findFilterPattern(needle)!;
          prefixOnly += lines.filter((line) => {
            prefix.lastIndex = 0;
            return prefix.test(line) && !foldFindText(line).includes(needle);
          }).length;
        }
      }
    }
    expect(compared).toBeGreaterThan(400);
    expect(packedChunks).toBeGreaterThan(0);
    // The prefix flagged lines the whole needle does not match.
    expect(prefixOnly).toBeGreaterThan(20);
  });

  it('reads no line text for lines that cannot match, and still yields by the clock', async () => {
    const lines = Array.from({ length: 20_000 }, (_, i) => `+line ${i}${i % 7 === 0 ? ' Ã©' : ''} café`);
    const patch = ['diff --git a/big b/big', '--- a/big', '+++ b/big', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n');
    setPatchTextBudgetForTest(64 * 1024);
    // Every line is wide, so the packed read answers it plain.
    const streamed = streamPatch(patch, 4096, 'packed');
    disposers.push(() => streamed.parser.store.dispose());
    expect(streamed.files[0].body.resident()).toBe(false);
    for (const files of [parseReviewFiles(patch), streamed.files]) {
      const reads = [
        vi.spyOn(PatchBody.prototype, 'text'),
        vi.spyOn(PatchStore.prototype, 'text'),
        vi.spyOn(PatchStore.prototype, 'textRange'),
        vi.spyOn(PatchBody.prototype, 'patchText'),
      ];
      try {
        let clock = 0;
        let pauses = 0;
        const { hits } = await collectScan(content({ files }), 'needle', 1000, {
          now: () => (clock += 1), sliceMs: 0, pause: async () => { pauses += 1; },
        });
        expect(hits).toHaveLength(0);
        // A needle holding a newline matches no line.
        expect((await collectScan(content({ files }), 'café\n+line', 1000)).hits).toHaveLength(0);
        for (const read of reads) expect(read).not.toHaveBeenCalled();
        // A clock read per CLOCK_ROWS rows, each past its slice.
        expect(pauses).toBeGreaterThanOrEqual(Math.floor(lines.length / CLOCK_ROWS));
        // A line that can match is read, once.
        expect((await collectScan(content({ files }), 'é', 100_000)).hits).toHaveLength(lines.length);
        expect(reads[0]).toHaveBeenCalledTimes(lines.length);
      } finally {
        for (const read of reads) read.mockRestore();
      }
    }
  });

  it('reads blocks of bounded size, one segment at a time', async () => {
    const wide = Array.from({ length: 40 }, (_, i) => `+${String(i).padStart(10_000, 'w')}`);
    const short = Array.from({ length: 3000 }, (_, i) => `+s${i}`);
    const patch = [
      'diff --git a/w b/w', '--- a/w', '+++ b/w', `@@ -0,0 +1,${wide.length + short.length} @@`, ...wide, ...short,
      'diff --git a/t b/t', 'deleted file mode 100644', '--- a/t', '+++ /dev/null', '@@ -1,1 +0,0 @@', '-gone',
      'diff --git a/t b/t', 'new file mode 120000', '--- /dev/null', '+++ b/t', '@@ -0,0 +1,1 @@', '+came',
      '',
    ].join('\n');
    const files = parseReviewFiles(patch);
    expect(files[1].body.segments).toHaveLength(2);
    const blocks = vi.spyOn(PatchBody.prototype, 'storedRange');
    try {
      await collectScan(content({ files }), 'needle', 1000);
      const read = blocks.mock.results.map((result) => result.value as string);
      expect(read.length).toBeGreaterThan(wide.length + short.length / BLOCK_LINES);
      for (const stored of read) {
        expect(stored.length).toBeLessThanOrEqual(LONG_TEXT);
        expect(stored.split('\n').length).toBeLessThanOrEqual(BLOCK_LINES);
      }
      // Each segment of the type change is its own block.
      expect(read.slice(-2)).toEqual(['-gone', '+came']);
    } finally {
      blocks.mockRestore();
    }
  });

  it('searches on from the end of a line it flags, not from each match', async () => {
    const lines = [
      `+${'needle '.repeat(2000)}`,
      ...Array.from({ length: 100 }, (_, i) => `+plain ${i}`),
      ...Array.from({ length: 100 }, (_, i) => `+needle ${i}`),
    ];
    const files = parseReviewFiles(['diff --git a/n b/n', '--- a/n', '+++ b/n', `@@ -0,0 +1,${lines.length} @@`, ...lines, ''].join('\n'));
    const exec = vi.spyOn(RegExp.prototype, 'exec');
    const text = vi.spyOn(PatchBody.prototype, 'text');
    try {
      const { hits } = await collectScan(content({ files }), 'needle', 100_000);
      expect(hits).toHaveLength(2100);
      // Only the lines that match are read.
      expect(text).toHaveBeenCalledTimes(101);
      const source = findFilterPattern(findNeedle('needle'))!.source;
      const filter = exec.mock.contexts.filter((pattern) => (pattern as RegExp).source === source);
      // One search per matching line, and the one that finds nothing more.
      expect(filter.length).toBeLessThanOrEqual(102);
    } finally {
      exec.mockRestore();
      text.mockRestore();
    }
  });

  it('a line over the block size is searched by the rule, which pauses inside it', async () => {
    // One line of 8 MB with no match.
    const long = parseReviewFiles(['diff --git a/min.js b/min.js', '--- a/min.js', '+++ b/min.js', '@@ -0,0 +1,1 @@', `+${'x'.repeat(8_000_000)}`, ''].join('\n'));
    let live = true;
    let pauses = 0;
    let clock = 0;
    await scanReviewFind(content({ files: long }), 'needle', {
      limit: 1000, live: () => live, emit: () => {},
      pause: async () => { pauses += 1; live = false; }, now: () => (clock += 1), sliceMs: 0,
    });
    expect(pauses).toBe(1);
  });

  it('finds U+FFFD where the patch holds an invalid byte, packed', async () => {
    const filler = Array.from({ length: 30 }, (_, index) => `+ascii filler line ${index}`);
    const encoder = new TextEncoder();
    const raw = Uint8Array.from([
      ...encoder.encode(['diff --git a/l b/l', '--- a/l', '+++ b/l', '@@ -0,0 +1,32 @@', '+café ok', '+bad '].join('\n')),
      0xff,
      ...encoder.encode([' byte', ...filler, ''].join('\n')),
    ]);
    // The backend packs it, the invalid byte sent as U+FFFD's bytes.
    const chunk = fakeDiff(raw).read(0, raw.length, true);
    expect(chunk.wide).toBeDefined();
    const parser = new PatchParser();
    parser.append(chunk.data, chunk);
    parser.end();
    const { hits } = await collectScan(content({ files: parser.files }), '\ufffd', 1000);
    expect(hits.map((hit) => hit.identity.split('\u0000')[1])).toEqual(['a2']);
    expect((await collectScan(content({ files: parser.files }), 'CAFÉ', 1000)).hits).toHaveLength(1);
  });
});

// One added file of `count` lines of `text`.
function repeatedFiles(text: string, count: number): ReviewFile[] {
  const lines = Array.from({ length: count }, () => `+${text}`);
  return parseReviewFiles(['diff --git a/r b/r', '--- a/r', '+++ b/r', `@@ -0,0 +1,${count} @@`, ...lines, ''].join('\n'));
}

describe('scanReviewFind work between clock reads', () => {
  afterEach(() => setPatchTextBudgetForTest(null));

  // Stored characters the body reads between two clock reads, the largest
  // line read whole in each stretch set apart, on a clock that reads every
  // slice as spent.
  async function stretches(files: ReviewFile[], query: string): Promise<{ widest: number; pauses: number }> {
    const realText = PatchBody.prototype.text;
    const realStored = PatchBody.prototype.storedRange;
    let since = 0;
    let line = 0;
    let widest = 0;
    let pauses = 0;
    let clock = 0;
    const text = vi.spyOn(PatchBody.prototype, 'text').mockImplementation(function (this: PatchBody, index: number) {
      const size = this.lineSize(index);
      since += size;
      line = Math.max(line, size);
      return realText.call(this, index);
    });
    const stored = vi.spyOn(PatchBody.prototype, 'storedRange').mockImplementation(function (this: PatchBody, start: number, end: number) {
      const out = realStored.call(this, start, end);
      since += out.length;
      return out;
    });
    const close = () => {
      widest = Math.max(widest, since - line);
      since = 0;
      line = 0;
    };
    try {
      await scanReviewFind(content({ files }), query, {
        limit: 1000, live: () => true, emit: () => {}, sliceMs: 0,
        now: () => { close(); return (clock += 1); },
        pause: async () => { pauses += 1; },
      });
      close();
    } finally {
      text.mockRestore();
      stored.mockRestore();
    }
    return { widest, pauses };
  }

  it('counts what it reads and searches, so no stretch between clock reads is long', async () => {
    setPatchTextBudgetForTest(1 << 30);
    const longE = 'é'.repeat(60_000);
    const sigma = 'σ'.repeat(5000);
    const cases: [string, ReviewFile[], string][] = [
      // Long lines whose prefix matches everywhere and whole never.
      // A chunk of such lines is never packed (jstext.PackText).
      ['long é', repeatedFiles(longE, 40), `${'é'.repeat(63)}x`],
      ['σ rows the rule reads', repeatedFiles(sigma, 2000), `${'σ'.repeat(63)}x`],
      // A line over the block size is never searched as a block.
      ['long é, spelled whole', repeatedFiles(longE, 40), `${'é'.repeat(11)}x`],
      // Blocks searched, no row flagged.
      ['σ blocks', repeatedFiles(sigma, 1000), `${'σ'.repeat(11)}x`],
      // Every row read (a needle the filter cannot spell).
      ['σ rows, every one', repeatedFiles(sigma, 1000), 'σ\ufffd'],
    ];
    for (const [name, files, query] of cases) {
      const { widest, pauses } = await stretches(files, query);
      expect(pauses, name).toBeGreaterThan(10);
      // Under CLOCK_ROWS units counted plus one item of at most LONG_TEXT.
      expect(widest, name).toBeLessThan(2 * LONG_TEXT);
    }
  });

  it('counts what a needle longer than a chunk folds past the chunk', async () => {
    // Text that holds the needle's prefix everywhere: every chunk folds
    // the needle's length past its end.
    const needle = `${'a'.repeat(3 * LONG_TEXT)}x`;
    const code = 'a'.repeat(20 * LONG_TEXT);
    const files = repeatedFiles(code, 1);
    const realSlice = String.prototype.slice;
    let since = 0;
    let widest = 0;
    let folded = 0;
    let reads = 0;
    let clock = 0;
    // The rule's chunks: slices of the row's code text.
    const slice = vi.spyOn(String.prototype, 'slice').mockImplementation(function (this: string, ...args: [number?, number?]) {
      const out = realSlice.apply(this, args);
      if (this.length === code.length && out.length < code.length) {
        since += out.length;
        folded += out.length;
      }
      return out;
    });
    try {
      await scanReviewFind(content({ files }), needle, {
        limit: 1000, live: () => true, emit: () => {}, sliceMs: 0, pause: async () => {},
        now: () => { reads += 1; widest = Math.max(widest, since); since = 0; return (clock += 1); },
      });
    } finally {
      slice.mockRestore();
    }
    widest = Math.max(widest, since);
    expect(folded).toBeGreaterThan(20 * LONG_TEXT);
    const bound = 2 * LONG_TEXT + needle.length;
    expect(widest).toBeLessThanOrEqual(bound);
    expect(reads).toBeGreaterThanOrEqual(Math.floor(folded / bound));
  });
});

describe('scanReviewFind pattern work', () => {
  afterEach(() => setPatchTextBudgetForTest(null));

  // Pattern steps (one spelled code point tried at one position) weighed
  // as one character the rule folds and searches: the weight the bound
  // below holds the filter to.
  const STEPS_PER_RULE_CHAR = 16;

  // Characters the rule reads (`text`), and pattern steps: characters the
  // filter searched, each start trying every spelled code point.
  async function work(files: ReviewFile[], query: string): Promise<number> {
    const realText = PatchBody.prototype.text;
    const realExec = RegExp.prototype.exec;
    const source = findFilterPattern(findNeedle(query))!.source;
    // Its spelled code points: one class or group each.
    const spelled = source.match(/\(\?:|(?<!\(\?:)\[/g)!.length;
    let rule = 0;
    let steps = 0;
    const text = vi.spyOn(PatchBody.prototype, 'text').mockImplementation(function (this: PatchBody, index: number) {
      const out = realText.call(this, index);
      rule += out.length;
      return out;
    });
    const exec = vi.spyOn(RegExp.prototype, 'exec').mockImplementation(function (this: RegExp, input: string) {
      const from = this.lastIndex;
      const out = realExec.call(this, input);
      if (this.source === source) steps += ((out ? out.index + out[0].length : input.length) - from) * spelled;
      return out;
    });
    try {
      await collectScan(content({ files }), query, 100_000);
    } finally {
      text.mockRestore();
      exec.mockRestore();
    }
    return rule + steps / STEPS_PER_RULE_CHAR;
  }

  it('a long needle over repetitive text costs at most twice the rule on every row', async () => {
    setPatchTextBudgetForTest(1 << 30);
    // The rule on every row reads each row's text, its prefix included.
    const everyRow = 101 * 1000;
    // Every row is a wide line, so the backend never packs this text.
    const files = repeatedFiles('é'.repeat(100), 1000);
    for (const query of [`${'é'.repeat(63)}x`, `${'é'.repeat(11)}x`, `${'é'.repeat(5)}x`]) {
      expect(await work(files, query), `${query.length}`).toBeLessThanOrEqual(2 * everyRow);
    }
  });
});
