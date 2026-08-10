#!/usr/bin/env node
// Build the freeze-reproduction fixtures the agent harness replays:
//
//   node e2e/scripts/generate-freeze-repro.mjs \
//     --thread <id> --seed-turns 220-243 --live-turns 244-247
//
//   --thread     <id>    thread whose turns to capture (required)
//   --seed-turns <a-b>   turns written as COMPLETED history via HarnessSeed
//   --live-turns <a-b>   turns replayed LIVE through the mock provider
//   --db         <path>  defaults to the real app store (read-only)
//   --out        <dir>   defaults to e2e/fixtures/freeze-repro/ (gitignored)
//   --gap-ms     <n>     delay between items inside a live turn (default 150)
//   --delta-ms   <n>     delayBetweenMs inside one item's emit step (default 35)
//   --scale      <f>     multiplies both pacing knobs (default 1)
//
// Writes three files into --out:
//
//   seed.json      a HarnessSeedSpec: one project, one thread, the seed turns
//                  as completed history with the REAL payload text.
//   scenario.json  a Claude mock scenario whose turns[] replay one live turn
//                  each, in recorded item order, with thinking/text deltas cut
//                  at the REAL payload_chunks boundaries.
//   manifest.json  what was generated, what was skipped, and the per-turn user
//                  text the driver sends (one SendMessage per live turn).
//
// WHAT IT WRITES, and why you must not commit it: the output is the verbatim
// text of a real session — the model's prose, tool inputs, and every byte of
// command output. That is real code and real conversation. The output path is
// gitignored; keep it that way, and do not paste excerpts into a bug report.

import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { DEFAULT_DB, groupByTurn, query, readThreadItems } from './freeze-repro/db.mjs';
import {
  fileChangeToolUseResult,
  resultLine,
  streamedBlockLines,
  taskNotificationLine,
  taskStartedLine,
  taskUpdatedLine,
  toolResultLine,
  toolUseLine,
} from './freeze-repro/wire.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const DEFAULT_OUT = path.join(HERE, '..', 'fixtures', 'freeze-repro');

const SUBSTITUTED_TOKENS = ['SESSION_ID', 'THREAD_ID', 'TURN', 'TURN_ID', 'REQUEST_ID', 'CWD'];
const TASK_OUTPUT_DIR = 'ao-freeze-repro';

function usage(message) {
  if (message) process.stderr.write(`error: ${message}\n\n`);
  process.stderr.write(
    'usage: node e2e/scripts/generate-freeze-repro.mjs --thread <id> \\\n' +
      '         --seed-turns <a-b> --live-turns <a-b> [--db <path>] [--out <dir>]\\\n' +
      '         [--gap-ms <n>] [--delta-ms <n>] [--scale <f>]\n' +
      `\n  --db  defaults to ${DEFAULT_DB}\n` +
      `  --out defaults to ${DEFAULT_OUT}\n` +
      '\nThe output is a verbatim capture of real session content. It is gitignored.\n' +
      'Never commit it and never paste it into an issue.\n',
  );
  process.exit(message ? 2 : 0);
}

/**
 * Refuse a destination this repo could commit.
 *
 * The output is the verbatim text of a real session. `--out` accepts any
 * path, so one careless value writes it somewhere `git add -A` picks up —
 * and the content is exactly what must never leave the machine. Outside the
 * checkout is fine; inside it, git itself has to confirm the path is
 * ignored. Asking git rather than pattern-matching means the answer stays
 * right when `.gitignore` moves.
 */
function assertDestinationUncommittable(out) {
  const resolved = path.resolve(out);
  let repoRoot;
  try {
    repoRoot = execFileSync('git', ['rev-parse', '--show-toplevel'], {
      cwd: HERE,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    // No checkout around the generator: nothing here can be committed.
    return resolved;
  }
  const relative = path.relative(repoRoot, resolved);
  const inside = relative !== '' && !relative.startsWith('..') && !path.isAbsolute(relative);
  if (!inside) return resolved;
  try {
    execFileSync('git', ['check-ignore', '--quiet', '--no-index', '--', resolved], {
      cwd: repoRoot,
      stdio: 'ignore',
    });
  } catch {
    usage(
      `--out ${resolved} is inside the checkout and is NOT gitignored. `
        + 'The output is verbatim real session content — writing it to a committable '
        + 'path is how it ends up pushed. Point --out outside the repo, or add the '
        + 'path to .gitignore first.',
    );
  }
  return resolved;
}

function parseRange(raw, flag) {
  const match = /^(\d+)-(\d+)$/.exec(raw);
  if (!match) usage(`${flag} must be \`A-B\``);
  const from = Number(match[1]);
  const to = Number(match[2]);
  if (to < from) usage(`${flag} range is inverted`);
  return [from, to];
}

function parseArgs(argv) {
  const args = { db: DEFAULT_DB, out: DEFAULT_OUT, gapMs: 150, deltaMs: 35, scale: 1 };
  for (let i = 0; i < argv.length; i += 1) {
    const flag = argv[i];
    if (flag === '--help' || flag === '-h') usage();
    const value = argv[i + 1];
    if (value === undefined || value.startsWith('--')) usage(`${flag} needs a value`);
    i += 1;
    switch (flag) {
      case '--thread':
        args.thread = value;
        break;
      case '--seed-turns':
        args.seedTurns = value;
        break;
      case '--live-turns':
        args.liveTurns = value;
        break;
      case '--db':
        args.db = value;
        break;
      case '--out':
        args.out = value;
        break;
      case '--gap-ms':
        args.gapMs = Number(value);
        break;
      case '--delta-ms':
        args.deltaMs = Number(value);
        break;
      case '--scale':
        args.scale = Number(value);
        break;
      default:
        usage(`unknown flag ${flag}`);
    }
  }
  if (!args.thread) usage('--thread is required');
  if (!args.seedTurns) usage('--seed-turns is required');
  if (!args.liveTurns) usage('--live-turns is required');
  if (!/^[A-Za-z0-9_-]+$/.test(args.thread)) usage('--thread must be an id, not a quoted string');
  for (const [name, value] of [
    ['--gap-ms', args.gapMs],
    ['--delta-ms', args.deltaMs],
    ['--scale', args.scale],
  ]) {
    if (!Number.isFinite(value) || value < 0) usage(`${name} must be a non-negative number`);
  }
  [args.seedFrom, args.seedTo] = parseRange(args.seedTurns, '--seed-turns');
  [args.liveFrom, args.liveTo] = parseRange(args.liveTurns, '--live-turns');
  if (args.liveFrom <= args.seedTo) usage('--live-turns must start after --seed-turns ends');
  args.gapMs = Math.round(args.gapMs * args.scale);
  args.deltaMs = Math.round(args.deltaMs * args.scale);
  args.out = assertDestinationUncommittable(args.out);
  return args;
}

// ---------------------------------------------------------------- seed side

/** Turn one recorded row into a HarnessSeedItem. */
function seedItem(row) {
  const item = {
    kind: row.kind,
    role: row.role,
    status: row.status,
    summary: row.summary,
  };
  if (row.toolName) item.toolName = row.toolName;
  if (Object.keys(row.meta).length > 0) item.meta = row.meta;
  if (row.payload) {
    item.payload = { kind: row.payload.kind, meta: row.payload.meta, data: row.payload.text };
  }
  return item;
}

function buildSeed(byTurn, from, to, threadTitle) {
  const turns = [];
  let items = 0;
  let payloadChars = 0;
  for (let turn = from; turn <= to; turn += 1) {
    const rows = byTurn.get(turn);
    if (!rows) continue;
    const userRow = rows.find((row) => row.kind === 'user_text');
    const rest = rows.filter((row) => row !== userRow);
    for (const row of rest) payloadChars += row.payload?.text.length ?? 0;
    items += rest.length;
    turns.push({
      // HarnessSeedTurn refuses an empty userText, and a handful of real
      // rows are attachment-only. The placeholder names the source turn so
      // the substitution is visible in the UI rather than silent.
      userText: userRow?.summary?.trim() || `(recorded turn ${turn} had no user text)`,
      items: rest.map(seedItem),
    });
  }
  return {
    spec: {
      projects: [
        {
          name: 'freeze-repro',
          repo: { commits: [{ message: 'init', files: { 'README.md': '# freeze-repro\n' } }] },
          threads: [{ title: threadTitle, provider: 'claude', mode: 'chat', turns }],
        },
      ],
    },
    stats: { turns: turns.length, items, payloadChars },
  };
}

// ------------------------------------------------------------ scenario side

const TOOL_PAYLOAD_PREFIX = /^(?:command-output|tool-result|tool-call-result):(.+)$/;

function toolUseIdFor(row) {
  const fromPayload = TOOL_PAYLOAD_PREFIX.exec(row.payload?.id ?? '');
  if (fromPayload) return fromPayload[1];
  if (typeof row.meta.tool_use_id === 'string' && row.meta.tool_use_id) return row.meta.tool_use_id;
  return `tu-${row.turn}-${row.item}`;
}

/**
 * The tool_use input as it went out on the wire: `items.meta.input` holds
 * what survived shaping, and the promoted heavy fields (a Write's `content`,
 * an Edit's strings) were moved to the input payload.
 */
function toolInput(row) {
  const base = row.meta.input && typeof row.meta.input === 'object' ? row.meta.input : {};
  if (!row.inputPayload) return { ...base };
  // The promoted fields ARE the tool's input — a Write's whole `content`, an
  // Edit's strings. Dropping them on a parse failure emits a tool call the
  // model never made, and the fixture then reproduces something other than
  // the recorded session while looking fine. Corruption in the source store
  // is a reason to stop, not to guess.
  let promoted;
  try {
    promoted = JSON.parse(row.inputPayload.text);
  } catch (err) {
    throw new Error(
      `turn ${row.turn} item ${row.item} (${row.toolName || row.kind}): input payload `
        + `${row.inputPayload.id ?? ''} is not valid JSON (${err.message})`,
    );
  }
  if (!promoted || typeof promoted !== 'object' || Array.isArray(promoted)) {
    throw new Error(
      `turn ${row.turn} item ${row.item} (${row.toolName || row.kind}): input payload `
        + `${row.inputPayload.id ?? ''} decoded to ${Array.isArray(promoted) ? 'an array' : typeof promoted}, `
        + 'not the object of promoted input fields',
    );
  }
  return { ...base, ...promoted };
}

/**
 * The recorded terminal status, mapped back to the wire vocabulary.
 *
 * Hardcoding `completed` replayed every recorded failure as a success, which
 * silently removes the failure states from a repro whose whole subject is
 * how the timeline behaves under a dense turn — a failed background task
 * renders a different row, a different run summary, and a different
 * auto-collapse decision.
 *
 * Triage collapses every non-`completed` task terminal into `is_error`, so
 * the record cannot tell `failed` from `killed`; `failed` stands for both.
 * The notification envelope is better off: the parser stamps the wire status
 * into that row's meta verbatim, so use it when it survived.
 */
function taskTerminalStatus(row) {
  return row.status === 'errored' || row.meta.is_error === true ? 'failed' : 'completed';
}

function sanitizeTaskId(taskId) {
  return String(taskId).replace(/[^A-Za-z0-9._-]/g, '_') || 'task';
}

/** Steps that replay one recorded item. Returns [] for kinds with no wire shape. */
function itemSteps(row, rowsInTurn, deltaMs, counters) {
  const emit = (lines) => ({ emit: { lines, delayBetweenMs: deltaMs } });
  const messageId = `msg-${row.turn}-${row.item}`;

  switch (row.kind) {
    case 'user_text':
      // The driver sends this through SendMessage; the mock adapter echoes it.
      return [];

    case 'thinking':
    case 'assistant_text': {
      const fullText = row.payload?.text ?? row.summary;
      if (!fullText) {
        counters.emptyBlocks += 1;
        return [];
      }
      const pieces = row.payload?.pieces.length ? row.payload.pieces : [fullText];
      return [emit(streamedBlockLines({ messageId, kind: row.kind, pieces, fullText }))];
    }

    case 'tool_call': {
      const toolUseId = toolUseIdFor(row);
      const input = toolInput(row);
      const lines = [
        toolUseLine({ messageId, toolUseId, toolName: row.toolName || 'Bash', input }),
      ];

      const taskId = typeof row.meta.task_id === 'string' ? row.meta.task_id : '';
      if (row.isBackground || row.meta.is_background === true) {
        // A backgrounded launch has no inline tool_result: it stays `running`
        // until its task_updated/task_notification pair lands (which the
        // matching tool_completion row below emits).
        if (taskId) {
          lines.push(
            taskStartedLine({
              taskId,
              toolUseId,
              taskType: typeof row.meta.task_type === 'string' ? row.meta.task_type : '',
            }),
          );
        }
        counters.backgroundLaunches += 1;
        return [emit(lines)];
      }

      const isError = row.status === 'errored';
      const filePath = typeof input.file_path === 'string' ? input.file_path : '';
      let toolUseResult = null;
      if (!isError && row.payload?.kind === 'tool_result') {
        toolUseResult = fileChangeToolUseResult({
          toolName: row.toolName,
          filePath,
          storedPatch: row.payload.text,
          writtenContent: typeof input.content === 'string' ? input.content : undefined,
        });
        if (toolUseResult) counters.fileChangeResults += 1;
        else counters.fileChangeFallbacks += 1;
      }
      lines.push(
        toolResultLine({
          toolUseId,
          // A rebuilt file_change carries its diff in tool_use_result; the
          // block content is then the short ack the CLI sends, which is what
          // the recorded summary already is.
          content: toolUseResult ? row.summary : (row.payload?.text ?? row.summary),
          isError,
          toolUseResult,
        }),
      );
      return [emit(lines)];
    }

    case 'tool_completion': {
      const taskId = typeof row.meta.task_id === 'string' ? row.meta.task_id : '';
      const toolUseId = typeof row.meta.tool_use_id === 'string' ? row.meta.tool_use_id : '';
      if (!taskId || !toolUseId) {
        counters.skipped.tool_completion = (counters.skipped.tool_completion ?? 0) + 1;
        return [];
      }
      // The sibling notification row carries the human-readable report the
      // notification envelope's `summary` field produced.
      const notification = rowsInTurn.find(
        (other) => other.kind === 'notification' && other.meta.task_id === taskId,
      );
      const relPath = `${TASK_OUTPUT_DIR}/${sanitizeTaskId(taskId)}.output`;
      const steps = [];
      if (row.payload?.text) {
        // The backend READS output_file off disk, so the bytes have to exist
        // in the mock's workspace before the notification names them.
        steps.push({ writeFile: { path: relPath, content: row.payload.text } });
      }
      const terminalStatus = taskTerminalStatus(row);
      const notifiedStatus = typeof notification?.meta.status === 'string' && notification.meta.status
        ? notification.meta.status
        : terminalStatus;
      steps.push({
        emit: {
          delayBetweenMs: deltaMs,
          lines: [
            taskUpdatedLine({
              taskId,
              toolUseId,
              status: terminalStatus,
              description: notification?.summary ?? row.summary,
            }),
            taskNotificationLine({
              taskId,
              toolUseId,
              status: notifiedStatus,
              outputFile: row.payload?.text ? `\${CWD}/${relPath}` : '',
              summary: notification?.summary ?? '',
            }),
          ],
        },
      });
      counters.backgroundCompletions += 1;
      return steps;
    }

    case 'notification':
      // Produced by the task_notification envelope the tool_completion emits.
      return [];

    default:
      counters.skipped[row.kind] = (counters.skipped[row.kind] ?? 0) + 1;
      return [];
  }
}

function buildScenario(byTurn, from, to, gapMs, deltaMs) {
  const turns = [];
  const perTurn = [];
  const counters = {
    skipped: {},
    emptyBlocks: 0,
    backgroundLaunches: 0,
    backgroundCompletions: 0,
    fileChangeResults: 0,
    fileChangeFallbacks: 0,
  };

  for (let turn = from; turn <= to; turn += 1) {
    const rows = byTurn.get(turn);
    if (!rows) throw new Error(`live turn ${turn} has no recorded items`);
    const userRow = rows.find((row) => row.kind === 'user_text');
    if (!userRow?.summary?.trim()) {
      throw new Error(`live turn ${turn} has no user text to send`);
    }

    const steps = [];
    let emitted = 0;
    let payloadChars = 0;
    for (const row of rows) {
      const rowSteps = itemSteps(row, rows, deltaMs, counters);
      if (rowSteps.length === 0) continue;
      if (steps.length > 0 && gapMs > 0) steps.push({ delayMs: gapMs });
      steps.push(...rowSteps);
      emitted += 1;
      payloadChars += row.payload?.text.length ?? 0;
    }
    steps.push({ emit: { lines: [resultLine()] } });

    turns.push({ label: `turn-${turn}`, steps });
    perTurn.push({
      turn,
      userText: userRow.summary,
      recordedItems: rows.length,
      emittedItems: emitted,
      payloadChars,
    });
  }

  return {
    scenario: {
      version: 1,
      name: 'freeze-repro',
      description:
        'Replay of a recorded high-churn agentic turn sequence: thinking/text blocks cut at ' +
        'their real payload_chunks boundaries, tool_use + tool_result pairs carrying the real ' +
        'recorded output, and backgrounded Bash launches completed through the ' +
        'task_updated/task_notification pair.',
      provider: 'claude',
      turns,
      // The driver sends exactly len(turns) messages. `silent` makes an extra
      // send hang loudly instead of silently replaying the last turn again.
      afterTurns: 'silent',
    },
    perTurn,
    counters,
  };
}

// -------------------------------------------------------------------- main

const args = parseArgs(process.argv.slice(2));

const [threadRow] = query(
  args.db,
  `SELECT json_object('title', title, 'provider', provider)
   FROM threads WHERE id = '${args.thread}';`,
);
if (!threadRow) throw new Error(`thread ${args.thread} not found in ${args.db}`);
// Claude only, and not a preference: `buildSeed` writes `provider: 'claude'`
// and every line `buildScenario` emits is Claude stream-json. A Codex thread
// would produce a fixture that seeds and replays as Claude while carrying
// Codex content — a repro of nothing.
if (threadRow.provider !== 'claude') {
  throw new Error(
    `thread ${args.thread} is a "${threadRow.provider}" thread; this generator emits Claude `
      + 'stream-json only. Pick a Claude thread, or teach it the Codex wire shape first.',
  );
}

const rows = readThreadItems(args.db, args.thread, args.seedFrom, args.liveTo);
const byTurn = groupByTurn(rows);

const threadTitle = threadRow.title?.trim() || `freeze repro ${args.thread.slice(0, 8)}`;
const seed = buildSeed(byTurn, args.seedFrom, args.seedTo, threadTitle);
const live = buildScenario(byTurn, args.liveFrom, args.liveTo, args.gapMs, args.deltaMs);

// ${VAR} tokens in recorded content WILL be rewritten by the mock's
// substituter, and the generator cannot know whether a literal `${CWD}` in a
// shell command was meant to survive. It used to warn and write the fixture
// anyway — which is the worst of both: the run then reproduces mangled
// content, and the one line saying so scrolls past above a green result.
const collisions = [];
for (const row of rows) {
  const haystack = `${row.summary}\n${row.payload?.text ?? ''}\n${row.inputPayload?.text ?? ''}`;
  for (const token of SUBSTITUTED_TOKENS) {
    if (haystack.includes(`\${${token}}`)) collisions.push({ turn: row.turn, item: row.item, token });
  }
}
if (collisions.length > 0) {
  const where = collisions
    .slice(0, 10)
    .map((hit) => `turn ${hit.turn} item ${hit.item}: \${${hit.token}}`)
    .join('\n  ');
  throw new Error(
    `${collisions.length} recorded string(s) contain a \${VAR} token the mock substitutes:\n  `
      + `${where}${collisions.length > 10 ? '\n  …' : ''}\n`
      + 'The fixture would replay rewritten content. Narrow the turn range past these '
      + 'items, or teach the mock to escape the token, before generating.',
  );
}

const manifest = {
  generatedAt: new Date().toISOString(),
  sourceThread: args.thread,
  sourceDb: args.db,
  threadTitle,
  provider: threadRow.provider,
  seedTurns: [args.seedFrom, args.seedTo],
  liveTurns: [args.liveFrom, args.liveTo],
  pacing: { gapMs: args.gapMs, deltaMs: args.deltaMs, scale: args.scale },
  seed: seed.stats,
  live: live.perTurn,
  wire: live.counters,
  // Fidelity gaps worth knowing when reading a repro result.
  knownGaps: [
    'HarnessSeed writes no input payloads, parent ids, or completion links: seeded ' +
      'tool rows lose their promoted inputs and seeded subagent/completion nesting flattens.',
    'compaction rows are not replayed live (they need a real compact_boundary plus usage).',
    'Rebuilt Edit hunk headers drop any trailing @@ section label.',
    'Triage collapses every non-completed background-task terminal into is_error, so a '
      + 'recorded `killed` task replays as `failed`.',
  ],
};

mkdirSync(args.out, { recursive: true });
writeFileSync(path.join(args.out, 'seed.json'), JSON.stringify(seed.spec), 'utf8');
writeFileSync(path.join(args.out, 'scenario.json'), JSON.stringify(live.scenario), 'utf8');
writeFileSync(path.join(args.out, 'manifest.json'), JSON.stringify(manifest, null, 2), 'utf8');

const liveItems = live.perTurn.reduce((total, turn) => total + turn.emittedItems, 0);
const liveChars = live.perTurn.reduce((total, turn) => total + turn.payloadChars, 0);
process.stderr.write(
  `wrote ${args.out}\n` +
    `  seed:     ${seed.stats.turns} turns, ${seed.stats.items} items, ${seed.stats.payloadChars} chars\n` +
    `  scenario: ${live.perTurn.length} turns, ${liveItems} items, ${liveChars} chars\n` +
    `  pacing:   gap ${args.gapMs}ms, delta ${args.deltaMs}ms\n` +
    `  wire:     ${JSON.stringify(live.counters)}\n` +
    '  NOT COMMITTABLE — real session content; the path is gitignored.\n',
);
