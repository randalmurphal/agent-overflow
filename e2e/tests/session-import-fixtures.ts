// Hand-written provider homes for the session-import spec.
//
// The harness already isolates HOME: boot points `$HOME`/`%USERPROFILE%` AND
// `App.credentialHomeOverride` at `<dataRoot>/home` (main_harness.go
// `isolateHarnessHome`), and session import resolves its provider homes
// through exactly that override (`app_session_import.go#sessionImportDeps`).
// So "seed a provider home" is nothing more than writing files under
// `harness.bootstrap.homeDir` — the developer's real `~/.claude` / `~/.codex`
// are unreachable from a harness process by construction, and nothing here
// widens that.
//
// The rows are the minimum each reader needs, copied in SHAPE (not by import
// — this is a test-only file and production must never link one) from the Go
// fixture builders in `app_session_import_fixture_test.go` and
// `internal/provider/codex/rollout/list_test.go`. The Codex thread index is
// built with `node:sqlite` in the same schema subset the Go fixtures use.
//
// The workspace every session claims to have run in is a real git repository
// seeded through `HarnessSeed` (the harness's own project path), so an
// imported thread opens against a real repo and `HarnessReset` removes the
// tree between tests. Project auto-creation from an unknown workspace is
// covered by the Go tests, not here.

import { DatabaseSync } from 'node:sqlite';
import { appendFile, mkdir, rm, writeFile } from 'node:fs/promises';
import * as path from 'node:path';
import type { HarnessApp } from '../src/harness.js';

/** Session ids are UUID-shaped because Claude's lister admits nothing else. */
const CLAUDE_LINEAR_SESSION = 'aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa';
const CLAUDE_BRANCHED_SESSION = 'bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb';
const CLAUDE_BROKEN_SESSION = 'dddddddd-4444-4444-8444-dddddddddddd';
const CLAUDE_FORK_SESSION = 'eeeeeeee-5555-4555-8555-eeeeeeeeeeee';
const CODEX_THREAD = 'cccccccc-3333-4333-8333-cccccccccccc';

/** A fixed wall time in the past, so a row restamped with now() is off by
 *  years rather than milliseconds. */
const BASE_MILLIS = 1_700_000_000_000;

/** Claude's projects directory names one directory per workspace slug; the
 *  lister does not read the name, so a fixed one is fine. */
const CLAUDE_PROJECT_SLUG = '-import-fixture-repo';

/**
 * The branched transcript's catalogue title, written as a `custom-title`
 * record — which beats every other source in the lister's title chain. It has
 * to differ from both leaves' last prompts, because a branch whose own prompt
 * equals the session title falls back to an ordinal ("— branch 2") instead.
 */
const BRANCHED_TITLE = 'Parser work';

// Both divergent branches open their second turn with one thinking block, so
// the importer deliberately gives them the same logical item/payload ids
// (`think:2:0` / `thinking:think:2:0`). The bodies must remain thread-owned
// all the way through SQLite, the transport, and the frontend payload cache.
// Keep the identifying prefix outside the 400-rune summary tail: seeing it in
// the browser proves the disclosure loaded the full payload on demand.
export const BRANCH_A_THINKING_PREFIX = 'BRANCH A PRIVATE REASONING PREFIX';
export const BRANCH_B_THINKING_PREFIX = 'BRANCH B PRIVATE REASONING PREFIX';
export const FORK_SHARED_ANSWER = 'Added the retry helper with a backoff test.';
export const FORK_CHILD_PROMPT = 'Adapt the retry helper for jitter';
export const FORK_CHILD_ANSWER = 'Added bounded jitter to the imported fork.';

const branchThinking = (prefix: string, detail: string): string =>
  `${prefix}\n${`${detail} `.repeat(32)}\n${detail} conclusion.`;

export interface FixtureSession {
  /** The opaque scan row id (`<provider>:<session id>`). */
  rowId: string;
  /** `data-testid` of the row in the import modal. */
  rowTestId: string;
  /** Title the catalogue row shows. */
  title: string;
}

export interface ImportFixtures {
  /** `<dataRoot>/home` — the harness's isolated HOME. */
  home: string;
  /** The seeded git repository every fixture session records as its cwd. */
  workspace: string;
  claudeLinear: FixtureSession;
  claudeBranched: FixtureSession;
  /** Present only when `seedImportFixtures` was asked for it. */
  claudeBroken?: FixtureSession;
  /** Present only when `seedImportFixtures` was asked for an explicit fork. */
  claudeFork?: FixtureSession;
  codex: FixtureSession;
}

export interface SeedImportOptions {
  /**
   * Also write a transcript that LISTS cleanly but fails at import: its
   * `tool_result` names a `tool_use` that is nowhere in the file, which the
   * writer refuses (a completion with no launch and no `import_unavailable`
   * marker). It is the only deterministic way to hold the modal open after a
   * run — a clean run closes itself — so the progress strip, the per-row
   * outcome stamps and the Retry CTA can be asserted without a race.
   */
  withFailingSession?: boolean;
  /** Also write a second Claude provider session explicitly forked from the
   * linear session, including the shared prefix and its own continuation. */
  withForkedSession?: boolean;
}

function session(provider: 'claude' | 'codex', id: string, title: string): FixtureSession {
  const rowId = `${provider}:${id}`;
  return { rowId, rowTestId: `session-import-row-${rowId}`, title };
}

const iso = (offset: number): string => new Date(BASE_MILLIS + offset).toISOString();

const jsonl = (rows: unknown[]): string => rows.map((row) => JSON.stringify(row)).join('\n') + '\n';

/**
 * Seed both provider homes from scratch and return what was written.
 *
 * Every call starts from a blank slate: the fixture trees are removed first,
 * so a test never inherits the previous one's files. Only the paths this
 * module writes are removed — `~/.codex` also holds the harness's credential
 * state, which is not ours to delete.
 */
export async function seedImportFixtures(
  harness: HarnessApp,
  opts: SeedImportOptions = {},
): Promise<ImportFixtures> {
  const home = harness.bootstrap.homeDir;
  if (!home) throw new Error('harness bootstrap carries no homeDir; cannot seed provider homes');

  const seed = await harness.rpc<{ projects: Array<{ path: string }> }>('HarnessSeed', {
    projects: [
      {
        name: 'import-fixture-repo',
        repo: { commits: [{ message: 'init', files: { 'README.md': '# Import fixture\n' } }] },
      },
    ],
  });
  const workspace = seed.projects[0].path;

  const claudeProjects = path.join(home, '.claude', 'projects');
  const codexHome = path.join(home, '.codex');
  await rm(claudeProjects, { recursive: true, force: true });
  await rm(path.join(codexHome, 'sessions'), { recursive: true, force: true });
  await rm(path.join(codexHome, 'state_5.sqlite'), { force: true });

  const claudeDir = path.join(claudeProjects, CLAUDE_PROJECT_SLUG);
  await mkdir(claudeDir, { recursive: true });

  const fixtures: ImportFixtures = {
    home,
    workspace,
    claudeLinear: session('claude', CLAUDE_LINEAR_SESSION, 'Add the retry helper'),
    claudeBranched: session('claude', CLAUDE_BRANCHED_SESSION, BRANCHED_TITLE),
    codex: session('codex', CODEX_THREAD, 'Wire up the Codex client'),
  };

  await writeLinearClaudeSession(claudeDir, workspace);
  await writeBranchedClaudeSession(claudeDir, workspace);
  if (opts.withForkedSession) {
    await writeForkedClaudeSession(claudeDir, workspace);
    fixtures.claudeFork = session('claude', CLAUDE_FORK_SESSION, FORK_CHILD_PROMPT);
  }
  if (opts.withFailingSession) {
    await writeBrokenClaudeSession(claudeDir, workspace);
    fixtures.claudeBroken = session('claude', CLAUDE_BROKEN_SESSION, 'Refuse to import');
  }
  await writeCodexSession(codexHome, workspace, fixtures.codex.title);

  return fixtures;
}

// ------------------------------------------------------------------ Claude

const userRow = (
  workspace: string,
  uuid: string,
  parent: string | null,
  text: string,
  offset: number,
): Record<string, unknown> => ({
  type: 'user',
  uuid,
  parentUuid: parent,
  isSidechain: false,
  timestamp: iso(offset),
  cwd: workspace,
  gitBranch: 'main',
  message: { role: 'user', content: text },
});

const assistantTextRow = (
  workspace: string,
  uuid: string,
  parent: string,
  messageId: string,
  text: string,
  offset: number,
): Record<string, unknown> => ({
  type: 'assistant',
  uuid,
  parentUuid: parent,
  isSidechain: false,
  timestamp: iso(offset),
  cwd: workspace,
  message: {
    role: 'assistant',
    id: messageId,
    model: 'claude-sonnet-4-5',
    content: [{ type: 'text', text }],
    usage: { input_tokens: 120, output_tokens: 24 },
  },
});

const assistantThinkingRow = (
  workspace: string,
  uuid: string,
  parent: string,
  messageId: string,
  thinking: string,
  offset: number,
): Record<string, unknown> => ({
  type: 'assistant',
  uuid,
  parentUuid: parent,
  isSidechain: false,
  timestamp: iso(offset),
  cwd: workspace,
  message: {
    role: 'assistant',
    id: messageId,
    model: 'claude-sonnet-4-5',
    content: [{ type: 'thinking', thinking, signature: `sig-${uuid}` }],
    usage: { input_tokens: 90, output_tokens: 12 },
  },
});

const assistantToolRow = (
  workspace: string,
  uuid: string,
  parent: string,
  messageId: string,
  toolUseId: string,
  name: string,
  input: Record<string, unknown>,
  offset: number,
): Record<string, unknown> => ({
  type: 'assistant',
  uuid,
  parentUuid: parent,
  isSidechain: false,
  timestamp: iso(offset),
  cwd: workspace,
  message: {
    role: 'assistant',
    id: messageId,
    model: 'claude-sonnet-4-5',
    content: [{ type: 'tool_use', id: toolUseId, name, input }],
    usage: { input_tokens: 90, output_tokens: 12 },
  },
});

const toolResultRow = (
  workspace: string,
  uuid: string,
  parent: string,
  toolUseId: string,
  content: string,
  offset: number,
  toolUseResult?: Record<string, unknown>,
): Record<string, unknown> => ({
  type: 'user',
  uuid,
  parentUuid: parent,
  isSidechain: false,
  timestamp: iso(offset),
  cwd: workspace,
  message: {
    role: 'user',
    content: [{ type: 'tool_result', tool_use_id: toolUseId, content }],
  },
  ...(toolUseResult ? { toolUseResult } : {}),
});

/** The per-leaf title record the CLI appends; it is what names each branch. */
const lastPrompt = (leafUuid: string, prompt: string): Record<string, unknown> => ({
  type: 'last-prompt',
  leafUuid,
  lastPrompt: prompt,
});

/** One prompt, one answer, one tool call — the smallest session that still
 *  renders a user row, an assistant row and a tool card once imported. */
async function writeLinearClaudeSession(dir: string, workspace: string): Promise<void> {
  await writeFile(
    path.join(dir, `${CLAUDE_LINEAR_SESSION}.jsonl`),
    jsonl(linearClaudeRows(workspace)),
  );
}

const linearClaudeRows = (workspace: string): Array<Record<string, unknown>> => [
  userRow(workspace, 'lin-u1', null, 'Add the retry helper', 0),
  assistantTextRow(workspace, 'lin-a1', 'lin-u1', 'msg-lin-1', 'Running the suite first.', 1_000),
  assistantToolRow(
    workspace,
    'lin-a2',
    'lin-a1',
    'msg-lin-2',
    'toolu_lin_bash',
    'Bash',
    { command: 'go test ./internal/retry', description: 'Run the retry tests' },
    2_000,
  ),
  toolResultRow(workspace, 'lin-r1', 'lin-a2', 'toolu_lin_bash', 'ok  internal/retry 0.02s', 3_000),
  assistantTextRow(
    workspace,
    'lin-a3',
    'lin-r1',
    'msg-lin-3',
    FORK_SHARED_ANSWER,
    4_000,
  ),
  lastPrompt('lin-a3', 'Add the retry helper'),
];

async function writeForkedClaudeSession(dir: string, workspace: string): Promise<void> {
  const shared = linearClaudeRows(workspace);
  shared[0] = {
    ...shared[0],
    forkedFrom: { sessionId: CLAUDE_LINEAR_SESSION, messageUuid: 'lin-a3' },
  };
  await writeFile(
    path.join(dir, `${CLAUDE_FORK_SESSION}.jsonl`),
    jsonl([
      ...shared,
      userRow(workspace, 'fork-u2', 'lin-a3', FORK_CHILD_PROMPT, 5_000),
      assistantTextRow(
        workspace,
        'fork-a2',
        'fork-u2',
        'msg-fork-2',
        FORK_CHILD_ANSWER,
        6_000,
      ),
      lastPrompt('fork-a2', FORK_CHILD_PROMPT),
    ]),
  );
}

/**
 * Two leaves off one answer, the second of which runs a subagent.
 *
 * Importing it selects the second, file-order-last leaf as the active history.
 * The catalogue row's title is the LAST `last-prompt` record (the tail wins).
 */
async function writeBranchedClaudeSession(dir: string, workspace: string): Promise<void> {
  await writeFile(
    path.join(dir, `${CLAUDE_BRANCHED_SESSION}.jsonl`),
    jsonl([
      userRow(workspace, 'br-u1', null, 'Parse the config file', 0),
      assistantTextRow(workspace, 'br-a1', 'br-u1', 'msg-br-1', 'Parsed it.', 1_000),
      userRow(workspace, 'br-u2a', 'br-a1', 'Document the parser', 2_000),
      assistantThinkingRow(
        workspace,
        'br-t2a',
        'br-u2a',
        'msg-br-think-a',
        branchThinking(BRANCH_A_THINKING_PREFIX, 'Document branch reasoning'),
        2_500,
      ),
      assistantTextRow(workspace, 'br-a2a', 'br-t2a', 'msg-br-2a', 'Documented the parser.', 3_000),
      userRow(workspace, 'br-u2b', 'br-a1', 'Benchmark the parser', 4_000),
      assistantThinkingRow(
        workspace,
        'br-t2b',
        'br-u2b',
        'msg-br-think-b',
        branchThinking(BRANCH_B_THINKING_PREFIX, 'Benchmark branch reasoning'),
        4_500,
      ),
      assistantToolRow(
        workspace,
        'br-a2b',
        'br-t2b',
        'msg-br-2b',
        'toolu_br_task',
        'Task',
        { description: 'Benchmark the parser', prompt: 'Benchmark parseConfig and report ns/op' },
        5_000,
      ),
      toolResultRow(
        workspace,
        'br-r2b',
        'br-a2b',
        'toolu_br_task',
        'Benchmark complete.',
        6_000,
        { agentId: 'benchmarker' },
      ),
      assistantTextRow(
        workspace,
        'br-a2c',
        'br-r2b',
        'msg-br-2c',
        'Benchmarked the parser at 120ns/op.',
        7_000,
      ),
      lastPrompt('br-a2a', 'Document the parser'),
      lastPrompt('br-a2c', 'Benchmark the parser'),
      { type: 'custom-title', customTitle: BRANCHED_TITLE },
    ]),
  );

  // `<sessionDir>/subagents/agent-<agentId>.jsonl` — joined onto the Task
  // launch by the `toolUseResult.agentId` above. Its own opening prompt is
  // the Task tool's input and is skipped by the converter.
  const subagentDir = path.join(dir, CLAUDE_BRANCHED_SESSION, 'subagents');
  await mkdir(subagentDir, { recursive: true });
  await writeFile(
    path.join(subagentDir, 'agent-benchmarker.jsonl'),
    jsonl([
      {
        ...userRow(workspace, 'sub-u1', null, 'Benchmark parseConfig and report ns/op', 5_100),
        isSidechain: true,
      },
      {
        ...assistantTextRow(
          workspace,
          'sub-a1',
          'sub-u1',
          'msg-sub-1',
          'parseConfig runs at 120ns/op.',
          5_200,
        ),
        isSidechain: true,
      },
    ]),
  );
}

/**
 * Lists cleanly, refuses at import. The `tool_result` names a `tool_use` that
 * appears nowhere in the file, which the import writer treats as structurally
 * broken input and refuses for the whole session. See
 * `SeedImportOptions.withFailingSession` for why the spec wants one.
 */
async function writeBrokenClaudeSession(dir: string, workspace: string): Promise<void> {
  await writeFile(
    path.join(dir, `${CLAUDE_BROKEN_SESSION}.jsonl`),
    jsonl([
      userRow(workspace, 'bad-u1', null, 'Refuse to import', 0),
      toolResultRow(workspace, 'bad-r1', 'bad-u1', 'toolu_never_launched', 'orphan result', 1_000),
      lastPrompt('bad-r1', 'Refuse to import'),
    ]),
  );
}

/**
 * Append a second exchange to the linear transcript, chained onto the leaf the
 * first import stopped at. This is what "the provider session grew since the
 * import" looks like on disk.
 */
export async function growLinearClaudeSession(fx: ImportFixtures): Promise<{
  prompt: string;
  answer: string;
}> {
  const prompt = 'Now add jittered backoff';
  const answer = 'Added jittered backoff to the retry helper.';
  const file = path.join(
    fx.home,
    '.claude',
    'projects',
    CLAUDE_PROJECT_SLUG,
    `${CLAUDE_LINEAR_SESSION}.jsonl`,
  );
  await appendFile(
    file,
    jsonl([
      userRow(fx.workspace, 'lin-u2', 'lin-a3', prompt, 60_000),
      assistantTextRow(fx.workspace, 'lin-a4', 'lin-u2', 'msg-lin-4', answer, 61_000),
      lastPrompt('lin-a4', prompt),
    ]),
  );
  return { prompt, answer };
}

// ------------------------------------------------------------------- Codex

/** The subset of Codex's `threads` table the lister reads, with upstream's own
 *  column names (mirrors `codexFixtureSchema` in the Go fixtures). */
const CODEX_SCHEMA = `
CREATE TABLE threads (
    id TEXT PRIMARY KEY,
    rollout_path TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT NOT NULL,
    cwd TEXT NOT NULL,
    title TEXT NOT NULL,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    git_branch TEXT,
    first_user_message TEXT NOT NULL DEFAULT '',
    model TEXT,
    reasoning_effort TEXT,
    created_at_ms INTEGER,
    updated_at_ms INTEGER,
    thread_source TEXT,
    preview TEXT NOT NULL DEFAULT '',
    recency_at_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE thread_spawn_edges (
    parent_thread_id TEXT NOT NULL,
    child_thread_id TEXT NOT NULL PRIMARY KEY,
    status TEXT NOT NULL
);`;

const codexLine = (offset: number, kind: string, payload: Record<string, unknown>): unknown => ({
  timestamp: iso(offset),
  type: kind,
  payload,
});

export const CODEX_PROMPT = 'Wire up the Codex client';
export const CODEX_ANSWER = 'Wired the Codex client to the app-server.';

async function writeCodexSession(
  codexHome: string,
  workspace: string,
  title: string,
): Promise<void> {
  const rolloutPath = path.join(
    codexHome,
    'sessions',
    `rollout-2026-08-07T15-07-44-${CODEX_THREAD}.jsonl`,
  );
  await mkdir(path.dirname(rolloutPath), { recursive: true });
  await writeFile(
    rolloutPath,
    jsonl([
      codexLine(0, 'session_meta', {
        id: CODEX_THREAD,
        cwd: workspace,
        originator: 'codex_cli',
        cli_version: '0.146.0',
        git: { branch: 'main' },
      }),
      codexLine(100, 'turn_context', { turn_id: 'turn-1', model: 'gpt-5.6-sol', effort: 'high' }),
      codexLine(110, 'event_msg', {
        type: 'task_started',
        turn_id: 'turn-1',
        model_context_window: 258_400,
      }),
      codexLine(120, 'event_msg', { type: 'user_message', message: CODEX_PROMPT }),
      codexLine(130, 'event_msg', {
        type: 'agent_message',
        message: CODEX_ANSWER,
        phase: 'final_answer',
      }),
      codexLine(140, 'event_msg', {
        type: 'task_complete',
        turn_id: 'turn-1',
        last_agent_message: CODEX_ANSWER,
      }),
    ]),
  );

  const db = new DatabaseSync(path.join(codexHome, 'state_5.sqlite'));
  try {
    // The reader opens the index with `immutable=1`, which skips -wal/-shm
    // entirely — everything has to be in the main database file for it to be
    // visible, so this fixture never runs in WAL mode.
    db.exec('PRAGMA journal_mode = DELETE');
    db.exec(CODEX_SCHEMA);
    db.prepare(
      `INSERT INTO threads (id, rollout_path, created_at, updated_at, source, cwd, title,
                            first_user_message, archived, thread_source, preview, recency_at_ms,
                            created_at_ms, updated_at_ms, git_branch, model, reasoning_effort, tokens_used)
       VALUES (?, ?, ?, ?, '{}', ?, ?, ?, 0, NULL, 'preview', ?, ?, ?, 'main', 'gpt-5.6-sol', 'high', 42)`,
    ).run(
      CODEX_THREAD,
      rolloutPath,
      Math.trunc(BASE_MILLIS / 1000),
      Math.trunc((BASE_MILLIS + 9_000) / 1000),
      workspace,
      title,
      CODEX_PROMPT,
      BASE_MILLIS + 9_000,
      BASE_MILLIS,
      BASE_MILLIS + 9_000,
    );
  } finally {
    db.close();
  }
}
