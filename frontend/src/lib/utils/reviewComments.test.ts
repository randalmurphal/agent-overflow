import { describe, expect, it } from 'vitest';
import { commentSnippet, countReviewComments, visibleBody } from './reviewComments';
import type { DiffReviewComment, ReviewThread } from '../types/models';

function thread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: overrides.id ?? 'thread-1',
    path: overrides.path ?? 'src/a.ts',
    line: overrides.line !== undefined ? overrides.line : 5,
    side: overrides.side ?? 'new',
    isResolvable: overrides.isResolvable ?? true,
    isResolved: overrides.isResolved ?? false,
    isOutdated: overrides.isOutdated ?? false,
    comments: overrides.comments ?? [
      { authorLogin: 'alice', body: 'first line\nsecond line', createdAt: '2026-01-01', databaseID: 1 },
      { authorLogin: 'bob', body: 'reply', createdAt: '2026-01-02', databaseID: 2 },
    ],
  };
}

function draft(overrides: Partial<DiffReviewComment> = {}): DiffReviewComment {
  return {
    id: overrides.id ?? 'draft-1',
    threadId: 'thread-1',
    scope: 'pr',
    sourceKey: 'source',
    filePath: overrides.filePath ?? 'src/a.ts',
    status: 'draft',
    oldLine: overrides.oldLine,
    newLine: overrides.newLine ?? 2,
    side: overrides.side ?? 'new',
    selectedText: '',
    body: overrides.body ?? 'my draft',
    createdAt: 1,
    updatedAt: 1,
  };
}

describe('commentSnippet', () => {
  it('takes the first non-empty line and truncates long ones', () => {
    expect(commentSnippet('\n\n  hello there  \nrest')).toBe('hello there');
    const long = 'x '.repeat(200);
    expect(commentSnippet(long).length).toBe(160);
    expect(commentSnippet(long).endsWith('…')).toBe(true);
  });

  it('skips bot badge lines and surfaces the bolded finding title', () => {
    // CodeRabbit-shaped body from a real GitLab MR review.
    const body = [
      '_🛠️ Functional Correctness_ | _🟠 Major_ | _⚡ Quick win_',
      '',
      "**Do not infer the primary leg's provider from `primary_tier` alone.**",
      '',
      '`primary_provider_models` is still a runtime setting…',
    ].join('\n');
    expect(commentSnippet(body)).toBe(
      "Do not infer the primary leg's provider from primary_tier alone.",
    );
  });

  it('strips inline markdown but keeps snake_case identifiers intact', () => {
    expect(commentSnippet('**Bold** with `code` and [a link](https://x) and _emph_')).toBe(
      'Bold with code and a link and emph',
    );
    expect(commentSnippet('Derive from `primary_provider_models` instead')).toBe(
      'Derive from primary_provider_models instead',
    );
  });

  it('skips HTML comments, tags, table rows, and fenced code', () => {
    const body = [
      '<!-- fingerprint:abc123 -->',
      '<details><summary>🤖 Prompt for AI Agents</summary>',
      '| col | col2 |',
      '|-----|------|',
      '```suggestion',
      'code_line = 1',
      '```',
      'The actual point of the comment.',
    ].join('\n');
    expect(commentSnippet(body)).toBe('The actual point of the comment.');
  });

  it('strips heading, blockquote, and list markers', () => {
    expect(commentSnippet('## Walkthrough')).toBe('Walkthrough');
    expect(commentSnippet('> quoted intro')).toBe('quoted intro');
    expect(commentSnippet('- first bullet point')).toBe('first bullet point');
  });

  it('skips pipe-less italic category labels', () => {
    const body = ['_⚠️ Potential issue_', '', '**Race in the retry loop.**'].join('\n');
    expect(commentSnippet(body)).toBe('Race in the retry loop.');
  });

  it('returns empty for bodies with no prose', () => {
    expect(commentSnippet('🎉 ✅ | 🚀')).toBe('');
    expect(commentSnippet('')).toBe('');
  });

  it('unwraps badge rows (a link wrapping an image) with no syntax residue', () => {
    expect(commentSnippet('[![Build passing](https://img/b.svg)](https://ci/run) ready to merge')).toBe(
      'Build passing ready to merge',
    );
  });
});

describe('visibleBody', () => {
  it('is empty for marker-only bot replies', () => {
    expect(visibleBody('<!-- coderabbit resolve -->')).toBe('');
    expect(visibleBody(' <!-- a -->\n<!-- b --> ')).toBe('');
    expect(visibleBody('')).toBe('');
  });

  it('strips markers glued to real prose and keeps the prose', () => {
    expect(visibleBody('<!-- fingerprint:abc -->\nReal reply.')).toBe('Real reply.');
  });

  it('drops an unterminated trailing comment', () => {
    expect(visibleBody('Prose first.\n<!-- truncated marker')).toBe('Prose first.');
  });
});

describe('countReviewComments', () => {
  it('counts per file and overall, unresolved only for open resolvable threads', () => {
    const counts = countReviewComments({
      prThreads: [
        thread({ id: 't1', path: 'src/a.ts' }),
        thread({ id: 't2', path: 'src/a.ts', isResolved: true }),
        thread({ id: 't3', path: 'src/a.ts', isOutdated: true }),
        // A PR-level comment: neutral, never unresolved.
        thread({ id: 'conv', path: '', line: null, side: '', isResolvable: false }),
      ],
      drafts: [draft({ id: 'd1', filePath: 'src/b.ts' })],
    });

    expect(counts.byFile.get('src/a.ts')).toEqual({ total: 3, unresolved: 1 });
    expect(counts.byFile.get('src/b.ts')).toEqual({ total: 1, unresolved: 0 });
    expect(counts.byFile.get('')).toEqual({ total: 1, unresolved: 0 });
    expect(counts.byFile.has('src/c.ts')).toBe(false);
    expect(counts.tally).toEqual({ unresolved: 1, drafts: 1, total: 5 });
  });

  it('is empty with nothing to count', () => {
    const counts = countReviewComments({ prThreads: [], drafts: [] });
    expect(counts.byFile.size).toBe(0);
    expect(counts.tally).toEqual({ unresolved: 0, drafts: 0, total: 0 });
  });
});
