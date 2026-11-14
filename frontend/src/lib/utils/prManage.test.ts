// The management controls' pure rules (prManage.ts): which controls a
// detail's flags and state offer, the picker's selection as one change,
// the wire shape that passes the backend's objects through, and the
// search query bound.

import { describe, expect, it } from 'vitest';
import {
  canBypass,
  canEditRow,
  canRerequest,
  dismissibleChangeRequests,
  labelChange,
  methodHasCommitMessage,
  peopleChange,
  pickerQuery,
  prStateMenuItems,
  selectionChange,
} from './prManage';
import { prDetailFixture } from '../../test/helpers/reviewPanePR';
import type { PRDetail, PRManageCan } from '../types/models';

const NONE: PRManageCan = {
  editTitle: false,
  editDescription: false,
  reviewers: false,
  assignees: false,
  labels: false,
  close: false,
  reopen: false,
  convertToDraft: false,
  rerequest: false,
  dismiss: false,
  bypass: false,
  reviewerTeams: false,
};

function detail(can: Partial<PRManageCan>, overrides: Partial<PRDetail> = {}): PRDetail {
  return prDetailFixture({ manage: { reviewers: [], assignees: [], labels: [], can: { ...NONE, ...can } }, ...overrides });
}

describe('prStateMenuItems', () => {
  it('offers exactly what the flags allow on an open PR', () => {
    expect(prStateMenuItems(detail({ close: true, convertToDraft: true }))).toEqual(['draft', 'close']);
    expect(prStateMenuItems(detail({ close: true }))).toEqual(['close']);
    expect(prStateMenuItems(detail({}))).toEqual([]);
  });

  it('offers Reopen on a closed PR and no Convert to draft on a draft', () => {
    expect(prStateMenuItems(detail({ reopen: true }, { state: 'closed' }))).toEqual(['reopen']);
    expect(prStateMenuItems(detail({ close: true, convertToDraft: true }, { draft: true }))).toEqual(['close']);
  });

  it('offers nothing on a merged PR, or from a backend without the flags', () => {
    expect(prStateMenuItems(detail({ close: true, reopen: true, convertToDraft: true }, { state: 'MERGED' }))).toEqual([]);
    expect(prStateMenuItems(prDetailFixture())).toEqual([]);
  });
});

describe('row and review controls', () => {
  it('decides each row on its own flag', () => {
    const d = detail({ reviewers: true, labels: true });
    expect(canEditRow(d, 'reviewers')).toBe(true);
    expect(canEditRow(d, 'assignees')).toBe(false);
    expect(canEditRow(d, 'labels')).toBe(true);
    expect(canEditRow(prDetailFixture(), 'labels')).toBe(false);
  });

  it('re-requests a settled or outdated review, never a pending one or a team', () => {
    const d = detail({ rerequest: true });
    expect(canRerequest(d, { login: 'a', state: 'COMMENTED' })).toBe(true);
    expect(canRerequest(d, { login: 'a', state: 'APPROVED' })).toBe(true);
    expect(canRerequest(d, { login: 'a', state: 'PENDING' })).toBe(false);
    expect(canRerequest(d, { login: 'a', state: 'PENDING', stale: true })).toBe(true);
    expect(canRerequest(d, { login: 'org/team', state: 'COMMENTED', team: true })).toBe(false);
    expect(canRerequest(detail({}), { login: 'a', state: 'COMMENTED' })).toBe(false);
  });

  it('offers Dismiss per change request and Bypass only with both the blocker and the flag', () => {
    const blocker = { kind: 'changes_requested', text: 'Changes requested', changeRequests: [{ reviewId: 'R1', authorLogin: 'k' }], bypassable: true };
    expect(dismissibleChangeRequests(detail({ dismiss: true }), blocker)).toHaveLength(1);
    expect(dismissibleChangeRequests(detail({}), blocker)).toHaveLength(0);
    expect(dismissibleChangeRequests(detail({ dismiss: true }), { ...blocker, kind: 'approvals' })).toHaveLength(0);
    expect(canBypass(detail({ bypass: true }), blocker)).toBe(true);
    expect(canBypass(detail({}), blocker)).toBe(false);
    expect(canBypass(detail({ bypass: true }), { ...blocker, bypassable: false })).toBe(false);
  });
});

describe('selectionChange', () => {
  it('is the adds and removes against what the row showed', () => {
    const initial = new Map([['a', 'A'], ['b', 'B']]);
    const selected = new Map([['b', 'B'], ['c', 'C']]);
    expect(selectionChange(initial, selected)).toEqual({ add: ['C'], remove: ['A'] });
  });

  it('is empty when nothing changed', () => {
    const initial = new Map([['a', 'A']]);
    expect(selectionChange(initial, new Map(initial))).toEqual({ add: [], remove: [] });
  });
});

describe('the wire shape', () => {
  it('passes people and labels through as the backend gave them, a reviewer without its state', () => {
    const reviewer = { login: 'k', name: 'Kim', id: '7', state: 'CHANGES_REQUESTED', stale: true, future: 'kept' } as never;
    expect(peopleChange({ add: [], remove: [reviewer] })).toEqual({ add: [], remove: [{ login: 'k', name: 'Kim', id: '7', future: 'kept' }] });
    const label = { name: 'bug', id: 'LA_1', color: '#d73a4a' };
    expect(labelChange({ add: [label], remove: [] }).add[0]).toEqual(label);
  });
});

describe('pickerQuery', () => {
  it('is one trimmed line of at most 100 bytes, cut at a character', () => {
    expect(pickerQuery('  jane\ndoe ')).toBe('jane doe');
    expect(new TextEncoder().encode(pickerQuery('é'.repeat(80))).length).toBe(100);
    expect(pickerQuery('é'.repeat(80))).toBe('é'.repeat(50));
  });
});

describe('methodHasCommitMessage', () => {
  it('is merge and squash, never rebase', () => {
    expect(methodHasCommitMessage('merge')).toBe(true);
    expect(methodHasCommitMessage('squash')).toBe(true);
    expect(methodHasCommitMessage('rebase')).toBe(false);
  });
});
