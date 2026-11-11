import { describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { findNeedle } from './findMatch';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from './patchStore';
import { visibleBody } from './reviewComments';
import { forgetReviewFindSpotsForTest, placeReviewFindHit, resolveReviewFindHit, seedReviewFindSpot, type ReviewFindLocateContent, type ReviewFindResolution } from './reviewFindRows';
import { findThreadCommentKey, makeReviewFindHit, reviewCommentKey, scanReviewFind, type ReviewFindContent, type ReviewFindHit } from './reviewFindScan';
import { freshScan, shownOf, type Scanned } from '../../test/helpers/reviewFindOracle';
import type { DiffReviewComment, ReviewComment, ReviewThread } from '../types/models';

// The resolver against content that changed under published matches. The
// oracle is a fresh scan of the new content: a match resolves to where
// that scan finds the same identity (its element, the painter's index
// there, its place), else to nothing.

const QUERY = 'needle';
const NEEDLE = findNeedle(QUERY);

const diffHeader = (path: string) => [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`];

const PATCHES = {
  base: [
    ...diffHeader('src/a.test.ts'), '@@ -10,3 +10,3 @@', ' ctx needle', '-del needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -100,2 +100,2 @@', ' needle hundred', ' end', '',
  ],
  // A gap expanded above each match: every row moves down one.
  moved: [
    ...diffHeader('src/a.test.ts'), '@@ -9,4 +9,4 @@', ' nine', ' ctx needle', '-del needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -99,3 +99,3 @@', ' ninety-nine', ' needle hundred', ' end', '',
  ],
  // The removed line and the context line at 100 are gone.
  removed: [
    ...diffHeader('src/a.test.ts'), '@@ -10,2 +10,3 @@', ' ctx needle', '+add needle', ' tail',
    ...diffHeader('src/b.ts'), '@@ -101,1 +101,1 @@', ' end', '',
  ],
  // The same rows; the match on the added line is replaced.
  replaced: [
    ...diffHeader('src/a.test.ts'), '@@ -10,3 +10,3 @@', ' ctx needle', '-del needle', '+add pin', ' tail',
    ...diffHeader('src/b.ts'), '@@ -100,2 +100,2 @@', ' needle hundred', ' end', '',
  ],
};

const draft: DiffReviewComment = {
  id: 'd1', threadId: 't', scope: 'workspace', sourceKey: 'k', filePath: 'src/b.ts', status: 'draft',
  newLine: 100, side: 'new', selectedText: '', body: 'needle draft', createdAt: 0, updatedAt: 0,
};
const thread: ReviewThread = {
  id: 'p1', path: 'src/a.test.ts', line: 10, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [{ authorLogin: 'a', body: 'needle one', createdAt: '', databaseID: 1 }, { authorLogin: 'b', body: 'needle two', createdAt: '', databaseID: 2 }],
};

function diffContent(patch: string[], comments = true): ReviewFindContent {
  return { files: parseReviewFiles(patch.join('\n')), drafts: comments ? [draft] : [], prThreads: comments ? [thread] : [], commentText: (body) => [{ text: body, shown: true }] };
}

// A scan seeded as the pane's source seeds what its scans read.
async function seededScan(content: ReviewFindContent): Promise<Map<string, Scanned>> {
  const out = await freshScan(content, QUERY);
  await scanReviewFind(content, QUERY, { limit: 100_000, live: () => true, pause: async () => {}, emit: (hit, file, spot) => seedReviewFindSpot(file, hit, spot) });
  return out;
}

// What the fresh scan of `after` says `hit` resolves to.
function expected(fresh: Map<string, Scanned>, hit: ReviewFindHit): ReviewFindResolution {
  const scanned = fresh.get(hit.identity);
  return scanned ? { kind: 'present', at: scanned.at } : { kind: 'absent' };
}

// Every match of `published` resolved against `after`, with what the scans
// recorded and again with nothing recorded; the resolutions by identity.
async function expectResolvesTo(published: Iterable<ReviewFindHit>, after: ReviewFindContent, label = ''): Promise<Map<string, ReviewFindResolution>> {
  const fresh = await seededScan(after);
  const resolved = new Map<string, ReviewFindResolution>();
  for (const seeded of [true, false]) {
    if (!seeded) for (const file of after.files) forgetReviewFindSpotsForTest(file);
    for (const hit of published) {
      const got = resolveReviewFindHit(shownOf(after), hit, NEEDLE);
      expect(got, `${label} ${hit.identity} (${seeded ? 'seeded' : 'walked'})`).toEqual(expected(fresh, hit));
      const scanned = fresh.get(hit.identity);
      // A fresh match is placed where the scan read it.
      if (scanned) expect(placeReviewFindHit(after, hit), `${label} ${hit.identity}`).toEqual({ fileIndex: scanned.at.fileIndex, line: scanned.line });
      resolved.set(hit.identity, got);
    }
  }
  return resolved;
}

async function expectResolves(before: ReviewFindContent, after: ReviewFindContent): Promise<Map<string, ReviewFindResolution>> {
  const published = await seededScan(before);
  return expectResolvesTo([...published.values()].map((each) => each.hit), after);
}

const id = (path: string, element: string, occurrence: number | string = 0) => `${path}\u0000${element}\u0000${occurrence}`;
const pc = (threadId: string, comment: string) => findThreadCommentKey(threadId, comment);
const at = (resolution: ReviewFindResolution | undefined) => (resolution?.kind === 'present' ? resolution.at : null);

describe('resolveReviewFindHit', () => {
  it('every kind of diff match resolves where it is shown now', async () => {
    const base = diffContent(PATCHES.base);
    const resolved = await expectResolves(base, base);
    expect([...resolved.keys()]).toEqual([
      id('src/a.test.ts', pc('p1', '1'), '0:0'), id('src/a.test.ts', pc('p1', '2'), '0:0'), id('src/a.test.ts', 'c10:10'), id('src/a.test.ts', 'd11'), id('src/a.test.ts', 'a11'),
      id('src/b.ts', 'c100:100'), id('src/b.ts', 'dc:d1'),
    ]);
    expect(at(resolved.get(id('src/b.ts', 'c100:100')))).toEqual({ fileIndex: 1, rowKey: 'l:4:src/b.ts', ordinal: 0, place: { kind: 'line', lineIndex: 4 } });
    expect(at(resolved.get(id('src/b.ts', 'dc:d1')))).toEqual({ fileIndex: 1, rowKey: 'dc:d1', ordinal: 0, place: { kind: 'draft', commentId: 'd1' } });
  });

  it('a match whose row moved resolves to its new row', async () => {
    const resolved = await expectResolves(diffContent(PATCHES.base), diffContent(PATCHES.moved));
    expect(at(resolved.get(id('src/b.ts', 'c100:100')))).toMatchObject({ fileIndex: 1, rowKey: 'l:5:src/b.ts', place: { kind: 'line', lineIndex: 5 } });
    expect(at(resolved.get(id('src/a.test.ts', 'a11')))).toMatchObject({ rowKey: 'l:7:src/a.test.ts' });
  });

  it('a match whose line or comment is gone is not present', async () => {
    const resolved = await expectResolves(diffContent(PATCHES.base), diffContent(PATCHES.removed, false));
    expect(resolved.get(id('src/b.ts', 'c100:100'))).toEqual({ kind: 'absent' });
    expect(resolved.get(id('src/a.test.ts', 'd11'))).toEqual({ kind: 'absent' });
    expect(resolved.get(id('src/b.ts', 'dc:d1'))).toEqual({ kind: 'absent' });
    expect(resolved.get(id('src/a.test.ts', pc('p1', '2'), '0:0'))).toEqual({ kind: 'absent' });
    expect(at(resolved.get(id('src/a.test.ts', 'c10:10')))).not.toBeNull();
  });

  it('a line that keeps its number but loses the match is not present', async () => {
    const resolved = await expectResolves(diffContent(PATCHES.base), diffContent(PATCHES.replaced));
    expect(resolved.get(id('src/a.test.ts', 'a11'))).toEqual({ kind: 'absent' });
    expect(at(resolved.get(id('src/a.test.ts', 'd11')))).not.toBeNull();
  });

  it('a comment deleted while its file stays the same is not present; the next keeps its key', async () => {
    const base = diffContent(PATCHES.base);
    const resolved = await expectResolves(base, { ...base, drafts: [], prThreads: [{ ...thread, comments: thread.comments.slice(1) }] });
    expect(resolved.get(id('src/b.ts', 'dc:d1'))).toEqual({ kind: 'absent' });
    expect(resolved.get(id('src/a.test.ts', pc('p1', '1'), '0:0'))).toEqual({ kind: 'absent' });
    expect(at(resolved.get(id('src/a.test.ts', pc('p1', '2'), '0:0')))).toMatchObject({ rowKey: pc('p1', '2') });
  });

  it('a comment without a forge id is keyed by its time and author', () => {
    const comment: ReviewComment = { authorLogin: 'a:b', body: 'x', createdAt: '2026-01-01T00:00:00Z', databaseID: 0 };
    expect(reviewCommentKey(comment)).toBe('@2026-01-01T00:00:00Z@a:b');
    // The key is encoded: a colon in it does not split the row key.
    expect(pc('p:1', reviewCommentKey(comment))).toBe('pc:p%3A1:%402026-01-01T00%3A00%3A00Z%40a%3Ab');
  });

  it('a comment match is counted among the shown runs before it', async () => {
    const two: ReviewThread = { ...thread, comments: [{ authorLogin: 'a', body: 'needle|needle needle|needle', createdAt: '', databaseID: 7 }] };
    const runs = (hidden: boolean): ReviewFindContent => ({
      ...diffContent(PATCHES.base), prThreads: [two],
      commentText: (body) => body.split('|').map((text, index) => ({ text, shown: !hidden || index !== 1 })),
    });
    const resolved = await expectResolvesTo([makeReviewFindHit('src/a.test.ts', { kind: 'thread', threadId: 'p1', comment: '7' }, 2, 0)], runs(false));
    expect(at(resolved.values().next().value)).toMatchObject({ ordinal: 3 });
    // The <details> run before it closed: the same match, two fewer before it.
    const closed = await expectResolvesTo([makeReviewFindHit('src/a.test.ts', { kind: 'thread', threadId: 'p1', comment: '7' }, 2, 0)], runs(true));
    expect(at(closed.values().next().value)).toMatchObject({ ordinal: 1 });
  });

  it('a file hidden by a filter takes its matches with it; the others resolve at their new index', async () => {
    const base = diffContent(PATCHES.base);
    const hidden = { ...base, files: base.files.filter((file) => !file.path.endsWith('.test.ts')) };
    const resolved = await expectResolves(base, hidden);
    expect(resolved.get(id('src/a.test.ts', 'c10:10'))).toEqual({ kind: 'absent' });
    expect(at(resolved.get(id('src/b.ts', 'c100:100')))).toMatchObject({ fileIndex: 0, rowKey: 'l:4:src/b.ts' });
  });

  it('reordered files resolve by path', async () => {
    const base = diffContent(PATCHES.base);
    const resolved = await expectResolves(base, { ...base, files: [...base.files].reverse() });
    expect(at(resolved.get(id('src/b.ts', 'c100:100')))).toMatchObject({ fileIndex: 0 });
    expect(at(resolved.get(id('src/a.test.ts', pc('p1', '1'), '0:0')))).toMatchObject({ fileIndex: 1, rowKey: pc('p1', '1') });
  });

  it('a conflict fold\'s hidden match resolves to the fold, and to its line once the fold opens; markers move with it', async () => {
    const lead = Array.from({ length: 40 }, (_, i) => (i === 20 ? 'needle in the fold' : `lead ${i}`));
    // Their side is empty: the last marker is the second of a run.
    const source = [...lead, '<<<<<<< ours', 'needle ours', '=======', '>>>>>>> theirs needle'].join('\n');
    const build = (expanded: Set<number> | undefined): ReviewFindContent => {
      const folds = new Map<number, readonly string[]>();
      const file = reviewFileFromPatchFile(conflictPatchFile('c.go', source, { expandedFolds: expanded, onFold: (foldId, lines) => folds.set(foldId, lines) }));
      return { files: [file], drafts: [], prThreads: [], commentText: (body) => [{ text: body, shown: true }], foldLines: () => folds };
    };
    const closed = build(undefined);
    const atClosed = await expectResolves(closed, closed);
    const fold = at(atClosed.get(id('c.go', 'c21:21')));
    expect(fold).toMatchObject({ place: { kind: 'fold' } });
    const foldId = (fold!.place as { foldId: number }).foldId;
    const atOpen = await expectResolves(closed, build(new Set([foldId])));
    expect(at(atOpen.get(id('c.go', 'c21:21')))).toMatchObject({ place: { kind: 'line' } });
    expect([...atOpen.values()].every((each) => each.kind === 'present')).toBe(true);
  });

  it('a content listing no file of the match is not present', () => {
    const hit: ReviewFindHit = { identity: id('gone.ts', 'h'), path: 'gone.ts', at: { kind: 'header' }, run: 0, occurrence: 0 };
    const content: ReviewFindLocateContent = { files: [] as ReviewFile[], drafts: [], prThreads: [] };
    expect(placeReviewFindHit(content, hit)).toBeNull();
    expect(resolveReviewFindHit({ ...content, commentRuns: () => [], editing: new Set() }, hit, NEEDLE)).toEqual({ kind: 'absent' });
  });
});

// ---- Property tests over random review content and random changes ----

// A deterministic generator, so a failure names its seed.
function random(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const WORDS = ['needle', 'x', 'Needle', 'needleneedle', 'y', 'nee', 'dle'];

interface Row { type: ' ' | '+' | '-'; text: string }
interface DiffModel { path: string; start: number; rows: Row[] }
interface ConflictModel { path: string; lead: string[]; expanded: Set<number> }
interface Model {
  diffs: DiffModel[];
  conflict: ConflictModel;
  drafts: DiffReviewComment[];
  threads: ReviewThread[];
  /** Row keys of comments whose odd runs (a <details> body) are closed. */
  closed: Set<string>;
  editing: Set<string>;
  hiddenPaths: Set<string>;
  order: string[];
}

/** What a change touched: identities whose element source it changed. */
type Touched = (hit: ReviewFindHit) => boolean;

class World {
  private readonly next: () => number;
  private readonly parsed = new Map<string, ReviewFile>();
  private readonly conflicts = new Map<string, { file: ReviewFile; folds: Map<number, readonly string[]> }>();
  private ids = 100;
  model: Model;

  constructor(seed: number) {
    this.next = random(seed);
    const diffs = [0, 1].map((n) => ({ path: n === 0 ? 'src/needle.ts' : 'src/other.ts', start: 1 + Math.floor(this.next() * 50), rows: Array.from({ length: 4 + Math.floor(this.next() * 12) }, () => this.row()) }));
    const conflict: ConflictModel = { path: 'c.go', lead: Array.from({ length: 30 + Math.floor(this.next() * 20) }, () => this.text()), expanded: new Set() };
    this.model = { diffs, conflict, drafts: [], threads: [], closed: new Set(), editing: new Set(), hiddenPaths: new Set(), order: [...diffs.map((d) => d.path), conflict.path] };
    for (let i = 0; i < 3; i += 1) this.model.drafts.push(this.draft());
    for (let i = 0; i < 3; i += 1) this.model.threads.push(this.thread());
  }

  pick<T>(items: readonly T[]): T {
    return items[Math.floor(this.next() * items.length)];
  }

  chance(p: number): boolean {
    return this.next() < p;
  }

  text(): string {
    return Array.from({ length: Math.floor(this.next() * 4) }, () => this.pick(WORDS)).join(' ');
  }

  row(): Row {
    return { type: this.pick([' ', ' ', '+', '-'] as const), text: this.text() };
  }

  body(): string {
    // Runs split by '|'; odd runs are a <details> body.
    return Array.from({ length: 1 + Math.floor(this.next() * 4) }, () => this.text() || 'needle').join('|');
  }

  comment(): ReviewComment {
    this.ids += 1;
    // Some comments have no forge id.
    return { authorLogin: `u${this.ids}`, body: this.body(), createdAt: `t${this.ids}`, databaseID: this.chance(0.2) ? 0 : this.ids };
  }

  draft(): DiffReviewComment {
    this.ids += 1;
    const file = this.pick(this.model.diffs);
    return {
      id: `d${this.ids}`, threadId: 't', scope: 'workspace', sourceKey: 'k', filePath: file.path, status: 'draft',
      newLine: file.start + Math.floor(this.next() * 12), side: 'new', selectedText: '', body: this.text() || 'needle', createdAt: 0, updatedAt: 0,
    };
  }

  thread(): ReviewThread {
    this.ids += 1;
    const file = this.pick(this.model.diffs);
    return {
      id: `p${this.ids}`, path: file.path, line: this.chance(0.2) ? null : file.start + Math.floor(this.next() * 12), side: 'right',
      isResolvable: true, isResolved: false, isOutdated: false, comments: Array.from({ length: 1 + Math.floor(this.next() * 3) }, () => this.comment()),
    };
  }

  // The tag each row of a diff has, as the scan names code lines.
  tags(diff: DiffModel): string[] {
    let oldLine = diff.start;
    let newLine = diff.start;
    return diff.rows.map((row) => {
      if (row.type === '+') return `a${newLine++}`;
      if (row.type === '-') return `d${oldLine++}`;
      return `c${oldLine++}:${newLine++}`;
    });
  }

  private diffFile(diff: DiffModel): ReviewFile {
    const olds = diff.rows.filter((row) => row.type !== '+').length;
    const news = diff.rows.filter((row) => row.type !== '-').length;
    const patch = [...diffHeader(diff.path), `@@ -${diff.start},${olds} +${diff.start},${news} @@`, ...diff.rows.map((row) => `${row.type}${row.text}`), ''].join('\n');
    // Unchanged files are the same objects, as the pane keeps them.
    let file = this.parsed.get(patch);
    if (!file) {
      file = parseReviewFiles(patch)[0];
      this.parsed.set(patch, file);
    }
    return file;
  }

  private conflictFile(): { file: ReviewFile; folds: Map<number, readonly string[]> } {
    const c = this.model.conflict;
    const source = [...c.lead, '<<<<<<< ours', 'needle ours', '=======', '>>>>>>> theirs needle'].join('\n');
    const key = `${source}\u0000${[...c.expanded].sort().join(',')}`;
    let built = this.conflicts.get(key);
    if (!built) {
      const folds = new Map<number, readonly string[]>();
      const file = reviewFileFromPatchFile(conflictPatchFile(c.path, source, { expandedFolds: new Set(c.expanded), onFold: (foldId, lines) => folds.set(foldId, lines) }));
      built = { file, folds };
      this.conflicts.set(key, built);
    }
    return built;
  }

  content(): ReviewFindContent {
    const m = this.model;
    const conflict = this.conflictFile();
    const byPath = new Map<string, ReviewFile>([...m.diffs.map((d) => [d.path, this.diffFile(d)] as const), [m.conflict.path, conflict.file]]);
    const closed = new Set(m.closed);
    return {
      files: m.order.filter((path) => !m.hiddenPaths.has(path)).map((path) => byPath.get(path)!),
      drafts: m.drafts.map((d) => ({ ...d })),
      prThreads: m.threads.map((t) => ({ ...t, comments: t.comments.map((c) => ({ ...c })) })),
      commentText: (body, rowKey) => body.split('|').map((text, index) => ({ text, shown: !(closed.has(rowKey) && index % 2 === 1) })),
      editing: new Set(m.editing),
      foldLines: (path) => (path === m.conflict.path ? conflict.folds : null),
    };
  }

  /** Applies one random change; answers what it touched. */
  change(): { name: string; touched: Touched } {
    const m = this.model;
    const none: Touched = () => false;
    const lineIn = (path: string, tags: ReadonlySet<string>): Touched => (hit) => hit.path === path && hit.at.kind === 'line' && tags.has(hit.at.tag);
    const kind = Math.floor(this.next() * 15);
    if (kind === 0 || kind === 1) {
      // A line's text replaced; its number kept.
      const diff = this.pick(m.diffs);
      const r = Math.floor(this.next() * diff.rows.length);
      const tag = this.tags(diff)[r];
      diff.rows[r] = { ...diff.rows[r], text: this.text() };
      return { name: `replace ${diff.path}:${tag}`, touched: lineIn(diff.path, new Set([tag])) };
    }
    if (kind === 2) {
      // A line removed (or added): the lines below are renumbered.
      const diff = this.pick(m.diffs);
      const r = Math.floor(this.next() * diff.rows.length);
      const below = new Set(this.tags(diff).slice(r));
      if (diff.rows.length > 1 && this.chance(0.5)) diff.rows.splice(r, 1);
      else diff.rows.splice(r, 0, this.row());
      return { name: `renumber ${diff.path} from row ${r}`, touched: lineIn(diff.path, below) };
    }
    if (kind === 3) {
      const thread = this.pick(m.threads);
      if (!thread.comments.length) return { name: 'no-op', touched: none };
      const index = Math.floor(this.next() * thread.comments.length);
      const key = reviewCommentKey(thread.comments[index]);
      thread.comments[index] = { ...thread.comments[index], body: this.body() };
      return { name: `edit comment ${thread.id}/${key}`, touched: (hit) => hit.at.kind === 'thread' && hit.at.threadId === thread.id && hit.at.comment === key };
    }
    if (kind === 4) {
      const thread = this.pick(m.threads);
      if (!thread.comments.length) return { name: 'no-op', touched: none };
      const index = Math.floor(this.next() * thread.comments.length);
      const [gone] = thread.comments.splice(index, 1);
      return { name: `delete comment ${thread.id}/${reviewCommentKey(gone)}`, touched: none };
    }
    if (kind === 5) {
      const thread = this.pick(m.threads);
      thread.comments.splice(Math.floor(this.next() * (thread.comments.length + 1)), 0, this.comment());
      return { name: `insert comment in ${thread.id}`, touched: none };
    }
    if (kind === 6) {
      const thread = this.pick(m.threads);
      const comment = thread.comments.length ? this.pick(thread.comments) : null;
      if (!comment) return { name: 'no-op', touched: none };
      const rowKey = findThreadCommentKey(thread.id, reviewCommentKey(comment));
      if (!m.closed.delete(rowKey)) m.closed.add(rowKey);
      return { name: `toggle details ${rowKey}`, touched: none };
    }
    if (kind === 7) {
      if (!m.drafts.length) return { name: 'no-op', touched: none };
      const draft = this.pick(m.drafts);
      const rowKey = `dc:${draft.id}`;
      if (!m.editing.delete(rowKey)) m.editing.add(rowKey);
      return { name: `toggle editor ${rowKey}`, touched: none };
    }
    if (kind === 8) {
      if (!m.drafts.length) return { name: 'no-op', touched: none };
      const draft = this.pick(m.drafts);
      draft.body = this.text();
      return { name: `edit draft ${draft.id}`, touched: (hit) => hit.at.kind === 'draft' && hit.at.commentId === draft.id };
    }
    if (kind === 9) {
      const path = this.pick(m.order);
      if (!m.hiddenPaths.delete(path)) m.hiddenPaths.add(path);
      return { name: `toggle filter ${path}`, touched: none };
    }
    if (kind === 10) {
      m.order.reverse();
      return { name: 'reorder files', touched: none };
    }
    if (kind === 11) {
      // A fold opened or closed.
      const folds = [...this.conflictFile().folds.keys(), ...m.conflict.expanded];
      if (!folds.length) return { name: 'no-op', touched: none };
      const foldId = this.pick(folds);
      if (!m.conflict.expanded.delete(foldId)) m.conflict.expanded.add(foldId);
      return { name: `toggle fold ${foldId}`, touched: none };
    }
    if (kind === 12) {
      // A conflict file line, maybe in a fold, rewritten.
      const index = Math.floor(this.next() * m.conflict.lead.length);
      m.conflict.lead[index] = this.text();
      return { name: `rewrite c.go:${index + 1}`, touched: lineIn(m.conflict.path, new Set([`c${index + 1}:${index + 1}`])) };
    }
    if (kind === 13) {
      // A refreshed thread anchored in the other file.
      const thread = this.pick(m.threads);
      const from = thread.path;
      thread.path = m.diffs.find((d) => d.path !== from)!.path;
      return { name: `move thread ${thread.id} from ${from}`, touched: (hit) => hit.at.kind === 'thread' && hit.at.threadId === thread.id };
    }
    // A draft added or deleted.
    if (this.chance(0.5) && m.drafts.length) {
      const [gone] = m.drafts.splice(Math.floor(this.next() * m.drafts.length), 1);
      return { name: `delete draft ${gone.id}`, touched: none };
    }
    m.drafts.push(this.draft());
    return { name: 'add draft', touched: none };
  }
}

// The text of the run a scanned match is in, read from where the scan
// found it, and whether that run is shown.
function runText(content: ReviewFindContent, scanned: Scanned): string {
  const { hit, at: where } = scanned;
  const file = content.files[where.fileIndex];
  const at = hit.at;
  if (at.kind === 'header') return file.path;
  if (at.kind === 'draft') return content.drafts.find((d) => d.id === at.commentId)!.body;
  if (at.kind === 'thread') {
    // The comment its identity names, by forge id.
    const comment = content.prThreads.find((t) => t.id === at.threadId)?.comments.find((c) => reviewCommentKey(c) === at.comment);
    if (!comment) throw new Error(`${JSON.stringify(hit.identity)} names no comment of its thread by its forge key`);
    return content.commentText(visibleBody(comment.body), where.rowKey)[hit.run].text;
  }
  const place = where.place;
  if (place.kind === 'fold') return content.foldLines!(file.path)!.get(place.foldId)![scanned.foldLine!];
  const raw = file.body.text((place as { lineIndex: number }).lineIndex);
  return at.tag.startsWith('m') ? raw : raw.slice(1);
}

// Whether `hit`'s element and run are shown in `content` at all.
function runShown(content: ReviewFindContent, hit: ReviewFindHit): boolean {
  if (!content.files.some((file) => file.path === hit.path)) return false;
  const at = hit.at;
  if (at.kind === 'draft') return !content.editing?.has(`dc:${at.commentId}`) && content.drafts.some((d) => d.id === at.commentId);
  if (at.kind !== 'thread') return true;
  const comment = content.prThreads.find((t) => t.id === at.threadId)?.comments.find((c) => reviewCommentKey(c) === at.comment);
  return !!comment && content.commentText(visibleBody(comment.body), findThreadCommentKey(at.threadId, at.comment))[hit.run]?.shown === true;
}

const SEEDS = 120;
const STEPS = 12;

describe('review find identity and resolution, over random changes', () => {
  // Invariant 1: identity is stable. A change that does not touch a match's
  // element source leaves its identity, and the identity still names the
  // same text: a fresh scan of the new content yields it, in a run whose
  // text is the run it was found in.
  it('a match whose element source a change did not touch keeps its identity and its text', async () => {
    const checked = { header: 0, line: 0, fold: 0, draft: 0, thread: 0 };
    for (let seed = 1; seed <= SEEDS; seed += 1) {
      const world = new World(seed);
      let content = world.content();
      let before = await freshScan(content, QUERY);
      for (let step = 0; step < STEPS; step += 1) {
        const { name, touched } = world.change();
        const after = world.content();
        const fresh = await freshScan(after, QUERY);
        for (const scanned of before.values()) {
          const was = runText(content, scanned);
          if (touched(scanned.hit) || !runShown(after, scanned.hit)) continue;
          const label = `seed ${seed} step ${step} (${name}) ${JSON.stringify(scanned.hit.identity)}`;
          const now = fresh.get(scanned.hit.identity);
          expect(now, label).toBeDefined();
          expect(runText(after, now!), label).toBe(was);
          checked[scanned.at.place.kind === 'fold' ? 'fold' : scanned.hit.at.kind] += 1;
        }
        content = after;
        before = fresh;
      }
    }
    // Every kind of element is exercised in volume.
    expect(checked.line).toBeGreaterThan(20_000);
    expect(checked.thread).toBeGreaterThan(10_000);
    expect(checked.draft).toBeGreaterThan(1500);
    expect(checked.header).toBeGreaterThan(600);
    expect(checked.fold).toBeGreaterThan(10_000);
  });

  // Invariant 2: the resolver agrees with a fresh scan. Between a change
  // and the recount's swap, every published identity resolves against the
  // new content to what a fresh scan of it says: the same element, the
  // same painter's index and place, or absent.
  it('resolving a published match against changed content answers what a fresh scan of it does', async () => {
    const outcomes = { present: 0, absent: 0 };
    for (let seed = 1; seed <= SEEDS; seed += 1) {
      const world = new World(seed);
      let published = [...(await seededScan(world.content())).values()].map((each) => each.hit);
      for (let step = 0; step < STEPS; step += 1) {
        // One to three changes land before the recount swaps.
        const names: string[] = [];
        for (let n = 1 + (step % 3); n > 0; n -= 1) names.push(world.change().name);
        const after = world.content();
        const resolved = await expectResolvesTo(published, after, `seed ${seed} step ${step} (${names.join(', ')})`);
        for (const each of resolved.values()) outcomes[each.kind === 'present' ? 'present' : 'absent'] += 1;
        published = [...(await seededScan(after)).values()].map((each) => each.hit);
      }
    }
    // Both answers are exercised in volume.
    expect(outcomes.present).toBeGreaterThan(50_000);
    expect(outcomes.absent).toBeGreaterThan(3000);
  });
});
