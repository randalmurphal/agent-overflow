import { describe, expect, it } from 'vitest';
import {
  buildCommentGroups,
  commentCountsByFile,
  commentSnippet,
  commentTally,
} from './reviewComments';
import type { PatchFile } from './patchFiles';
import type { DiffReviewComment, ReviewThread } from '../types/models';

function file(path: string): PatchFile {
  return { path, kind: 'modified', additions: 1, deletions: 0, lines: [] };
}

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

describe('buildCommentGroups', () => {
  it('groups by file in diff order with comment-only paths appended', () => {
    const groups = buildCommentGroups({
      files: [file('src/b.ts'), file('src/a.ts')],
      prThreads: [
        thread({ id: 't1', path: 'src/a.ts' }),
        thread({ id: 't2', path: 'zz/not-in-diff.ts' }),
        thread({ id: 't3', path: 'aa/not-in-diff.ts' }),
      ],
      drafts: [draft({ id: 'd1', filePath: 'src/b.ts' })],
      orphanedDraftIds: new Set(),
    });

    expect(groups.map((group) => group.filePath)).toEqual([
      'src/b.ts',
      'src/a.ts',
      'aa/not-in-diff.ts',
      'zz/not-in-diff.ts',
    ]);
    expect(groups.map((group) => group.inDiff)).toEqual([true, true, false, false]);
    expect(groups[3]!.items[0]!.inDiff).toBe(false);
  });

  it('sorts actionable items first, then by line', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts')],
      prThreads: [
        thread({ id: 'resolved', line: 1, isResolved: true }),
        thread({ id: 'late', line: 9 }),
        thread({ id: 'early', line: 3 }),
      ],
      drafts: [draft({ id: 'd1', newLine: 6 })],
      orphanedDraftIds: new Set(),
    });

    expect(groups[0]!.items.map((item) => item.rowKey)).toEqual([
      'pt:early',
      't:d1',
      'pt:late',
      'pt:resolved',
    ]);
  });

  it('carries state, author, replies, orphaned flags, and row keys', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts')],
      prThreads: [
        thread({ id: 't-open' }),
        thread({ id: 't-outdated', isOutdated: true }),
      ],
      drafts: [draft({ id: 'd-orphan' })],
      orphanedDraftIds: new Set(['d-orphan']),
    });

    const items = groups[0]!.items;
    const open = items.find((item) => item.rowKey === 'pt:t-open')!;
    expect(open.state).toBe('unresolved');
    expect(open.author).toBe('alice');
    // ISO createdAt from the forge parses to epoch ms; the stub's
    // "2026-01-01" is date-only but still valid ISO.
    expect(open.createdAtMs).toBe(Date.parse('2026-01-01'));
    expect(open.replies).toBe(1);
    expect(open.snippet).toBe('first line');
    expect(open.threadId).toBe('t-open');

    const outdated = items.find((item) => item.rowKey === 'pt:t-outdated')!;
    expect(outdated.state).toBe('outdated');
    expect(outdated.orphaned).toBe(true);

    const orphanDraft = items.find((item) => item.rowKey === 't:d-orphan')!;
    expect(orphanDraft.state).toBe('draft');
    expect(orphanDraft.orphaned).toBe(true);
    expect(orphanDraft.author).toBe('You');
    expect(orphanDraft.threadId).toBeNull();
    // Drafts store epoch ms directly.
    expect(orphanDraft.createdAtMs).toBe(1);
  });

  it('maps unparseable thread timestamps to null', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts')],
      prThreads: [
        thread({
          id: 't1',
          comments: [{ authorLogin: 'alice', body: 'x', createdAt: 'not-a-date', databaseID: 1 }],
        }),
      ],
      drafts: [],
      orphanedDraftIds: new Set(),
    });
    expect(groups[0]!.items[0]!.createdAtMs).toBeNull();
  });

  it('leads with a conversation group for path-less threads', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts')],
      prThreads: [
        thread({ id: 'diff-thread', path: 'src/a.ts' }),
        thread({
          id: 'conv-flat',
          path: '',
          line: null,
          side: '',
          isResolvable: false,
          comments: [{ authorLogin: 'coderabbitai', body: 'Walkthrough…', createdAt: '2026-01-01', databaseID: 9 }],
        }),
        thread({ id: 'conv-resolvable', path: '', line: null, side: '', isResolved: true }),
      ],
      drafts: [],
      orphanedDraftIds: new Set(),
    });

    expect(groups.map((group) => group.filePath)).toEqual(['', 'src/a.ts']);
    const conversation = groups[0]!;
    expect(conversation.inDiff).toBe(false);
    const flat = conversation.items.find((item) => item.rowKey === 'pt:conv-flat')!;
    // Non-resolvable → neutral state, and never jumpable (no diff row).
    expect(flat.state).toBe('comment');
    expect(flat.inDiff).toBe(false);
    expect(flat.comments).toEqual([{ author: 'coderabbitai', body: 'Walkthrough…' }]);
    const resolvable = conversation.items.find((item) => item.rowKey === 'pt:conv-resolvable')!;
    expect(resolvable.state).toBe('resolved');
    // Neutral comments do not inflate the unresolved tally.
    expect(commentTally(groups)).toEqual({ unresolved: 1, drafts: 0, total: 3 });
  });

  it('file-level drafts have no line', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts')],
      prThreads: [],
      drafts: [draft({ id: 'd1', side: 'file', newLine: undefined })],
      orphanedDraftIds: new Set(),
    });
    expect(groups[0]!.items[0]!.line).toBeNull();
  });
});

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
});

describe('tallies', () => {
  it('counts per file and overall', () => {
    const groups = buildCommentGroups({
      files: [file('src/a.ts'), file('src/b.ts')],
      prThreads: [
        thread({ id: 't1', path: 'src/a.ts' }),
        thread({ id: 't2', path: 'src/a.ts', isResolved: true }),
      ],
      drafts: [draft({ id: 'd1', filePath: 'src/b.ts' })],
      orphanedDraftIds: new Set(),
    });

    expect(commentCountsByFile(groups).get('src/a.ts')).toBe(2);
    expect(commentCountsByFile(groups).get('src/b.ts')).toBe(1);
    expect(commentTally(groups)).toEqual({ unresolved: 1, drafts: 1, total: 3 });
  });
});
