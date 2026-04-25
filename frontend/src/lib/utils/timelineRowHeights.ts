import type { SettledTurn } from '../stores/thread.svelte';
import type { Item } from '../types/models';
import type { TurnDiffView } from './turnDiffSummary';
import type { TimelineNode } from './subagentGrouping';

export const DEFAULT_TIMELINE_ROW_HEIGHT = 140;

export function rootTurnIndex(node: TimelineNode): number {
  return node.kind === 'leaf' ? node.item.turnIndex : node.parent.turnIndex;
}

export function isLastRootInTurn(index: number, nodes: TimelineNode[]): boolean {
  const current = nodes[index];
  const next = nodes[index + 1];
  if (!current) return false;
  if (!next) return true;
  return rootTurnIndex(current) !== rootTurnIndex(next);
}

export function estimateTimelineNodeHeight(node: TimelineNode): number {
  if (node.kind === 'group') {
    return Math.min(320, 132 + node.children.length * 44);
  }

  const item = node.item;
  switch (item.kind) {
    case 'terminal_interaction':
      return item.payloadKind === 'command_output' ? 72 : 32;
    case 'tool_call':
    case 'tool_completion':
      return 116;
    case 'thinking':
      return 96;
    case 'user_text':
      return 84;
    case 'assistant_text':
      return 120;
    case 'error':
      return 84;
    case 'notification':
    case 'compaction':
      return 56;
    default:
      return DEFAULT_TIMELINE_ROW_HEIGHT;
  }
}

export function timelineNodeHeightSignature(
  node: TimelineNode,
  index: number,
  nodes: TimelineNode[],
  showEndOfTurnDiffs: boolean,
  latestSettledTurn: SettledTurn | null,
  diffViews: ReadonlyMap<number, TurnDiffView>,
): string {
  const base = timelineNodeBaseHeightSignature(node);
  const divider = nodeHasCompletionDivider(node, latestSettledTurn)
    ? latestSettledTurnSignature(latestSettledTurn)
    : '';
  const turnDiff = isLastRootInTurn(index, nodes) && showEndOfTurnDiffs
    ? turnDiffViewHeightSignature(diffViews.get(rootTurnIndex(node)))
    : '';
  return [base, divider, turnDiff].join('::');
}

function timelineNodeBaseHeightSignature(node: TimelineNode): string {
  if (node.kind === 'group') {
    const children = node.children
      .map((child) => timelineNodeBaseHeightSignature(child))
      .join('|');
    return [
      'group',
      timelineItemHeightSignature(node.parent),
      node.children.length,
      children,
    ].join(':');
  }

  return `leaf:${timelineItemHeightSignature(node.item)}`;
}

function nodeHasCompletionDivider(node: TimelineNode, turn: SettledTurn | null): boolean {
  if (!turn?.assistantMessageId || node.kind !== 'leaf') return false;
  return node.item.id === turn.assistantMessageId;
}

function latestSettledTurnSignature(turn: SettledTurn | null): string {
  if (!turn) return '';
  return [
    'divider',
    turn.turnId,
    turn.assistantMessageId ?? '',
    turn.completedAt,
    turn.stopReason,
    turn.aborted ? '1' : '0',
    hashString(turn.errorMessage),
  ].join(':');
}

function turnDiffViewHeightSignature(view: TurnDiffView | undefined): string {
  if (!view) return '';
  return [
    'diff',
    view.summary.fileCount,
    view.summary.insertions,
    view.summary.deletions,
    view.files
      .map((file) => [
        hashString(file.path),
        file.kind,
        file.insertions,
        file.deletions,
        file.payloadId,
      ].join(','))
      .join('|'),
  ].join(':');
}

function timelineItemHeightSignature(item: Item): string {
  return [
    item.kind,
    item.status,
    item.payloadKind ?? '',
    item.payloadId ?? '',
    item.toolName ?? '',
    contentSignature(item.summary),
    contentSignature(item.meta ?? ''),
    contentSignature(item.payloadMeta ?? ''),
    item.updatedAt,
  ].join(':');
}

function contentSignature(value: string): string {
  if (!value) return '0:0';
  return `${value.length}:${hashString(value)}`;
}

function hashString(value: string): number {
  let hash = 5381;
  for (let index = 0; index < value.length; index += 1) {
    hash = ((hash << 5) + hash) ^ value.charCodeAt(index);
  }
  return hash >>> 0;
}
