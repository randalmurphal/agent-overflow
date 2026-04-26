import type {
  Item,
  ProposedPlanItemMeta,
  ProposedPlanMeta,
  SourceProposedPlan,
} from '../types/models';

export interface ProposedPlanMarkdownBlock {
  id: string;
  markdown: string;
  startLine: number;
  endLine: number;
}

export function proposedPlanTitle(planMarkdown: string): string | null {
  const heading = planMarkdown.match(/^\s{0,3}#{1,6}\s+(.+)$/m)?.[1]?.trim();
  return heading && heading.length > 0 ? heading : null;
}

export function stripDisplayedPlanMarkdown(planMarkdown: string): string {
  const lines = planMarkdown.trimEnd().split(/\r?\n/);
  const sourceLines = lines[0] && /^\s{0,3}#{1,6}\s+/.test(lines[0]) ? lines.slice(1) : [...lines];
  while (sourceLines[0]?.trim().length === 0) {
    sourceLines.shift();
  }
  const firstHeadingMatch = sourceLines[0]?.match(/^\s{0,3}#{1,6}\s+(.+)$/);
  if (firstHeadingMatch?.[1]?.trim().toLowerCase() === 'summary') {
    sourceLines.shift();
    while (sourceLines[0]?.trim().length === 0) {
      sourceLines.shift();
    }
  }
  return sourceLines.join('\n');
}

/**
 * Wrap plan markdown into the prompt body the agent sees when the user
 * implements a plan. Stable string literal — referenced from tests too.
 */
export function buildPlanImplementationPrompt(planMarkdown: string): string {
  return `PLEASE IMPLEMENT THIS PLAN:\n${planMarkdown.trim()}`;
}

/**
 * Title used for the new thread spawned by "Implement plan in new thread".
 * Falls back to a generic label when the plan has no leading heading.
 */
export function buildPlanImplementationThreadTitle(planMarkdown: string): string {
  const title = proposedPlanTitle(planMarkdown);
  if (!title) return 'Implement plan';
  return `Implement ${title}`;
}

function sanitizePlanFileSegment(input: string): string {
  const sanitized = input
    .toLowerCase()
    .replace(/[`'".,!?()[\]{}]+/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
  return sanitized.length > 0 ? sanitized : 'plan';
}

export function buildProposedPlanMarkdownFilename(planMarkdown: string): string {
  return `${sanitizePlanFileSegment(proposedPlanTitle(planMarkdown) ?? 'plan')}.md`;
}

export function normalizePlanMarkdownForExport(planMarkdown: string): string {
  return `${planMarkdown.trimEnd()}\n`;
}

export function parseProposedPlanItemMeta(item?: Pick<Item, 'meta'> | null): ProposedPlanItemMeta {
  if (!item?.meta) return {};
  try {
    return JSON.parse(item.meta) as ProposedPlanItemMeta;
  } catch {
    return {};
  }
}

export function parseProposedPlanPayloadMeta(item?: Pick<Item, 'payloadMeta'> | null): ProposedPlanMeta {
  if (!item?.payloadMeta) {
    return { title: 'Proposed plan', preview: '', lineCount: 0, charCount: 0 };
  }
  try {
    return JSON.parse(item.payloadMeta) as ProposedPlanMeta;
  } catch {
    return { title: 'Proposed plan', preview: '', lineCount: 0, charCount: 0 };
  }
}

export function sourceFromProposedPlanItem(threadId: string | null | undefined, item: Item | null | undefined): SourceProposedPlan | null {
  if (!threadId || !item || item.payloadKind !== 'proposed_plan') return null;
  const itemMeta = parseProposedPlanItemMeta(item);
  if (itemMeta.planImplementedAt) return null;
  const payloadMeta = parseProposedPlanPayloadMeta(item);
  return {
    threadId,
    itemId: item.id,
    payloadId: item.payloadId,
    title: payloadMeta.title || 'Proposed plan',
  };
}

export function latestProposedPlanItem(
  threadId: string | null | undefined,
  ...itemGroups: Array<readonly Item[] | null | undefined>
): Item | null {
  if (!threadId) return null;
  const seenItemIds = new Set<string>();
  let latest: Item | null = null;
  for (const group of itemGroups) {
    for (const item of group ?? []) {
      if (item.threadId !== threadId || seenItemIds.has(item.id)) continue;
      seenItemIds.add(item.id);
      if (item.payloadKind !== 'proposed_plan' || !item.payloadId) continue;
      if (!latest || comparePlanItemPosition(item, latest) > 0) {
        latest = item;
      }
    }
  }
  return latest;
}

function comparePlanItemPosition(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return a.updatedAt - b.updatedAt;
}

export function splitProposedPlanMarkdownBlocks(markdown: string): ProposedPlanMarkdownBlock[] {
  const lines = markdown.trimEnd().split(/\r?\n/);
  if (lines.length === 1 && lines[0] === '') return [];

  const blocks: ProposedPlanMarkdownBlock[] = [];
  let blockStart = 0;
  let inFence = false;
  let fenceChar = '';
  let fenceLength = 0;

  function pushBlock(endIndex: number): void {
    while (blockStart <= endIndex && lines[blockStart]?.trim() === '') {
      blockStart += 1;
    }
    while (endIndex >= blockStart && lines[endIndex]?.trim() === '') {
      endIndex -= 1;
    }
    if (blockStart > endIndex) return;
    blocks.push({
      id: `${blockStart + 1}-${endIndex + 1}`,
      markdown: lines.slice(blockStart, endIndex + 1).join('\n'),
      startLine: blockStart + 1,
      endLine: endIndex + 1,
    });
  }

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index] ?? '';
    const fence = line.match(/^\s*(`{3,}|~{3,})/);
    if (fence) {
      const marker = fence[1] ?? '';
      if (!inFence) {
        inFence = true;
        fenceChar = marker[0] ?? '';
        fenceLength = marker.length;
      } else if ((marker[0] ?? '') === fenceChar && marker.length >= fenceLength) {
        inFence = false;
      }
      continue;
    }

    if (!inFence && line.trim() === '') {
      pushBlock(index - 1);
      blockStart = index + 1;
    }
  }
  pushBlock(lines.length - 1);
  return blocks;
}
