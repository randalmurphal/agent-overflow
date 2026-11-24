// Pure string transforms used by ProposedPlanCard. Each path is load-
// bearing for what the user sees when a plan renders, so title extraction
// and filename sanitization both deserve a test.

import { describe, expect, it } from 'vitest';
import {
  buildPlanImplementationPrompt,
  buildPlanImplementationThreadTitle,
  buildProposedPlanMarkdownFilename,
  isProposedPlanPreviewTruncated,
  latestProposedPlanItem,
  normalizePlanMarkdownForExport,
  proposedPlanPayloadVersion,
  proposedPlanTitle,
  shouldCapProposedPlanBody,
  splitProposedPlanMarkdownBlocks,
} from './proposedPlan';
import type { Item } from '../types/models';

function planItem(overrides: Partial<Item> = {}): Item {
  return {
    rev: 0,
    id: 'plan-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: 'ExitPlanMode',
    payloadKind: 'proposed_plan',
    payloadId: 'payload-1',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  };
}

describe('proposedPlanTitle', () => {
  it.each([
    ['returns the first heading as the title', '# My plan\nbody', 'My plan'],
    ['accepts H1–H6', '### Nested heading', 'Nested heading'],
    ['trims leading whitespace up to 3 spaces before the heading marker', '   # Indented', 'Indented'],
    ['returns null when no heading is found', 'just body text', null],
    ['returns null for empty input', '', null],
    // The regex uses the /m flag so any line that starts with a heading
    // marker qualifies — falls out of the match at the first one.
    ['treats a heading deeper in the document the same as the first line', 'body\n# Title later\nmore body', 'Title later'],
  ])('%s', (_name, input, want) => {
    expect(proposedPlanTitle(input)).toBe(want);
  });
});

describe('proposed plan body sizing helpers', () => {
  it('uses the backend truncation flag when present', () => {
    expect(isProposedPlanPreviewTruncated({
      title: 'Plan',
      lineCount: 3,
      charCount: 20,
      preview: 'short',
      previewTruncated: true,
    })).toBe(true);
  });

  it('falls back to legacy preview shape for older metadata', () => {
    expect(isProposedPlanPreviewTruncated({
      title: 'Plan',
      lineCount: 11,
      charCount: 80,
      preview: 'line 1\n\n...',
    })).toBe(true);
  });

  it('caps long single-paragraph plans even when the line count is small', () => {
    expect(shouldCapProposedPlanBody({
      title: 'Plan',
      lineCount: 1,
      charCount: 901,
      preview: 'long',
      previewTruncated: false,
    })).toBe(true);
  });

  it('prefers the payload signature as the payload version', () => {
    const item = planItem({
      payloadMeta: JSON.stringify({ title: 'Plan' }),
      updatedAt: 42,
    });

    expect(proposedPlanPayloadVersion(item, {
      title: 'Plan',
      lineCount: 1,
      charCount: 4,
      preview: 'body',
      signature: 'sha256:abc',
    })).toBe('sha256:abc');
  });
});

describe('buildProposedPlanMarkdownFilename', () => {
  it.each([
    ['slugifies the title into a lowercase kebab-case .md name', '# Fix the login flow', 'fix-the-login-flow.md'],
    ['drops punctuation and parentheses', "# Don't touch (yet)!", 'dont-touch-yet.md'],
    ['falls back to "plan.md" when the title has no usable characters', '# !!!', 'plan.md'],
    ['falls back to "plan.md" when no title is present', 'no heading here', 'plan.md'],
    ['collapses runs of spaces into single hyphens', '# Make    it   so', 'make-it-so.md'],
    ['strips leading and trailing hyphens after sanitization', '# ---wrap---', 'wrap.md'],
  ])('%s', (_name, input, want) => {
    expect(buildProposedPlanMarkdownFilename(input)).toBe(want);
  });
});

describe('normalizePlanMarkdownForExport', () => {
  it.each([
    ['adds the one trailing newline', 'body', 'body\n'],
    ['keeps exactly one trailing newline', 'body\n', 'body\n'],
    ['collapses trailing newlines to one', 'body\n\n\n', 'body\n'],
    ['strips trailing whitespace on the last line', 'body   ', 'body\n'],
    ['preserves interior blank lines', 'a\n\nb', 'a\n\nb\n'],
  ])('%s', (_name, input, want) => {
    expect(normalizePlanMarkdownForExport(input)).toBe(want);
  });
});

describe('latestProposedPlanItem', () => {
  it('returns the latest plan even when later assistant text exists', () => {
    const olderPlan = planItem({ id: 'plan-1', turnIndex: 2, itemIndex: 0 });
    const newerText = planItem({
      id: 'text-1',
      kind: 'assistant_text',
      payloadKind: undefined,
      payloadId: undefined,
      turnIndex: 2,
      itemIndex: 1,
    });

    expect(latestProposedPlanItem('thread-1', [olderPlan, newerText])?.id).toBe('plan-1');
  });

  it('returns the latest plan even when it has already been implemented', () => {
    const oldPlan = planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 });
    const implementedPlan = planItem({
      id: 'plan-2',
      turnIndex: 2,
      itemIndex: 0,
      meta: JSON.stringify({ planImplementedAt: 123 }),
    });

    expect(latestProposedPlanItem('thread-1', [oldPlan, implementedPlan])?.id).toBe('plan-2');
  });

  it('ignores items belonging to other threads', () => {
    const ownPlan = planItem({ id: 'plan-1', turnIndex: 1, itemIndex: 0 });
    const otherThreadPlan = planItem({
      id: 'plan-other',
      threadId: 'thread-2',
      turnIndex: 5,
      itemIndex: 0,
    });

    expect(latestProposedPlanItem('thread-1', [ownPlan, otherThreadPlan])?.id).toBe('plan-1');
  });
});

describe('splitProposedPlanMarkdownBlocks', () => {
  it('keeps source line ranges for repeated rendered text', () => {
    expect(splitProposedPlanMarkdownBlocks('Repeat\n\nRepeat')).toEqual([
      { id: '1-1', markdown: 'Repeat', startLine: 1, endLine: 1 },
      { id: '3-3', markdown: 'Repeat', startLine: 3, endLine: 3 },
    ]);
  });

  it('keeps multi-line list selections in one source block', () => {
    expect(splitProposedPlanMarkdownBlocks('- first\n- second\n\nNext')).toEqual([
      { id: '1-2', markdown: '- first\n- second', startLine: 1, endLine: 2 },
      { id: '4-4', markdown: 'Next', startLine: 4, endLine: 4 },
    ]);
  });

  it('does not split fenced code blocks on blank lines', () => {
    expect(splitProposedPlanMarkdownBlocks('```ts\nconst a = 1;\n\nconst b = 2;\n```\n\nAfter')).toEqual([
      { id: '1-5', markdown: '```ts\nconst a = 1;\n\nconst b = 2;\n```', startLine: 1, endLine: 5 },
      { id: '7-7', markdown: 'After', startLine: 7, endLine: 7 },
    ]);
  });
});

describe('buildPlanImplementationPrompt', () => {
  it.each([
    ['prefixes the trimmed plan with PLEASE IMPLEMENT THIS PLAN', '# Ship feature\n\n- step\n', 'PLEASE IMPLEMENT THIS PLAN:\n# Ship feature\n\n- step'],
    ['handles plans without a heading', 'just steps', 'PLEASE IMPLEMENT THIS PLAN:\njust steps'],
  ])('%s', (_name, input, want) => {
    expect(buildPlanImplementationPrompt(input)).toBe(want);
  });
});

describe('buildPlanImplementationThreadTitle', () => {
  it.each([
    ['uses the plan title as the implementation thread title', '# Ship feature\n\nbody', 'Implement Ship feature'],
    ['falls back to a generic label when there is no heading', '- step one\n- step two', 'Implement plan'],
  ])('%s', (_name, input, want) => {
    expect(buildPlanImplementationThreadTitle(input)).toBe(want);
  });
});
