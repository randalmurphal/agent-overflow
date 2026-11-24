import { describe, expect, it } from 'vitest';
import { commentSnippet, countReviewComments, visibleBody } from './reviewComments';
import { indexReviewComments } from './reviewRows';
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
      { authorLogin: 'alice', body: 'first line\nsecond line', createdAt: '2026-01-01', databaseID: 1, id: `n${1}`, viewerCanEdit: false, viewerCanDelete: false },
      { authorLogin: 'bob', body: 'reply', createdAt: '2026-01-02', databaseID: 2, id: `n${2}`, viewerCanEdit: false, viewerCanDelete: false },
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

  it.each([
    // CodeRabbit-shaped body from a real GitLab MR review.
    [
      'skips bot badge lines and surfaces the bolded finding title',
      [
        '_🛠️ Functional Correctness_ | _🟠 Major_ | _⚡ Quick win_',
        '',
        "**Do not infer the primary leg's provider from `primary_tier` alone.**",
        '',
        '`primary_provider_models` is still a runtime setting…',
      ].join('\n'),
      "Do not infer the primary leg's provider from primary_tier alone.",
    ],
    ['strips inline markdown', '**Bold** with `code` and [a link](https://x) and _emph_', 'Bold with code and a link and emph'],
    ['keeps snake_case identifiers intact', 'Derive from `primary_provider_models` instead', 'Derive from primary_provider_models instead'],
    [
      'skips HTML comments, tags, table rows, and fenced code',
      [
        '<!-- fingerprint:abc123 -->',
        '<details><summary>🤖 Prompt for AI Agents</summary>',
        '| col | col2 |',
        '|-----|------|',
        '```suggestion',
        'code_line = 1',
        '```',
        'The actual point of the comment.',
      ].join('\n'),
      'The actual point of the comment.',
    ],
    ['strips a heading marker', '## Walkthrough', 'Walkthrough'],
    ['strips a blockquote marker', '> quoted intro', 'quoted intro'],
    ['strips a list marker', '- first bullet point', 'first bullet point'],
    ['skips pipe-less italic category labels', '_⚠️ Potential issue_\n\n**Race in the retry loop.**', 'Race in the retry loop.'],
    ['returns empty for a body with no prose', '🎉 ✅ | 🚀', ''],
    ['returns empty for an empty body', '', ''],
    ['unwraps badge rows (a link wrapping an image) with no syntax residue', '[![Build passing](https://img/b.svg)](https://ci/run) ready to merge', 'Build passing ready to merge'],
  ])('%s', (_name, body, want) => {
    expect(commentSnippet(body)).toBe(want);
  });
});

describe('visibleBody', () => {
  it.each([
    ['is empty for a marker-only bot reply', '<!-- coderabbit resolve -->', ''],
    ['is empty for several padded markers', ' <!-- a -->\n<!-- b --> ', ''],
    ['is empty for an empty body', '', ''],
    ['strips markers glued to real prose and keeps the prose', '<!-- fingerprint:abc -->\nReal reply.', 'Real reply.'],
    ['drops an unterminated trailing comment', 'Prose first.\n<!-- truncated marker', 'Prose first.'],
  ])('%s', (_name, body, want) => {
    expect(visibleBody(body)).toBe(want);
  });

  it('keeps fenced code whole: a comment opener there is code', () => {
    const fenced = 'Before.\n```ts\nconst opener = "<!--";\n```\nAfter. <!-- gone -->\n~~~\n<!-- kept -->\n~~~';
    expect(visibleBody(fenced)).toBe('Before.\n```ts\nconst opener = "<!--";\n```\nAfter. \n~~~\n<!-- kept -->\n~~~');
    // A longer closer, CRLF, an indented fence, and a fence never closed.
    expect(visibleBody('````\r\n<!--\r\n```\r\n`````\r\nx <!-- y -->')).toBe('````\r\n<!--\r\n```\r\n`````\r\nx');
    expect(visibleBody('   ```\n<!--\n   ```\nx')).toBe('```\n<!--\n   ```\nx');
    expect(visibleBody('a\n```\n<!-- to the end')).toBe('a\n```\n<!-- to the end');
  });

  it('reads a fence only where one opens', () => {
    // A comment that opens first swallows the fence inside it.
    expect(visibleBody('a <!--\n```\nx\n```\n--> b')).toBe('a  b');
    // Backticks later on the line make it a code span, not a fence, and a
    // fence opens only at the start of a line.
    expect(visibleBody('```a``` <!-- x -->\ntail')).toBe('```a``` \ntail');
    expect(visibleBody('<!-- x -->```\n<!-- y -->')).toBe('```');
  });

  it('stays linear over many comments ahead of a fence', () => {
    const body = `${'<!-- m -->\n'.repeat(200000)}\`\`\`\n<!--\n\`\`\``;
    const started = performance.now();
    expect(visibleBody(body)).toBe('```\n<!--\n```');
    expect(performance.now() - started).toBeLessThan(1000);
  });
});

describe('countReviewComments', () => {
  it('counts per file and overall, unresolved for every open resolvable thread, outdated included', () => {
    const counts = countReviewComments(indexReviewComments(
      [
        thread({ id: 't1', path: 'src/a.ts' }),
        thread({ id: 't2', path: 'src/a.ts', isResolved: true }),
        // Outdated but open: the code moved, the question is still asked.
        thread({ id: 't3', path: 'src/a.ts', isOutdated: true }),
        // A PR-level comment: neutral, never unresolved.
        thread({ id: 'conv', path: '', line: null, side: '', isResolvable: false }),
      ],
      [draft({ id: 'd1', filePath: 'src/b.ts' })],
    ));

    expect(counts.byFile.get('src/a.ts')).toEqual({ total: 3, unresolved: 2 });
    expect(counts.byFile.get('src/b.ts')).toEqual({ total: 1, unresolved: 0 });
    expect(counts.byFile.get('')).toEqual({ total: 1, unresolved: 0 });
    expect(counts.byFile.has('src/c.ts')).toBe(false);
    expect(counts.tally).toEqual({ unresolved: 2, drafts: 1, total: 5 });
  });

  it('counts a held reply in its thread\'s file, and in the file it was held on once the thread is gone', () => {
    const reply = { ...draft({ id: 'r1', filePath: 'src/a.ts', side: 'reply', newLine: 0 }), replyThreadId: 't1' };
    const moved = countReviewComments(indexReviewComments([thread({ id: 't1', path: 'src/b.ts' })], [reply]));
    expect(moved.byFile.get('src/b.ts')).toEqual({ total: 2, unresolved: 1 });
    expect(moved.byFile.has('src/a.ts')).toBe(false);
    const gone = countReviewComments(indexReviewComments([], [reply]));
    expect(gone.byFile.get('src/a.ts')).toEqual({ total: 1, unresolved: 0 });
  });

  it('is empty with nothing to count', () => {
    const counts = countReviewComments(indexReviewComments([], []));
    expect(counts.byFile.size).toBe(0);
    expect(counts.tally).toEqual({ unresolved: 0, drafts: 0, total: 0 });
  });
});
