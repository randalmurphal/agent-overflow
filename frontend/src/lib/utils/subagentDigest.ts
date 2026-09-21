// The agent DIGEST: the allowlist of an agent's child rows that its
// collapsed-card body and its background tray row show
// (docs/specs/agent-visibility.md Q2; user ruling 2026-08-23). The body is
// what the agent was asked, what it did, and what it produced: the
// initial prompt (the first user_text; Codex echoes the spawn prompt as
// one), its tool calls, a provider refusal's reason (the only place "why a
// tool did not run" lives), errors, and its FINAL text. Everything else
// (thinking, intermediate prose, later prompts, progress chatter,
// compaction, retries) lives in the agent pane. Nested launches also stay
// in the pane, where they render as direct child rows that navigate the
// same pane with breadcrumbs.

import type { TimelineNode } from './subagentGrouping';
import { parseJsonObject } from './parseJsonObject';

/**
 * Main-thread agent digests never embed another agent card. Wait groups
 * can contain completed agent cards, so filtering only direct children is
 * insufficient. Keep the wait carrier and remove its nested agent rows.
 */
export function withoutNestedAgentCards(node: TimelineNode): TimelineNode | null {
  if (node.kind === 'group') return null;
  if (node.kind !== 'wait_group') return node;
  const children = node.children
    .map(withoutNestedAgentCards)
    .filter((child): child is TimelineNode => child !== null);
  return {
    ...node,
    children,
    descendantCount: children.length,
  };
}

/**
 * The digest rows of an agent's direct children, in order.
 *
 * `keepFinalText` says whether the agent's latest text is an answer: true
 * while it runs (the latest text is its live report) or after a clean
 * completion. A killed or errored agent's last text is mid-flight prose,
 * not an answer, and a forked Skill publishes its answer as a top-level
 * result; both keep only tool calls and errors.
 */
export function subagentDigestNodes(
  children: readonly TimelineNode[],
  keepFinalText: boolean,
): TimelineNode[] {
  let lastTextId = '';
  let firstPromptId = '';
  for (const node of children) {
    if (node.kind !== 'leaf') continue;
    if (keepFinalText && node.item.kind === 'assistant_text') lastTextId = node.item.id;
    if (!firstPromptId && node.item.kind === 'user_text') firstPromptId = node.item.id;
  }
  return children.flatMap((node) => {
    if (node.kind !== 'leaf') {
      const sanitized = withoutNestedAgentCards(node);
      return sanitized ? [sanitized] : [];
    }
    const item = node.item;
    switch (item.kind) {
      case 'tool_call':
      case 'tool_completion':
      case 'error':
      case 'api_error':
        return [node];
      case 'user_text':
        return item.id === firstPromptId ? [node] : [];
      case 'assistant_text':
        return item.id === lastTextId ? [node] : [];
      case 'notification': {
        const kind = parseJsonObject(item.meta)?.kind ?? item.toolName;
        return kind === 'permission_denied' || kind === 'transcript_mirror_degraded'
          ? [node]
          : [];
      }
      default:
        return [];
    }
  });
}
