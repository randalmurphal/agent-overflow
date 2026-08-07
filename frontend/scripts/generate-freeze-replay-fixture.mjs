#!/usr/bin/env node
// Build the freeze-replay corpus that `src/lib/markdown/freezeReplay.manual.ts`
// drives, by reading the local agent-overflow SQLite store READ-ONLY.
//
//   cd frontend
//   node scripts/generate-freeze-replay-fixture.mjs --thread <thread-id> --turns 109-118
//
//   --thread <id>     thread whose items to capture (required)
//   --turns  <a-b|n>  inclusive turn_index range, or a single turn (required)
//   --db     <path>   defaults to $XDG_CONFIG_HOME (or ~/.config)/agent-overflow/agent-overflow.db
//   --out    <path>   defaults to src/lib/markdown/__fixtures__/freezeReplay.fixture.json
//
// WHAT IT WRITES, and why you must not commit it: the output is the VERBATIM
// text of everything that streamed in those turns — the model's prose, the
// tool inputs, and every byte of command output. That is real code and real
// conversation. The output path is gitignored (see the repo `.gitignore`);
// keep it that way, and do not paste excerpts into a bug report either.
//
// Shape (matches `FixturePayload` / `FixtureItem` in the manual driver):
//
//   [{ turn, item, kind, tool, summary, meta,
//      payload:      { id, kind, text, headLen, boundaries } | null,
//      inputPayload: { … } | null }]
//
// `text` is the stored head (`payloads.data`) followed by every appended
// chunk (`payload_chunks`, in `chunk_index` order) — i.e. the full string the
// row ended up rendering. `headLen` is the head's length and `boundaries` is
// the cumulative end offset of each successive piece, so replaying at those
// offsets reproduces the ACTUAL wire chunking the renderer saw. That is the
// part a synthetic corpus cannot fake, and the part the freeze depended on.
//
// Reads go through the `sqlite3` CLI with `-readonly` rather than a Node
// driver: no dependency to install, and a live app holding the WAL open stays
// untouched. Rows come back as one `json_object(...)` per line — SQLite's own
// JSON writer does the escaping, so embedded newlines and quotes survive
// intact and each row stays on a single line.

import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = path.dirname(fileURLToPath(import.meta.url));

const DEFAULT_DB = path.join(
  process.env.XDG_CONFIG_HOME || path.join(homedir(), '.config'),
  'agent-overflow',
  'agent-overflow.db',
);
const DEFAULT_OUT = path.join(
  HERE,
  '..',
  'src',
  'lib',
  'markdown',
  '__fixtures__',
  'freezeReplay.fixture.json',
);

function usage(message) {
  if (message) process.stderr.write(`error: ${message}\n\n`);
  process.stderr.write(
    'usage: node scripts/generate-freeze-replay-fixture.mjs --thread <id> --turns <a-b|n>\n' +
      '                                                     [--db <path>] [--out <path>]\n' +
      `\n  --db  defaults to ${DEFAULT_DB}\n` +
      `  --out defaults to ${path.relative(process.cwd(), DEFAULT_OUT)}\n` +
      '\nThe output is a verbatim capture of real session content. It is gitignored.\n' +
      'Never commit it and never paste it into an issue.\n',
  );
  process.exit(message ? 2 : 0);
}

function parseArgs(argv) {
  const args = { db: DEFAULT_DB, out: DEFAULT_OUT };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    if (flag === '--help' || flag === '-h') usage();
    const value = argv[i + 1];
    if (value === undefined || value.startsWith('--')) usage(`${flag} needs a value`);
    i += 1;
    if (flag === '--thread') args.thread = value;
    else if (flag === '--turns') args.turns = value;
    else if (flag === '--db') args.db = value;
    else if (flag === '--out') args.out = value;
    else usage(`unknown flag ${flag}`);
  }
  if (!args.thread) usage('--thread is required');
  if (!args.turns) usage('--turns is required');
  // Interpolated into SQL below (the sqlite3 CLI takes no bind parameters), so
  // the values are constrained to shapes that cannot carry a quote.
  if (!/^[A-Za-z0-9_-]+$/.test(args.thread)) usage('--thread must be an id, not a quoted string');
  const range = /^(\d+)(?:-(\d+))?$/.exec(args.turns);
  if (!range) usage('--turns must be `N` or `A-B`');
  args.fromTurn = Number(range[1]);
  args.toTurn = Number(range[2] ?? range[1]);
  if (args.toTurn < args.fromTurn) usage('--turns range is inverted');
  return args;
}

/** One `json_object(...)` per row, parsed line by line. */
function query(db, sql) {
  let out;
  try {
    out = execFileSync('sqlite3', ['-readonly', '-noheader', '-list', db, sql], {
      encoding: 'utf8',
      maxBuffer: 512 * 1024 * 1024,
    });
  } catch (err) {
    if (err.code === 'ENOENT') {
      throw new Error('the `sqlite3` CLI is not on PATH — install it and re-run');
    }
    throw new Error(`sqlite3 failed: ${err.stderr || err.message}`);
  }
  return out
    .split('\n')
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line));
}

const args = parseArgs(process.argv.slice(2));

const itemRows = query(
  args.db,
  `SELECT json_object(
     'turn', turn_index,
     'item', item_index,
     'kind', kind,
     'tool', tool_name,
     'summary', summary,
     'meta', meta,
     'payloadId', COALESCE(payload_id, ''),
     'inputPayloadId', COALESCE(input_payload_id, '')
   )
   FROM items
   WHERE thread_id = '${args.thread}'
     AND turn_index BETWEEN ${args.fromTurn} AND ${args.toTurn}
   ORDER BY turn_index, item_index;`,
);

if (itemRows.length === 0) {
  process.stderr.write(
    `no items for thread ${args.thread} turns ${args.fromTurn}-${args.toTurn} in ${args.db}\n`,
  );
  process.exit(1);
}

const payloadIds = new Set();
for (const row of itemRows) {
  if (row.payloadId) payloadIds.add(row.payloadId);
  if (row.inputPayloadId) payloadIds.add(row.inputPayloadId);
}
// Ids come from the store itself, but they end up inside a SQL literal list —
// refuse anything that could close the quote rather than escaping it.
for (const id of payloadIds) {
  if (id.includes("'")) throw new Error(`payload id contains a quote: ${JSON.stringify(id)}`);
}
const idList = [...payloadIds].map((id) => `'${id}'`).join(',');

const heads = new Map();
const chunks = new Map();

if (payloadIds.size > 0) {
  for (const row of query(
    args.db,
    `SELECT json_object('id', id, 'kind', kind, 'text', CAST(data AS TEXT))
     FROM payloads WHERE id IN (${idList});`,
  )) {
    heads.set(row.id, row);
  }
  for (const row of query(
    args.db,
    `SELECT json_object('id', payload_id, 'n', chunk_index, 'text', CAST(data AS TEXT))
     FROM payload_chunks WHERE payload_id IN (${idList})
     ORDER BY payload_id, chunk_index;`,
  )) {
    if (!chunks.has(row.id)) chunks.set(row.id, []);
    chunks.get(row.id).push(row);
  }
}

/**
 * Reassemble one payload's full text plus the wire boundaries that built it.
 *
 * Boundaries are the running JS string length, NOT the stored
 * `payload_chunks.start_offset`. The store counts bytes; the driver slices JS
 * strings, whose length is UTF-16 code units. The two agree only for ASCII, and
 * any non-ASCII byte would push every later boundary past the position the
 * renderer actually saw — which is precisely the detail the replay depends on.
 */
function buildPayload(id) {
  const head = id ? heads.get(id) : undefined;
  if (!head) return null;
  const pieces = [head.text ?? '', ...(chunks.get(id) ?? []).map((c) => c.text ?? '')];
  const boundaries = [];
  let at = 0;
  for (const piece of pieces) {
    at += piece.length;
    boundaries.push(at);
  }
  return {
    id,
    kind: head.kind,
    text: pieces.join(''),
    headLen: pieces[0].length,
    boundaries,
  };
}

const fixture = itemRows.map((row) => ({
  turn: row.turn,
  item: row.item,
  kind: row.kind,
  tool: row.tool,
  summary: row.summary,
  meta: row.meta,
  payload: buildPayload(row.payloadId),
  inputPayload: buildPayload(row.inputPayloadId),
}));

mkdirSync(path.dirname(args.out), { recursive: true });
writeFileSync(args.out, JSON.stringify(fixture), 'utf8');

const chars = fixture.reduce(
  (total, it) => total + (it.payload?.text.length ?? 0) + (it.inputPayload?.text.length ?? 0),
  0,
);
process.stderr.write(
  `wrote ${args.out}\n  ${fixture.length} items, ${payloadIds.size} payloads, ${chars} chars\n` +
    '  NOT COMMITTABLE — real session content; the path is gitignored.\n',
);
