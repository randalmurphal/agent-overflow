// The PR-owned draft bindings (ListPRDiffReviewComments and its siblings)
// over one in-memory table, as the backend's diff_review_comments rows
// under a PR owner, keyed here by PR number.

import { vi } from 'vitest';
import { setBindingMock } from '../mocks/bindings-app';
import { takePinnedBackend } from '../../lib/transport/backends';
import type { DiffReviewComment, DiffReviewCommentInput } from '../../lib/types/models';

export interface PRDraftsFake {
  /** Every comment the backend holds, by PR number. */
  held: Map<number, DiffReviewComment[]>;
  /** The backend each call was routed to. */
  routed: string[];
  create: ReturnType<typeof vi.fn>;
  /** SubmitPRReview: posts the PR's drafts it names and marks them sent,
   * answering a repeated submit id from its record. */
  submit: ReturnType<typeof vi.fn>;
  send: ReturnType<typeof vi.fn>;
}

type Wire = { Number: number };

export function installPRDraftsFake(): PRDraftsFake {
  const held = new Map<number, DiffReviewComment[]>();
  const routed: string[] = [];
  let next = 0;
  const rows = (wire: Wire) => {
    routed.push(String(takePinnedBackend() ?? ''));
    let list = held.get(wire.Number);
    if (!list) {
      list = [];
      held.set(wire.Number, list);
    }
    return list;
  };
  setBindingMock('ListPRDiffReviewComments', async (wire: Wire, scope: string, sourceKey: string) =>
    rows(wire).filter((comment) => comment.scope === scope && comment.sourceKey === sourceKey).map((comment) => ({ ...comment })));
  const create = setBindingMock('CreatePRDiffReviewComment', async (wire: Wire, input: DiffReviewCommentInput) => {
    next += 1;
    const comment = {
      id: `c${next}`, threadId: '', status: 'draft', createdAt: next, updatedAt: next,
      ...input,
      commitSha: input.commitSha ?? '',
    } as DiffReviewComment;
    rows(wire).push(comment);
    return { ...comment };
  });
  setBindingMock('UpdatePRDiffReviewComment', async (wire: Wire, id: string, update: { body: string }) => {
    const comment = rows(wire).find((candidate) => candidate.id === id);
    if (!comment) throw new Error('The draft comment no longer exists.');
    comment.body = update.body;
    return { ...comment };
  });
  setBindingMock('DeletePRDiffReviewComment', async (wire: Wire, id: string) => {
    const list = rows(wire);
    const at = list.findIndex((candidate) => candidate.id === id);
    if (at >= 0) list.splice(at, 1);
  });
  const settle = (wire: Wire, ids: readonly string[]) => {
    for (const comment of rows(wire)) if (ids.includes(comment.id)) comment.status = 'sent';
  };
  const submits = new Map<string, unknown>();
  const submit = setBindingMock('SubmitPRReview', async (wire: Wire, input: { submitId: string; drafts: { threadId?: string; commentIds: string[] } }) => {
    if (submits.has(input.submitId)) return submits.get(input.submitId);
    const result = { postedReview: true, postedFileComments: 0 };
    if (!input.drafts.threadId) settle(wire, input.drafts.commentIds);
    submits.set(input.submitId, result);
    return result;
  });
  const send = setBindingMock('SendPRDiffReviewComments', async (wire: Wire, threadId: string, _scope: string, _source: string, ids: string[]) => {
    settle(wire, ids);
    return { id: threadId };
  });
  return { held, routed, create, submit, send };
}
