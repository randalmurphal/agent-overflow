import type { Item } from '../types/models';
import { extractClaudeTaskID } from './claudeTaskMeta';
import { RAIL_EXEMPT_PAYLOAD_KINDS } from './timelineRail';

// Whether an in-place row replacement changes timeline STRUCTURE — the
// grouping pipeline's inputs — and must bump `timelineRevision`. The
// components below mirror the projection passes that consume them:
// subagent grouping (parent/completion links, the tool metas it groups
// by), read grouping (kind/tool/background), the notification filter
// (Claude task identity + a tool call's completed flag), receiver labels
// (collab_agent metas), structural meta/payload (completion rows), and
// rail membership (the exempt-or-not BIT of payloadKind — a payload
// attaching mid-stream must not read as structure; leaving the rail
// happens at most once, when a card-style payload lands).
//
// Written as field-wise comparisons with early exit, NOT as two built key
// strings: this runs per streamed upsert, and building two joined keys —
// each copying `meta`/`payloadMeta` — was 15MB/30s of string garbage
// during two-pane streaming (2026-08-25 alloc profile). The conditional
// reads are gated the same way the old fingerprints gated them, so the
// predicate's sensitivity is unchanged: a meta change on a row no
// fingerprint read never bumps revision. `extractClaudeTaskID` JSON-parses
// `meta`, so it runs only when the raw `meta` strings differ (equal metas
// cannot change the extracted id).
//
// timelineStructure.test.ts pins equivalence against the retired
// key-builder as its oracle.
export function itemTimelineStructureChanged(previous: Item | undefined, next: Item): boolean {
  if (previous === undefined) return true;
  if (
    previous.id !== next.id
    || previous.threadId !== next.threadId
    || previous.turnIndex !== next.turnIndex
    || previous.itemIndex !== next.itemIndex
    || previous.kind !== next.kind
    || (previous.parentId ?? '') !== (next.parentId ?? '')
    || (previous.completionOf ?? '') !== (next.completionOf ?? '')
    || (previous.toolName ?? '') !== (next.toolName ?? '')
    || (previous.isBackground === true) !== (next.isBackground === true)
  ) {
    return true;
  }
  // From here the two rows share kind, toolName, and completionOf
  // presence, so every conditional below gates both sides identically.
  const kind = next.kind;
  const metaChanged = (previous.meta ?? '') !== (next.meta ?? '');

  // Subagent grouping + receiver labels: the metas those passes read.
  if (kind === 'terminal_interaction' && metaChanged) return true;
  if (kind === 'tool_call') {
    const tool = next.toolName ?? '';
    if (
      metaChanged
      && (tool === 'wait_agent' || tool === 'Agent' || tool === 'Task' || tool === 'collab_agent')
    ) {
      return true;
    }
    if (
      tool === 'collab_agent'
      && (previous.payloadMeta ?? '') !== (next.payloadMeta ?? '')
    ) {
      return true;
    }
  }

  // Notification filter: Claude task identity, plus a tool call's
  // completed flag once it carries a task id.
  if (kind === 'notification' || kind === 'tool_completion') {
    if (
      metaChanged
      && (extractClaudeTaskID(previous) ?? '') !== (extractClaudeTaskID(next) ?? '')
    ) {
      return true;
    }
  } else if (kind === 'tool_call') {
    const completedFlagChanged =
      (previous.status === 'completed') !== (next.status === 'completed');
    if (metaChanged || completedFlagChanged) {
      const previousTask = extractClaudeTaskID(previous) ?? '';
      const nextTask = extractClaudeTaskID(next) ?? '';
      if (previousTask !== nextTask) return true;
      if (nextTask !== '' && completedFlagChanged) return true;
    }
  }

  // Structural meta: notification/completion rows and completion-linked
  // rows render their meta as structure.
  if (
    metaChanged
    && (kind === 'notification' || kind === 'tool_completion' || Boolean(next.completionOf))
  ) {
    return true;
  }

  // Structural payload id: a completion's payload landing.
  if (kind === 'tool_completion' && (previous.payloadId ?? '') !== (next.payloadId ?? '')) {
    return true;
  }

  // Rail membership: only the exempt bit.
  return (
    RAIL_EXEMPT_PAYLOAD_KINDS.has(previous.payloadKind ?? '')
    !== RAIL_EXEMPT_PAYLOAD_KINDS.has(next.payloadKind ?? '')
  );
}
