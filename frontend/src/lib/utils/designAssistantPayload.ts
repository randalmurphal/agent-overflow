// Parser for the structured payloads the design agent emits inside
// fenced ` ```aoflow-design ` blocks in its assistant text. The system
// prompt (`internal/design/prompts.go`) teaches the model the on-wire
// format; this util is the consumer.
//
// One payload kind is pane-state-bearing:
//   - clarification_request — populates `pane.pendingClarification` so
//     `<DesignClarificationPicker>` renders.
//
// User → agent payloads such as `option_chosen` are sent by UI panels
// and are intentionally not parsed here. Options panel hydration is
// driven by the file-watcher's `design:options-update` event, not by
// the agent emitting an `options_created` payload.

import type { ClarificationRequest } from '../types/design';

/** Discriminated union of payload shapes the parser hands back. */
export type DesignAssistantPayload =
  {
    kind: 'clarification_request';
    payload: ClarificationRequest;
  };

const FENCE_OPEN_PREFIX = '```aoflow-design';
const FENCE_CLOSE = '```';

/**
 * Scan an assistant-text body for `aoflow-design` fenced blocks. Returns
 * a payload entry per validly-classified block, in document order.
 *
 * Tolerant by design: an unparseable JSON body or an unrecognised `kind`
 * is silently dropped (the agent could still be streaming or could have
 * emitted a malformed block — neither is worth surfacing to the user).
 * Unclosed fences during streaming are also dropped — the next call,
 * once the chunk arrives, will see the closed block.
 */
export function parseDesignAssistantPayloads(text: string): DesignAssistantPayload[] {
  if (!text || text.indexOf(FENCE_OPEN_PREFIX) < 0) return [];

  const out: DesignAssistantPayload[] = [];
  let cursor = 0;
  while (cursor < text.length) {
    const open = text.indexOf(FENCE_OPEN_PREFIX, cursor);
    if (open < 0) break;
    // The opening fence may be followed by trailing whitespace before
    // the newline; we want to start the body at the next newline.
    const lineEnd = text.indexOf('\n', open + FENCE_OPEN_PREFIX.length);
    if (lineEnd < 0) break;
    const close = text.indexOf(FENCE_CLOSE, lineEnd + 1);
    if (close < 0) break; // unclosed — likely streaming
    const json = text.slice(lineEnd + 1, close).trim();
    cursor = close + FENCE_CLOSE.length;

    let parsed: unknown;
    try {
      parsed = JSON.parse(json);
    } catch {
      continue;
    }
    const classified = classifyPayload(parsed);
    if (classified) out.push(classified);
  }
  return out;
}

function classifyPayload(obj: unknown): DesignAssistantPayload | null {
  if (!obj || typeof obj !== 'object') return null;
  const record = obj as Record<string, unknown>;
  const kind = record.kind;
  if (typeof kind !== 'string') return null;

  switch (kind) {
    case 'clarification_request':
      return parseClarification(record);
    default:
      return null;
  }
}

function parseClarification(record: Record<string, unknown>): DesignAssistantPayload | null {
  const requestId = typeof record.requestId === 'string' ? record.requestId : '';
  const intro = typeof record.intro === 'string' ? record.intro : undefined;
  const threadId = typeof record.threadId === 'string' ? record.threadId : '';
  const rawQuestions = record.questions;
  if (!Array.isArray(rawQuestions) || rawQuestions.length === 0) return null;

  const questions: ClarificationRequest['questions'] = [];
  for (const raw of rawQuestions) {
    if (!raw || typeof raw !== 'object') return null;
    const q = raw as Record<string, unknown>;
    const id = typeof q.id === 'string' ? q.id : '';
    const prompt = typeof q.prompt === 'string' ? q.prompt : '';
    if (!id || !prompt) return null;

    const rawChoices = q.choices;
    if (!Array.isArray(rawChoices) || rawChoices.length === 0) return null;
    const choices: ClarificationRequest['questions'][number]['choices'] = [];
    for (const rc of rawChoices) {
      if (!rc || typeof rc !== 'object') return null;
      const choice = rc as Record<string, unknown>;
      const cid = typeof choice.id === 'string' ? choice.id : '';
      const label = typeof choice.label === 'string' ? choice.label : '';
      if (!cid || !label) return null;
      choices.push({ id: cid, label });
    }

    const multiple = typeof q.multiple === 'boolean' ? q.multiple : undefined;
    const question: ClarificationRequest['questions'][number] = {
      id,
      prompt,
      choices,
    };
    if (multiple !== undefined) question.multiple = multiple;
    questions.push(question);
  }

  // requestId is required by the picker for state isolation across
  // successive requests; if the agent omitted one, synthesize a stable
  // hash of the questions so re-renders stay idempotent.
  const finalRequestId = requestId || synthesizeRequestId(questions);

  const result: ClarificationRequest = {
    requestId: finalRequestId,
    threadId,
    questions,
  };
  if (intro !== undefined) result.intro = intro;

  return { kind: 'clarification_request', payload: result };
}

// synthesizeRequestId makes the picker idempotent when the agent
// emits a clarification block without a requestId — same questions
// produce the same id, so re-upserting the same item doesn't reset
// the user's in-progress selections.
function synthesizeRequestId(questions: ClarificationRequest['questions']): string {
  const key = questions
    .map((q) => `${q.id}:${q.choices.map((c) => c.id).join('|')}`)
    .join(';');
  // Tiny FNV-1a 32-bit hash, stable across runtimes; we don't need
  // crypto strength, just a deterministic short id.
  let h = 0x811c9dc5;
  for (let i = 0; i < key.length; i++) {
    h ^= key.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return `synth-${(h >>> 0).toString(16)}`;
}
