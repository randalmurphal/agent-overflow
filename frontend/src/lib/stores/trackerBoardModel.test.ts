// The patch rule replayed from the run the Go model recorded
// (internal/trackerapp/testdata/board_patch_cases.json, written by
// TestPatchCasesMatchTheModel, which checks every step against reading
// the board again). Each step's held board is digested in the text both
// sides write (heldCanon in internal/trackerapp/patch_model_test.go), so
// a step this port applies differently fails here with its index.
import { describe, expect, it } from 'vitest';
import cases from '../../../../internal/trackerapp/testdata/board_patch_cases.json';
import type {
  BoardPatch,
  BoardView,
  CardView,
  RowView,
  SectionPage,
  SectionTotal,
} from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  appendPage,
  applyBoardPatch,
  cardMatches,
  createRowInterner,
  heldFromView,
  heldSections,
  heldTotalsAt,
  screenColumns,
  sectionKey,
  searchWords,
  type HeldBoard,
} from './trackerBoardModel';

interface Step {
  ids?: string[];
  patch?: BoardPatch;
  page?: number;
  rows?: SectionPage;
  expect: string;
}

const recorded = cases as unknown as { sections: unknown[]; view: BoardView; steps: Step[]; final: string };

function num(v: number): string {
  return String(v);
}

function tot(t: SectionTotal): string {
  return `${t.rows}/${num(t.estimate)}`;
}

/** heldCanon's text, written the way the Go side writes it. */
function canon(h: HeldBoard): string {
  let out = `totals all ${tot(h.totals.all)} backlog ${tot(h.totals.backlog)} sprints`;
  for (const id of [...h.totals.sprints.keys()].sort()) {
    const t = h.totals.sprints.get(id)!;
    if (t.rows > 0) out += ` ${id}=${tot(t)}`;
  }
  out += ' columns';
  for (const i of [...h.totals.columns.keys()].sort((a, b) => a - b)) {
    const t = h.totals.columns.get(i)!;
    if (t.rows > 0) out += ` ${i}=${tot(t)}`;
  }
  out += '\n';
  for (const s of h.sections) {
    out += `${s.section.kind} ${s.section.sprint ?? ''} ${s.section.column ?? 0} `;
    out += s.next ? `next ${s.next.order} ${s.next.issue} ${s.next.lifetime} ${s.next.revision}\n` : 'whole\n';
    for (const r of s.rows) out += `${r.card.id} ${r.order} ${r.estimate == null ? '-' : num(r.estimate)} ${r.card.title}\n`;
  }
  return out;
}

/** The 32-bit FNV-1a of the text's UTF-8 bytes, in hex, as Go's hash/fnv. */
function digest(text: string): string {
  let h = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(text)) {
    h ^= byte;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h.toString(16).padStart(8, '0');
}

describe('the patch rule, replayed from the Go model', () => {
  it('holds after every recorded step what the Go reference holds', () => {
    const intern = createRowInterner();
    let held = heldFromView(structuredClone(recorded.view), intern);
    expect(heldSections(held)).toEqual(recorded.sections);
    expect(recorded.steps.some((s) => s.page !== undefined), 'the run loads a further page').toBe(true);
    expect(recorded.steps.some((s) => s.patch?.totals), 'the run has a patch with totals').toBe(true);
    expect(recorded.steps.some((s) => s.patch && !s.patch.totals), 'the run has a patch for the totals held').toBe(true);
    recorded.steps.forEach((step, i) => {
      if (step.page !== undefined) held = appendPage(held, step.page, structuredClone(step.rows!), intern);
      else held = applyBoardPatch(held, step.ids!, structuredClone(step.patch!), intern);
      expect(digest(canon(held)), `step ${i}:\n${canon(held)}`).toBe(step.expect);
    });
    expect(canon(held)).toBe(recorded.final);
  });

  it('keeps the arrays and rows a patch does not touch', () => {
    const intern = createRowInterner();
    const held = heldFromView(structuredClone(recorded.view), intern);
    const step = recorded.steps.find((s) => s.patch && s.patch.sections.some((p) => (p.rows ?? []).length === 0))!;
    const next = applyBoardPatch(held, step.ids!, structuredClone(step.patch!), intern);
    const named = new Set(step.ids);
    next.sections.forEach((sec, i) => {
      const before = held.sections[i];
      const touched = before.rows.some((r) => named.has(r.card.id)) || (step.patch!.sections[i].rows ?? []).length > 0;
      if (!touched) expect(sec.rows).toBe(before.rows);
      for (const r of sec.rows) {
        if (!named.has(r.card.id)) expect(before.rows).toContain(r);
      }
    });
  });

  it('names the totals it holds and refuses a patch of other sections', () => {
    const held = heldFromView(structuredClone(recorded.view), createRowInterner());
    expect(heldTotalsAt(held)).toEqual({ lifetime: recorded.view.lifetime, revision: recorded.view.totals_revision });
    const patch = structuredClone(recorded.steps.find((s) => s.patch)!.patch!);
    patch.sections = patch.sections.slice(1);
    expect(() => applyBoardPatch(held, [], patch, createRowInterner())).toThrow(/answered 5 sections for the 6 asked/);
  });
});

function row(id: string, order: string, status: string, extra: Partial<CardView> = {}): RowView {
  return { order, estimate: 2, card: { id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: status }, created: 0, updated: 0, ...extra } } as RowView;
}

function sprintBoard(rowsBySprint: Record<string, RowView[]>, more: string[] = [], unread: string[] = []): HeldBoard {
  const columns = [
    { name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: 0, estimate: 0 } },
    { name: 'In Progress', statuses: [{ id: 's2', group: 'in_progress' }], total: { rows: 0, estimate: 0 } },
  ];
  const view = {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: true, estimate: { unit: 'points' },
    columns, sprints: [...Object.keys(rowsBySprint), ...unread].map((id) => ({ id, name: id, state: 'active', total: { rows: 0, estimate: 0 } })),
    all: { rows: 0, estimate: 0 }, backlog: { rows: 0, estimate: 0 }, sync: {},
    screen: [...Object.keys(rowsBySprint), ...unread].map((id) => ({ kind: 'sprint', sprint: id })),
    sections: Object.entries(rowsBySprint).map(([id, rows]) => ({
      section: { kind: 'sprint', sprint: id }, rows,
      next: more.includes(id) ? { order: 'z', issue: 'z', lifetime: 1, revision: 1 } : null,
    })),
  } as unknown as BoardView;
  return heldFromView(view, (r) => r);
}

describe('the Board screen', () => {
  it('keys a section the same whether the wire names its first column or leaves it out', () => {
    expect(sectionKey({ kind: 'column', column: 0 })).toBe(sectionKey({ kind: 'column' }));
    expect(sectionKey({ kind: 'sprint', sprint: '8' })).not.toBe(sectionKey({ kind: 'sprint', sprint: '9' }));
  });

  it('groups sprint rows into columns by status, merges sprints in board order and leaves out an unmapped status', () => {
    const held = sprintBoard({
      '8': [row('1', 'a', 's1'), row('2', 'c', 's2'), row('3', 'd', 'gone')],
      '9': [row('4', 'b', 's1')],
    });
    const cols = screenColumns(held);
    expect(cols.map((c) => c.rows.map((r) => r.card.id))).toEqual([['1', '4'], ['2']]);
    expect(cols.map((c) => c.total)).toEqual([{ rows: 2, estimate: 4 }, { rows: 1, estimate: 2 }]);
    expect(cols.map((c) => c.statusGroups)).toEqual([['todo'], ['in_progress']]);
    expect(cols.every((c) => c.moreFrom === null)).toBe(true);
    // Unchanged columns keep their arrays across a rebuild.
    expect(screenColumns(held, cols)[0].rows).toBe(cols[0].rows);
  });

  it('has no column total while a sprint is still loading, and names the section to load', () => {
    const cols = screenColumns(sprintBoard({ '8': [row('1', 'a', 's1')] }, ['8']));
    expect(cols.map((c) => [c.total, c.moreFrom])).toEqual([[null, 0], [null, 0]]);
  });

  it('holds a screen section the view did not carry as unread until its first page, and keeps a patch off it', () => {
    const held = sprintBoard({ '8': [row('1', 'a', 's1')] }, [], ['9']);
    expect(held.sections.map((s) => [s.section, s.rows.length, s.unread ?? false])).toEqual([
      [{ kind: 'sprint', sprint: '8' }, 1, false],
      [{ kind: 'sprint', sprint: '9' }, 0, true],
    ]);
    expect(screenColumns(held).map((c) => [c.total, c.moreFrom, c.unread])).toEqual([[null, 1, false], [null, 1, false]]);
    expect(screenColumns(sprintBoard({}, [], ['9'])).map((c) => c.unread)).toEqual([true, true]);

    const patch = { lifetime: 1, revision: 2, sections: [{ rows: [] }, { rows: [row('2', 'b', 's1')] }] } as unknown as BoardPatch;
    const patched = applyBoardPatch(held, ['2'], patch, (r) => r);
    expect(patched.sections[1]).toBe(held.sections[1]);

    const read = appendPage(patched, 1, { section: { kind: 'sprint', sprint: '9' }, rows: [row('2', 'b', 's1')], next: null } as SectionPage, (r) => r);
    expect(read.sections[1]).toEqual({ section: { kind: 'sprint', sprint: '9' }, rows: [row('2', 'b', 's1')], next: null });
    expect(screenColumns(read).map((c) => [c.rows.map((r) => r.card.id), c.moreFrom, c.unread])).toEqual([[['1', '2'], null, false], [[], null, false]]);
  });
});

describe('Only mine and Search', () => {
  const card = row('1', 'a', 's1', {
    title: 'Export skips rows', assignee: { id: 'acc-user1', name: 'User 1' },
    labels: [{ id: 'bug', name: 'bug' }], parent: { id: '9', key: 'WEB-9', title: 'Import reliability' },
  }).card;

  it('keeps the cards assigned to the account and those holding every word', () => {
    expect(cardMatches(card, { mine: 'acc-user1', words: [] })).toBe(true);
    expect(cardMatches(card, { mine: 'acc-user2', words: [] })).toBe(false);
    expect(cardMatches(card, { mine: null, words: searchWords('  EXPORT   reliability ') })).toBe(true);
    expect(cardMatches(card, { mine: null, words: searchWords('web-1 bug user') })).toBe(true);
    expect(cardMatches(card, { mine: null, words: searchWords('export missing') })).toBe(false);
  });
});

describe('the row interner', () => {
  it('shares equal parts by content and keeps a renamed one apart', () => {
    const intern = createRowInterner();
    const a = intern(row('1', 'a', 's1', { assignee: { id: 'u', name: 'User 1' }, parent: { id: '9', title: 'Epic' } }));
    const b = intern(row('2', 'b', 's1', { assignee: { id: 'u', name: 'User 1' }, parent: { id: '9', title: 'Epic' } }));
    const c = intern(row('3', 'c', 's1', { assignee: { id: 'u', name: 'User 1' }, parent: { id: '9', title: 'Epic, renamed' } }));
    expect(b.card.assignee).toBe(a.card.assignee);
    expect(b.card.status).toBe(a.card.status);
    expect(b.card.parent).toBe(a.card.parent);
    expect(c.card.parent).not.toBe(a.card.parent);
    expect(c.card.parent?.title).toBe('Epic, renamed');
  });
});
