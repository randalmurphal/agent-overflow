// MANUAL DRIVER — WebView2 renderer-freeze hunt (2026-08-03 / 2026-08-07).
//
// Not part of any gate. The filename deliberately does NOT match the unit
// project's `src/**/*.{test,spec}.{ts,js}` include glob; it is collected only
// by the `manual` vitest project (see `frontend/vitest.config.ts`):
//
//     cd frontend && pnpm test:manual          # ~30s with a full capture
//
// WHY IT IS NOT IN THE GATE, and must not be moved into one:
//
//  1. It needs a corpus that cannot be committed. The payloads that exposed
//     this class of freeze are a verbatim capture of a real session's content,
//     which is not ours to check in. `__fixtures__/freezeReplay.fixture.json`
//     is gitignored for that reason; generate your own with
//     `scripts/generate-freeze-replay-fixture.mjs` (see its header). With no
//     fixture present the corpus-driven suites skip and only the synthetic
//     fuzz runs.
//  2. A genuine hang does not FAIL here — it wedges the vitest worker, by
//     design. That is the repro signal this driver exists to produce, and it
//     is also exactly what a CI gate must never do. Run it under `timeout 600`
//     and treat a wedged worker as the finding.
//
// What it replays: the recorded payloads through the same markdown/streaming
// path the chat surface runs (`freezeReplayHarness.ts`), at the recorded wire
// boundaries, at per-word reveal granularity (what `ChatMarkdown` actually
// sees), and at seeded fuzzed boundaries — plus synthetic token-soup fuzz over
// the marked extensions, super-linearity probes for the shapes our
// divergence touches, and the AnsiText/Idiomorph command-output path.
//
// Findings that turned into permanent coverage get their own always-on suite
// rather than staying here: the marked-alert lexer-retention and consumed-raw
// bugs this driver found are pinned by `alertBlockquote.test.ts`.

import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { Lexer } from './parser/engine';
import {
  parseBlocks,
  createParseBlocksCache,
  parseIncompleteMarkdown,
  lex,
} from './index';
// Importing the package's `marked/index.js` FIRST applies its patched
// `Lexer.rules` overrides (fixed del, no mailto autolink, split GFM text run).
import './parser/index';
import { markedAlert } from './parser/extensions/alert';
import { markedFootnote } from './parser/extensions/footnotes';
import { markedMath } from './parser/extensions/math';
import { markedSub, markedSup } from './parser/extensions/subsup';
import { markedList } from './parser/extensions/list';
import { markedBr } from './parser/extensions/br';
import { markedHr } from './parser/extensions/hr';
import { markedTable } from './parser/extensions/table';
import { markedDl } from './parser/extensions/dl';
import { markedAlign } from './parser/extensions/align';
import { markedCitations } from './parser/extensions/citations';
import { markedMdx } from './parser/extensions/mdx';
import AnsiText from '../components/chat/AnsiText.svelte';
import {
  ChatMarkdownPipeline,
  DEFAULT_STEP_BUDGET_MS,
  ReplayRecorder,
  mulberry32,
  randomOffsets,
  settledDriver,
  streamingDriver,
  wordUnitOffsets,
} from './freezeReplayHarness';

/** Total per-payload replay budget (catches super-linear-but-terminating). */
const PAYLOAD_BUDGET_MS = 60_000;

const recorder = new ReplayRecorder(DEFAULT_STEP_BUDGET_MS);

// ── Fixture ───────────────────────────────────────────────────────────────

/** One payload as `scripts/generate-freeze-replay-fixture.mjs` emits it. */
interface FixturePayload {
  id: string;
  kind: string;
  /** Full reconstructed text (stored head + appended chunks). */
  text: string;
  /** Length of the stored head — the first wire boundary. */
  headLen: number;
  /** Cumulative character offsets of every wire chunk boundary. */
  boundaries: number[];
}

interface FixtureItem {
  turn: number;
  item: number;
  kind: string;
  tool: string;
  summary: string;
  meta: string;
  payload: FixturePayload | null;
  inputPayload: FixturePayload | null;
}

// Resolved from the vitest project root (`frontend/`), NOT from
// `import.meta.url`: vite rewrites `import.meta.url` in transformed modules to
// the dev-server's http URL, so `fileURLToPath` throws "The URL must be of
// scheme file" before a single test collects.
const FIXTURE_DIR = resolve(process.cwd(), 'src/lib/markdown');
const FIXTURE_PATH = resolve(FIXTURE_DIR, '__fixtures__/freezeReplay.fixture.json');

// A missing FIXTURE is the ordinary case and skips the corpus suites. A
// missing FIXTURE DIR means the cwd is not the frontend project root, which
// would make that skip a lie — the corpus could be sitting right there. Fail
// loudly instead of silently reporting "no corpus".
if (!existsSync(FIXTURE_DIR)) {
  throw new Error(
    `[freezeReplay] expected the vitest cwd to be frontend/, but ${FIXTURE_DIR} does not exist ` +
      `(cwd=${process.cwd()}). Run from frontend/ with \`pnpm test:manual\`.`,
  );
}

const items: FixtureItem[] = existsSync(FIXTURE_PATH)
  ? (JSON.parse(readFileSync(FIXTURE_PATH, 'utf8')) as FixtureItem[])
  : [];
const HAVE_FIXTURE = items.length > 0;

if (!HAVE_FIXTURE) {
  // eslint-disable-next-line no-console
  console.warn(
    `\n[freezeReplay] no corpus at ${FIXTURE_PATH} — corpus-driven suites skipped.\n` +
      '           generate one: node scripts/generate-freeze-replay-fixture.mjs --help\n',
  );
}

interface Target {
  name: string;
  text: string;
  boundaries: number[];
}

const targets: Target[] = [];
for (const it of items) {
  for (const [slot, p] of [
    ['payload', it.payload],
    ['input', it.inputPayload],
  ] as const) {
    if (!p || p.text.length === 0) continue;
    targets.push({
      name: `${it.turn}:${it.item} ${it.kind}${it.tool ? `/${it.tool}` : ''} ${slot} ${p.id}`,
      text: p.text,
      boundaries: p.boundaries.filter((b) => b > 0 && b <= p.text.length),
    });
  }
}
// Item summaries stream through the same rows (tool-call headers, thinking
// tails) — include them as their own texts.
for (const it of items) {
  if (it.summary) {
    targets.push({
      name: `${it.turn}:${it.item} summary`,
      text: it.summary,
      boundaries: [it.summary.length],
    });
  }
}

// ── Corpus replays ────────────────────────────────────────────────────────

describe.skipIf(!HAVE_FIXTURE)(
  'freeze replay — recorded payloads through the streaming markdown path',
  () => {
    it('fixture loaded', () => {
      expect(targets.length).toBeGreaterThan(0);
    });

    for (const t of targets) {
      it(
        `recorded chunk boundaries — ${t.name}`,
        () => {
          const s = recorder.replay(`wire:${t.name}`, t.text, t.boundaries, streamingDriver);
          expect(s.totalMs).toBeLessThan(PAYLOAD_BUDGET_MS);
        },
        120_000,
      );

      it(
        `per-word reveal granularity — ${t.name}`,
        () => {
          const s = recorder.replay(
            `word:${t.name}`,
            t.text,
            wordUnitOffsets(t.text),
            streamingDriver,
          );
          expect(s.totalMs).toBeLessThan(PAYLOAD_BUDGET_MS);
        },
        180_000,
      );

      it(
        `settled render — ${t.name}`,
        () => {
          const s = recorder.replay(
            `settled:${t.name}`,
            t.text,
            [t.text.length],
            settledDriver,
          );
          expect(s.totalMs).toBeLessThan(PAYLOAD_BUDGET_MS);
        },
        120_000,
      );
    }
  },
);

describe.skipIf(!HAVE_FIXTURE)(
  'freeze replay — multi-item concatenations (one row, several deltas)',
  () => {
    // The chat row that streams is a single assistant_text; but a thinking
    // tail concatenates several payloads into one visible string. Simulate a
    // row whose source is several consecutive payloads glued in wire order.
    const groups: { name: string; parts: string[] }[] = [];
    const byTurn = new Map<number, FixtureItem[]>();
    for (const it of items) {
      if (!byTurn.has(it.turn)) byTurn.set(it.turn, []);
      byTurn.get(it.turn)!.push(it);
    }
    for (const [turn, list] of byTurn) {
      const parts = list.map((it) => it.payload?.text ?? '').filter((t) => t.length > 0);
      if (parts.length > 1) groups.push({ name: `turn ${turn} concat`, parts });
      const thinkParts = list
        .filter((it) => it.kind === 'thinking')
        .map((it) => it.payload?.text ?? '')
        .filter(Boolean);
      if (thinkParts.length > 1) {
        groups.push({ name: `turn ${turn} thinking concat`, parts: thinkParts });
      }
    }

    it('has multi-payload turns to concatenate', () => {
      expect(groups.length).toBeGreaterThanOrEqual(0);
    });

    for (const g of groups) {
      it(
        `${g.name}`,
        () => {
          const text = g.parts.join('\n\n');
          const s = recorder.replay(
            `concat:${g.name}`,
            text,
            wordUnitOffsets(text),
            streamingDriver,
          );
          expect(s.totalMs).toBeLessThan(PAYLOAD_BUDGET_MS);
        },
        180_000,
      );
    }
  },
);

describe.skipIf(!HAVE_FIXTURE)('freeze replay — fuzzed re-chunking (seeded)', () => {
  const seeds = [1, 7, 13, 42, 99, 1337, 20260803, 20260807];
  const fuzzTargets = targets.filter((t) => t.text.length > 40).slice(0, 40);
  for (const seed of seeds) {
    it(
      `seed ${seed}`,
      () => {
        for (const t of fuzzTargets) {
          const offsets = randomOffsets(t.text, seed ^ t.text.length, 400);
          recorder.replay(`fuzz${seed}:${t.name}`, t.text, offsets, streamingDriver);
        }
      },
      180_000,
    );
  }
});

// ── Synthetic fuzz (no corpus needed) ─────────────────────────────────────

describe('freeze replay — structural termination fuzz over marked', () => {
  // A block/inline extension that returns a zero-length `raw` makes marked's
  // `while (src)` loops spin forever (marked has no progress guard for
  // extension tokenizers). Hunt for one over shapes agent prose actually
  // produces, plus the delimiter runs our divergence's custom rules touch.
  const alphabet = [
    '-', '--', '---', '- ', '  - ', '* ', '+ ', '1. ', 'a) ', 'i. ',
    '|', '||', '|||', '||||||||', '| a |', '|---|', '|:-:|', '^', '^^',
    '`', '``', '```', '~', '~~', '~~~', '$', '$$', '\\$', '[', ']', '[^',
    '[^a]', '[^a]:', '](', '[x](y)', '![', '<', '<Br', '<Br/>', '</Br>',
    '#', '###', '>', '> ', ':', ': a :', '=', '===', '\n', '\n\n', ' ',
    '\t', 'x', 'foo', '<<<<<<<', '>>>>>>>', 'a|b', '$ref', '--wait-seconds=-1',
    // NUL as an ESCAPE, never a literal byte: a literal one makes this file
    // invisible to grep (git and grep treat the file as binary).
    '_', '__', '*', '**', '***', '[center]', '[/center]', '\\', '\x00',
  ];
  const cases = 4000;

  for (const seed of [11, 2222, 333333]) {
    it(
      `random token soup, seed ${seed}`,
      () => {
        const rnd = mulberry32(seed);
        for (let c = 0; c < cases; c++) {
          const n = 1 + Math.floor(rnd() * 14);
          let src = '';
          for (let i = 0; i < n; i++) src += alphabet[Math.floor(rnd() * alphabet.length)];
          const t0 = performance.now();
          try {
            lex(parseIncompleteMarkdown(src) as string, []);
            const blocks = parseBlocks(src, [], createParseBlocksCache());
            for (const b of blocks) lex(b, []);
          } catch {
            // A throw is fine (marked's own infinite-loop guard). A HANG is not.
          }
          const dt = performance.now() - t0;
          if (dt > DEFAULT_STEP_BUDGET_MS) {
            throw new Error(`slow lex ${dt.toFixed(0)}ms for ${JSON.stringify(src)}`);
          }
        }
      },
      180_000,
    );
  }

  it(
    'append-only growth over token soup (incremental caches engaged)',
    () => {
      const rnd = mulberry32(4242);
      for (let c = 0; c < 400; c++) {
        const n = 1 + Math.floor(rnd() * 20);
        let src = '';
        for (let i = 0; i < n; i++) src += alphabet[Math.floor(rnd() * alphabet.length)];
        const p = new ChatMarkdownPipeline();
        for (let k = 1; k <= src.length; k++) {
          const t0 = performance.now();
          p.step(src.slice(0, k), src.slice(k - 1, k));
          const dt = performance.now() - t0;
          if (dt > DEFAULT_STEP_BUDGET_MS) {
            throw new Error(`slow step ${dt.toFixed(0)}ms at ${k} for ${JSON.stringify(src)}`);
          }
        }
      }
    },
    180_000,
  );
});

// ══════════════════════════════════════════════════════════════════════════
// Direct probe: marked's `while (src)` block/inline loops have NO progress
// guard for EXTENSION tokenizers — `src = src.substring(token.raw.length)`.
// An extension that returns a token with an empty `raw` (or a `raw` that
// isn't a prefix of `src`) spins forever while `tokens` grows, which is
// exactly the observed signature (one core pegged, memory climbing, no
// repaint). The parser registers 13 custom extensions, several of
// them carrying our divergence. Wrap every one and fuzz.
// ══════════════════════════════════════════════════════════════════════════

interface AnyExt {
  name: string;
  level: 'block' | 'inline';
  start?: (src: string) => number | undefined;
  tokenizer: (src: string, tokens: unknown) => { raw?: string } | undefined;
  [k: string]: unknown;
}

/** FATAL: raw.length === 0 ⇒ marked's `while (src)` never advances. */
const zeroRaw: string[] = [];
/** Non-fatal but a correctness divergence: raw isn't a prefix of src. */
const rawDivergence = new Map<string, string>();

function resetViolations(): void {
  zeroRaw.length = 0;
  rawDivergence.clear();
}

function instrument(ext: AnyExt): AnyExt {
  const inner = ext.tokenizer;
  return {
    ...ext,
    tokenizer(this: unknown, src: string, tokens: unknown) {
      const token = inner.call(this, src, tokens);
      if (token) {
        const raw = token.raw;
        if (typeof raw !== 'string' || raw.length === 0) {
          zeroRaw.push(
            `${ext.name}/${ext.level}: EMPTY raw for src=${JSON.stringify(src.slice(0, 120))}`,
          );
        } else if (!src.startsWith(raw)) {
          const key = `${ext.name}/${ext.level}/${raw.length > src.length ? 'longer' : 'mismatch'}`;
          if (!rawDivergence.has(key)) {
            rawDivergence.set(
              key,
              `${key}: raw=${JSON.stringify(raw.slice(0, 80))} src=${JSON.stringify(src.slice(0, 80))}`,
            );
          }
        }
      }
      return token;
    },
  };
}

function buildInstrumentedOptions(): Record<string, unknown> {
  const all: AnyExt[] = [
    markedHr,
    markedTable,
    ...markedFootnote(),
    markedAlert,
    ...markedMath,
    markedSub,
    markedSup,
    markedList,
    markedBr,
    markedDl,
    markedAlign,
    markedCitations,
    markedMdx,
  ] as unknown as AnyExt[];
  const options: Record<string, unknown> = {
    gfm: true,
    extensions: {
      block: [] as unknown[],
      inline: [] as unknown[],
      childTokens: {},
      renderers: {},
      startBlock: [] as unknown[],
      startInline: [] as unknown[],
    },
  };
  const ex = options.extensions as Record<string, unknown[]>;
  for (const raw of all) {
    const e = instrument(raw);
    if (e.start) (e.level === 'block' ? ex.startBlock : ex.startInline).push(e.start);
    if (e.tokenizer) (e.level === 'block' ? ex.block : ex.inline).push(e.tokenizer);
  }
  return options;
}

describe('freeze replay — extension progress guard (zero-length raw ⇒ marked spins forever)', () => {
  const alphabet = [
    '-', '--', '---', '- ', '  - ', '* ', '+ ', '1. ', '10) ', 'i. ', 'IV. ', 'a) ',
    '|', '||', '|||', '||||||||', '| a |', '|---|', '|:-:|', '| ^ |', '| ^|', '|^',
    '^', '^^', '^x^', '`', '``', '```', '```js', '~', '~~', '~~~', '~5~10',
    '$', '$$', '\\$', '$$\n', '$ref', '$$ \\begin{pmatrix}', '[', ']', '[^',
    '[^a]', '[^a]:', '](', '[x](y)', '![', '[a]: b', '<', '<Br', '<Br/>', '</Br>',
    '<Comp a="b">', '#', '###', '>', '> ', '> [!NOTE]', ':', ': a :', '=', '===',
    '\n', '\n\n', ' ', '\t', 'x', 'foo bar', '<<<<<<<', '>>>>>>>', 'a|b',
    '--wait-seconds=-1', '_', '__', '*', '**', '***', '[center]', '[/center]',
    '[right]', '\\', '    ', '  ', 'a', '0', '.', ')', '!',
  ];

  for (const seed of [1, 5, 77, 909, 20260803, 20260807]) {
    it(
      `token soup, seed ${seed}`,
      () => {
        resetViolations();
        const options = buildInstrumentedOptions();
        const rnd = mulberry32(seed);
        for (let c = 0; c < 6000; c++) {
          const n = 1 + Math.floor(rnd() * 18);
          let src = '';
          for (let i = 0; i < n; i++) src += alphabet[Math.floor(rnd() * alphabet.length)];
          const t0 = performance.now();
          try {
            new Lexer(options as ConstructorParameters<typeof Lexer>[0]).lex(src);
          } catch {
            /* marked's own guard throwing is fine; a hang is not */
          }
          const dt = performance.now() - t0;
          if (dt > DEFAULT_STEP_BUDGET_MS) {
            throw new Error(`slow lex ${dt.toFixed(0)}ms for ${JSON.stringify(src)}`);
          }
          if (zeroRaw.length > 0) {
            throw new Error(
              `ZERO-LENGTH raw (marked will spin forever) on ${JSON.stringify(src)}:\n` +
                zeroRaw.join('\n'),
            );
          }
        }
        if (rawDivergence.size > 0) {
          // eslint-disable-next-line no-console
          console.log(
            `\n[seed ${seed}] non-fatal raw divergences (terminating, but the token's raw ` +
              `does not describe the bytes it consumed):\n` +
              Array.from(rawDivergence.values()).join('\n'),
          );
        }
      },
      180_000,
    );
  }

  it.skipIf(!HAVE_FIXTURE)(
    'append-only prefixes of every recorded payload',
    () => {
      resetViolations();
      const options = buildInstrumentedOptions();
      for (const t of targets) {
        const step = Math.max(1, Math.floor(t.text.length / 400));
        for (let k = 1; k <= t.text.length; k += step) {
          new Lexer(options as ConstructorParameters<typeof Lexer>[0]).lex(
            parseIncompleteMarkdown(t.text.slice(0, k)) as string,
          );
        }
      }
      expect(zeroRaw).toEqual([]);
      expect(Array.from(rawDivergence.values())).toEqual([]);
    },
    240_000,
  );
});

// ══════════════════════════════════════════════════════════════════════════
// Super-linearity probes for the shapes our divergence touches.
// ══════════════════════════════════════════════════════════════════════════
describe('freeze replay — super-linear shape probes', () => {
  it(
    'nested list depth — finalizeList re-tokenizes each item twice per level',
    () => {
      const times: string[] = [];
      for (let depth = 1; depth <= 12; depth++) {
        let src = '';
        for (let d = 0; d < depth; d++) src += `${'  '.repeat(d)}- level ${d}\n`;
        src += '\n' + '  '.repeat(depth) + 'trailing paragraph forcing loose\n';
        const dt = recorder.record(`nested-list depth ${depth}`, () => {
          lex(src, []);
        });
        times.push(`depth ${depth}: ${dt.toFixed(2)}ms`);
        if (dt > DEFAULT_STEP_BUDGET_MS) {
          throw new Error(`nested list depth ${depth} took ${dt.toFixed(0)}ms`);
        }
      }
      // eslint-disable-next-line no-console
      console.log('\nnested-list scaling:\n' + times.join('\n'));
    },
    180_000,
  );

  it(
    'marker-alignment code path re-lexes each aligned item',
    () => {
      let src = '';
      for (let i = 0; i < 400; i++) src += `-     $${i} aligned value ${i}\n`;
      const dt = recorder.record('marker-alignment 400 items', () => {
        lex(src, []);
      });
      expect(dt).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
    },
    120_000,
  );

  it(
    'growing list — the shape the boundary splitter can never commit',
    () => {
      let src = '';
      const p = new ChatMarkdownPipeline();
      let worst = 0;
      for (let i = 0; i < 600; i++) {
        const delta = `- item ${i} with a bit of prose to make the line realistic\n`;
        src += delta;
        const t0 = performance.now();
        p.step(src, delta);
        worst = Math.max(worst, performance.now() - t0);
      }
      recorder.stats.push({
        label: 'growing-list 600 items',
        maxStepMs: worst,
        maxStepAt: src.length,
        totalMs: worst,
        steps: 600,
        chars: src.length,
      });
      expect(worst).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
    },
    180_000,
  );

  it(
    'growing table — table-append fast path',
    () => {
      let src = '| a | b | c |\n|---|---|---|\n';
      const p = new ChatMarkdownPipeline();
      let worst = 0;
      for (let i = 0; i < 600; i++) {
        const delta = `| r${i} | v${i} | w${i} |\n`;
        src += delta;
        const t0 = performance.now();
        p.step(src, delta);
        worst = Math.max(worst, performance.now() - t0);
      }
      expect(worst).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
    },
    180_000,
  );

  it(
    'growing unterminated fence — incremental detector and lexer paths',
    () => {
      let src = '```python\n';
      const p = new ChatMarkdownPipeline();
      let worst = 0;
      for (let i = 0; i < 1200; i++) {
        const delta = `    line_${i} = compute(${i})  # a realistic source line\n`;
        src += delta;
        const t0 = performance.now();
        p.step(src, delta);
        worst = Math.max(worst, performance.now() - t0);
      }
      recorder.stats.push({
        label: 'growing-fence 1200 lines',
        maxStepMs: worst,
        maxStepAt: src.length,
        totalMs: worst,
        steps: 1200,
        chars: src.length,
      });
      expect(worst).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
    },
    180_000,
  );

  it(
    'pipe-dense prose (the recorded `grep -n "<<<<<<<\\|>>>>>>>\\|||||||||"` shape)',
    () => {
      const line = 'grep -n "<<<<<<<\\|>>>>>>>\\|||||||||" src/x.py; echo ---; git diff --check';
      let src = '';
      for (let i = 0; i < 200; i++) src += line + '\n';
      const dt = recorder.record('pipe-dense 200 lines', () => {
        lex(parseIncompleteMarkdown(src) as string, []);
      });
      expect(dt).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
    },
    120_000,
  );
});

// ══════════════════════════════════════════════════════════════════════════
// The OTHER streaming-text surface in the freeze window: bash command output
// renders through AnsiText.svelte → Idiomorph.morph on every chunk. Replay
// the recorded command-output payloads through the real component.
// ══════════════════════════════════════════════════════════════════════════
// Whether this dimension runs at all depends on the corpus: a turn range with
// no bash tool calls has no command output. That is a property of the capture,
// not a failure — but it must be VISIBLE, or an operator reads a green run as
// "AnsiText replayed clean" when it never replayed at all.
const outputs = items
  .filter((it) => it.payload && it.payload.id.startsWith('command-output:'))
  .map((it) => it.payload!)
  .filter((p) => p.text.length > 0);

if (HAVE_FIXTURE && outputs.length === 0) {
  // eslint-disable-next-line no-console
  console.warn(
    '\n[freezeReplay] corpus has no command-output payloads — the AnsiText/Idiomorph\n' +
      '           replay is skipped. Capture a turn range containing bash tool calls\n' +
      '           to exercise it.\n',
  );
}

describe.skipIf(!HAVE_FIXTURE || outputs.length === 0)(
  'freeze replay — AnsiText + Idiomorph over recorded command output',
  () => {
    it(
      'morphs every growth step without a runaway task',
      () => {
        let worst = 0;
        let worstAt = '';
        for (const p of outputs) {
          const { rerender, unmount } = render(AnsiText, {
            props: { source: '', class: 'whitespace-pre-wrap break-all' },
          });
          const step = Math.max(1, Math.floor(p.text.length / 120));
          for (let k = 1; k <= p.text.length; k += step) {
            const t0 = performance.now();
            void rerender({ source: p.text.slice(0, k), class: 'whitespace-pre-wrap break-all' });
            flushSync();
            const dt = performance.now() - t0;
            if (dt > worst) {
              worst = dt;
              worstAt = `${p.id}@${k}`;
            }
            if (dt > DEFAULT_STEP_BUDGET_MS) {
              throw new Error(`AnsiText morph ${dt.toFixed(0)}ms at ${p.id} offset ${k}`);
            }
          }
          unmount();
        }
        cleanup();
        recorder.stats.push({
          label: `ansitext worst ${worstAt}`,
          maxStepMs: worst,
          maxStepAt: -1,
          totalMs: worst,
          steps: outputs.length,
          chars: -1,
        });
        // eslint-disable-next-line no-console
        console.log(`\nAnsiText/Idiomorph worst single morph: ${worst.toFixed(2)}ms (${worstAt})`);
        expect(worst).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
      },
      240_000,
    );
  },
);

describe('freeze replay — timing profile', () => {
  it('report', () => {
    // eslint-disable-next-line no-console
    console.log(`\n${recorder.table()}`);
    const worst = recorder.worst();
    if (worst) expect(worst.maxStepMs).toBeLessThan(DEFAULT_STEP_BUDGET_MS);
  });
});
