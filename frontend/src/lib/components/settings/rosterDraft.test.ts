import { describe, expect, it } from 'vitest';
import { draftOf, markSaved, reconcile, takeTheirs, unsaved } from './rosterDraft';

type Note = { text: string };
const trim = (note: Note): Note => ({ text: note.text.trim() });
const values = (draft: { entries: { value: Note }[] }) => draft.entries.map((entry) => entry.value.text);

describe('roster draft', () => {
  it('follows the saved list where nothing is edited here', () => {
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }]);
    const next = reconcile(draft, [{ text: 'a' }, { text: 'B' }, { text: 'c' }], trim);
    expect(values(next)).toEqual(['a', 'B', 'c']);
    expect(next.entries.every((entry) => entry.theirs === undefined)).toBe(true);
  });

  it('keeps an edited entry in place of the one it was edited from, and an added one', () => {
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }]);
    draft.entries[1].value.text = 'b, edited';
    draft.entries.push({ value: { text: 'new' }, origin: null });
    const next = reconcile(draft, [{ text: 'A' }, { text: 'b' }], trim);
    expect(values(next)).toEqual(['A', 'b, edited', 'new']);
    expect(next.entries.every((entry) => entry.theirs === undefined)).toBe(true);
  });

  it('treats an edit that only adds surrounding space as no edit', () => {
    const draft = draftOf<Note>([{ text: 'a' }]);
    draft.entries[0].value.text = 'a ';
    expect(values(reconcile(draft, [{ text: 'A' }], trim))).toEqual(['A']);
  });

  it('leaves out an entry deleted here, unless it changed elsewhere', () => {
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }]);
    const [gone] = draft.entries.splice(0, 1);
    draft.removed.push(gone.origin!);
    expect(values(reconcile(draft, [{ text: 'a' }, { text: 'b' }, { text: 'c' }], trim))).toEqual(['b', 'c']);
    expect(values(reconcile(draft, [{ text: 'a, changed there' }, { text: 'b' }], trim))).toEqual(['a, changed there', 'b']);
  });

  it('marks an entry changed in both places with its counterpart, or with none', () => {
    const byFirstLetter = (origin: Note, entry: Note) => origin.text[0] === entry.text[0];
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }]);
    draft.entries[0].value.text = 'a, mine';
    draft.entries[1].value.text = 'b, mine';
    const next = reconcile(draft, [{ text: 'a, theirs' }, { text: 'x' }], trim, byFirstLetter);
    expect(values(next)).toEqual(['a, mine', 'x', 'b, mine']);
    expect(next.entries.map((entry) => entry.theirs)).toEqual([{ text: 'a, theirs' }, undefined, null]);

    takeTheirs(next, 2);
    takeTheirs(next, 0);
    expect(values(next)).toEqual(['a, theirs', 'x']);
    expect(next.entries.every((entry) => entry.theirs === undefined)).toBe(true);
  });

  it('clears marks and pending deletions once saved', () => {
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }]);
    draft.entries[0].value.text = 'mine';
    const marked = reconcile(draft, [{ text: 'theirs' }, { text: 'b' }], trim, () => true);
    marked.removed.push({ text: 'b' });
    const sent = marked.entries.map((entry) => trim(entry.value));
    markSaved(marked, [...marked.entries], sent, [...marked.removed]);
    expect(marked.entries.map((entry) => entry.theirs)).toEqual([undefined, undefined]);
    expect(marked.removed).toEqual([]);
    expect(values(reconcile(marked, sent, trim))).toEqual(['mine', 'b']);
  });

  // A save's answer lands after the person went on editing: only what the
  // save took becomes saved.
  it('leaves what changed after the save took its snapshot unsaved', () => {
    const draft = draftOf<Note>([{ text: 'a' }, { text: 'b' }, { text: 'c' }]);
    const entries = [...draft.entries];
    const sent = entries.map((entry) => trim(entry.value));
    const [deleted] = draft.entries.splice(2, 1);
    draft.removed.push(deleted.origin!);
    draft.entries.push({ value: { text: 'added' }, origin: null });
    draft.entries[0].value.text = 'typed after';
    markSaved(draft, entries, sent, []);
    expect(draft.removed).toEqual([{ text: 'c' }]);
    expect(draft.entries.map((entry) => entry.origin?.text ?? null)).toEqual(['a', 'b', null]);
    expect(unsaved(draft, trim)).toBe(true);
  });
});
