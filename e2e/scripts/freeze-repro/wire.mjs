// Claude stream-json wire construction for the freeze-repro scenario.
//
// Every line this module emits has to survive `internal/provider/claude`'s
// real parser and the scenario framing invariants enforced by
// `internal/harness/scenario/library_test.go`:
//
//   1. No scenario line may be a `system/init` envelope — the mock's Claude
//      adapter owns that frame (one per user turn) and the user-envelope echo.
//   2. An `assistant` envelope carrying text or thinking needs a preceding
//      `stream_event message_start` registering the SAME message id, or the
//      app renders the content twice instead of deduping the coalesced copy.
//
// One recorded item maps to one message id, so those two rules hold by
// construction here.

const THINKING = 'thinking';
const TEXT = 'assistant_text';

const json = (value) => JSON.stringify(value);

const streamEvent = (event, data) => json({ type: 'stream_event', event, data: { type: event, ...data } });

/**
 * The stream_event + coalesced-envelope sequence for one streamed content
 * block (thinking or assistant text).
 *
 * `pieces` are the recorded `payload_chunks` boundaries — the actual wire
 * chunking the renderer saw — not a synthetic split.
 */
export function streamedBlockLines({ messageId, kind, pieces, fullText }) {
  const isThinking = kind === THINKING;
  const blockStart = isThinking ? { type: 'thinking', thinking: '' } : { type: 'text', text: '' };
  const delta = (piece) =>
    isThinking
      ? { type: 'thinking_delta', thinking: piece }
      : { type: 'text_delta', text: piece };

  return [
    streamEvent('message_start', { message: { id: messageId, role: 'assistant' } }),
    streamEvent('content_block_start', { index: 0, content_block: blockStart }),
    ...pieces.map((piece) => streamEvent('content_block_delta', { delta: delta(piece) })),
    streamEvent('content_block_stop', { index: 0 }),
    streamEvent('message_stop', {}),
    json({
      type: 'assistant',
      message: {
        id: messageId,
        role: 'assistant',
        content: [isThinking ? { type: 'thinking', thinking: fullText } : { type: 'text', text: fullText }],
      },
    }),
  ];
}

/** The `assistant` envelope carrying a single tool_use block. */
export function toolUseLine({ messageId, toolUseId, toolName, input }) {
  return json({
    type: 'assistant',
    message: {
      id: messageId,
      role: 'assistant',
      content: [{ type: 'tool_use', id: toolUseId, name: toolName, input }],
    },
  });
}

/**
 * The `user` envelope echoing a tool_result.
 *
 * `toolUseResult` rides at the envelope ROOT (not inside the block) — that is
 * where `parse_user.go` reads it from, and it is what lets Write/Edit results
 * rebuild their file_change diff payload instead of landing as flat text.
 */
export function toolResultLine({ toolUseId, content, isError, toolUseResult }) {
  const envelope = {
    type: 'user',
    message: {
      role: 'user',
      content: [{ type: 'tool_result', tool_use_id: toolUseId, content, is_error: Boolean(isError) }],
    },
  };
  if (toolUseResult) envelope.tool_use_result = toolUseResult;
  return json(envelope);
}

/** `system/task_started` — registers the task_id ↔ tool_use_id mapping. */
export function taskStartedLine({ taskId, toolUseId, taskType }) {
  return json({
    type: 'system',
    subtype: 'task_started',
    task_id: taskId,
    tool_use_id: toolUseId,
    task_type: taskType || 'local_bash',
  });
}

/** `system/task_updated` with a terminal patch — the background completion. */
export function taskUpdatedLine({ taskId, toolUseId, status, description }) {
  return json({
    type: 'system',
    subtype: 'task_updated',
    task_id: taskId,
    tool_use_id: toolUseId,
    patch: { status: status || 'completed', description: description || '' },
  });
}

/**
 * `system/task_notification` — the attention signal, never a completion
 * source. `outputFile` must be a path the backend can read: the scenario
 * writes it into the mock's workspace first and passes `${CWD}/<rel>`.
 */
export function taskNotificationLine({ taskId, toolUseId, status, outputFile, summary }) {
  return json({
    type: 'system',
    subtype: 'task_notification',
    task_id: taskId,
    tool_use_id: toolUseId,
    status: status || 'completed',
    output_file: outputFile,
    summary: summary || '',
  });
}

/** The turn-terminal `result` envelope. */
export function resultLine() {
  return json({ type: 'result', subtype: 'success', is_error: false });
}

const HUNK_HEADER = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

/**
 * Rebuild a Claude `structuredPatch` array from a stored unified diff.
 *
 * The stored payload for an Edit/Write-update is precisely what
 * `triage.buildUnifiedPatch` assembled from that array, so feeding the hunks
 * back reproduces the same bytes. The one lossy detail is a hunk header's
 * trailing section label (`@@ … @@ func foo()`), which the wire shape has no
 * field for and the rebuild therefore drops.
 */
export function structuredPatchFromUnifiedDiff(patch) {
  const hunks = [];
  let current = null;
  for (const line of patch.split('\n')) {
    const match = HUNK_HEADER.exec(line);
    if (match) {
      current = {
        oldStart: Number(match[1]),
        oldLines: match[2] === undefined ? 1 : Number(match[2]),
        newStart: Number(match[3]),
        newLines: match[4] === undefined ? 1 : Number(match[4]),
        lines: [],
      };
      hunks.push(current);
      continue;
    }
    if (current) current.lines.push(line);
  }
  return hunks;
}

/**
 * Build the `tool_use_result` sibling for a file-editing tool whose recorded
 * payload is a synthesized diff rather than raw tool output.
 *
 * Returns null when the recorded shape cannot be rebuilt faithfully; the
 * caller then falls back to a flat-text tool_result, which keeps the row and
 * its bytes but renders as generic output instead of a diff.
 */
export function fileChangeToolUseResult({ toolName, filePath, storedPatch, writtenContent }) {
  if (!filePath || !storedPatch) return null;

  const isCreate = /^new file mode \d+$/m.test(storedPatch);
  if (toolName === 'Write' && isCreate) {
    if (typeof writtenContent !== 'string') return null;
    return { type: 'create', filePath, content: writtenContent, structuredPatch: [] };
  }

  const structuredPatch = structuredPatchFromUnifiedDiff(storedPatch);
  if (structuredPatch.length === 0) return null;
  return { type: 'update', filePath, structuredPatch };
}

export const ITEM_KINDS = { THINKING, TEXT };
