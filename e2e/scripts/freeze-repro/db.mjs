// Read-only reader for the local agent-overflow SQLite store, used by
// scripts/generate-freeze-repro.mjs.
//
// Conventions mirror frontend/scripts/generate-freeze-replay-fixture.mjs:
// reads go through the `sqlite3` CLI in read-only mode (no Node driver to
// install, and a live app holding the WAL open stays untouched), and every
// row comes back as one `json_object(...)` per line so SQLite's own JSON
// writer handles escaping and each row stays on a single line.
//
// Everything this module returns is VERBATIM real session content. Callers
// must only ever write it to a gitignored path.

import { execFileSync } from 'node:child_process';
import { homedir } from 'node:os';
import path from 'node:path';

export const DEFAULT_DB = path.join(
  process.env.XDG_CONFIG_HOME || path.join(homedir(), '.config'),
  'agent-overflow',
  'agent-overflow.db',
);

/**
 * Run one query and parse its `json_object(...)` rows.
 *
 * The database is opened through a `file:...?mode=ro` URI *and* the CLI's
 * own `-readonly` flag. Belt and braces is deliberate: the incident data
 * lives in the developer's live store, and a writable handle against a
 * running app is not a mistake anyone gets to make twice.
 */
export function query(db, sql) {
  let out;
  try {
    out = execFileSync(
      'sqlite3',
      ['-readonly', '-noheader', '-list', `file:${db}?mode=ro`, sql],
      { encoding: 'utf8', maxBuffer: 1024 * 1024 * 1024 },
    );
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

/** Refuse ids that could close a SQL string literal (the CLI takes no binds). */
function assertQuotable(value, what) {
  if (typeof value !== 'string' || value.includes("'")) {
    throw new Error(`${what} contains a quote and cannot be interpolated: ${JSON.stringify(value)}`);
  }
}

/**
 * Reassemble one payload's full text plus the wire boundaries that built it.
 *
 * Boundaries are running JS string lengths, NOT `payload_chunks.start_offset`:
 * the store counts bytes, the generated deltas slice JS strings (UTF-16 code
 * units). The two agree only for ASCII, and a single non-ASCII byte would push
 * every later boundary off the position the renderer actually saw — which is
 * exactly the detail this replay exists to preserve.
 */
function buildPayload(id, heads, chunks) {
  const head = id ? heads.get(id) : undefined;
  if (!head) return null;
  const pieces = [head.text ?? '', ...(chunks.get(id) ?? [])];
  const boundaries = [];
  let at = 0;
  for (const piece of pieces) {
    at += piece.length;
    boundaries.push(at);
  }
  return {
    id,
    kind: head.kind,
    meta: head.meta ?? '{}',
    text: pieces.join(''),
    // Non-empty pieces in wire order. A store append can be empty; a wire
    // delta carrying nothing is noise the renderer never saw.
    pieces: pieces.filter((piece) => piece.length > 0),
    boundaries,
  };
}

/**
 * Load every item of `threadId` in the inclusive turn range, each with its
 * result payload and its tool-input payload fully reassembled.
 *
 * Returns rows in (turn_index, item_index) order.
 */
export function readThreadItems(db, threadId, fromTurn, toTurn) {
  assertQuotable(threadId, 'thread id');
  if (!Number.isInteger(fromTurn) || !Number.isInteger(toTurn) || fromTurn > toTurn) {
    throw new Error(`bad turn range ${fromTurn}-${toTurn}`);
  }

  const itemRows = query(
    db,
    `SELECT json_object(
       'turn', turn_index,
       'item', item_index,
       'kind', kind,
       'role', role,
       'status', status,
       'summary', summary,
       'toolName', tool_name,
       'meta', meta,
       'parentId', parent_id,
       'completionOf', completion_of,
       'isBackground', is_background,
       'payloadId', COALESCE(payload_id, ''),
       'inputPayloadId', COALESCE(input_payload_id, '')
     )
     FROM items
     WHERE thread_id = '${threadId}'
       AND turn_index BETWEEN ${fromTurn} AND ${toTurn}
     ORDER BY turn_index, item_index;`,
  );

  if (itemRows.length === 0) {
    throw new Error(`no items for thread ${threadId} turns ${fromTurn}-${toTurn} in ${db}`);
  }

  const payloadIds = new Set();
  for (const row of itemRows) {
    if (row.payloadId) payloadIds.add(row.payloadId);
    if (row.inputPayloadId) payloadIds.add(row.inputPayloadId);
  }
  for (const id of payloadIds) assertQuotable(id, 'payload id');

  const heads = new Map();
  const chunks = new Map();
  if (payloadIds.size > 0) {
    const idList = [...payloadIds].map((id) => `'${id}'`).join(',');
    for (const row of query(
      db,
      `SELECT json_object('id', id, 'kind', kind, 'meta', meta, 'text', CAST(data AS TEXT))
       FROM payloads WHERE id IN (${idList});`,
    )) {
      heads.set(row.id, row);
    }
    for (const row of query(
      db,
      `SELECT json_object('id', payload_id, 'text', CAST(data AS TEXT))
       FROM payload_chunks WHERE payload_id IN (${idList})
       ORDER BY payload_id, chunk_index;`,
    )) {
      if (!chunks.has(row.id)) chunks.set(row.id, []);
      chunks.get(row.id).push(row.text ?? '');
    }
  }

  return itemRows.map((row) => ({
    turn: row.turn,
    item: row.item,
    kind: row.kind,
    role: row.role,
    status: row.status,
    summary: row.summary ?? '',
    toolName: row.toolName ?? '',
    // items.meta is a TEXT column holding JSON. Parse it: HarnessSeedItem.Meta
    // is json.RawMessage, so a string would seed a JSON-string, not an object.
    meta: parseMeta(row.meta),
    parentId: row.parentId ?? '',
    completionOf: row.completionOf ?? '',
    isBackground: row.isBackground === 1,
    payload: buildPayload(row.payloadId, heads, chunks),
    inputPayload: buildPayload(row.inputPayloadId, heads, chunks),
  }));
}

function parseMeta(raw) {
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {};
  } catch (err) {
    throw new Error(`items.meta is not a JSON object: ${raw.slice(0, 200)} (${err.message})`);
  }
}

/** Group rows by turn_index, preserving item order inside each turn. */
export function groupByTurn(rows) {
  const byTurn = new Map();
  for (const row of rows) {
    if (!byTurn.has(row.turn)) byTurn.set(row.turn, []);
    byTurn.get(row.turn).push(row);
  }
  return byTurn;
}
