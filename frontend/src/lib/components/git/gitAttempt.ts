// Git writes whose answer a client can lose: commit, push, pull and create
// PR. Each call carries a fresh attempt id. When the answer is lost (the
// connection dropped, or the wait ran out), the computer is asked what that
// attempt did (GetGitActionOutcome), which waits for one still running, so
// a write that landed reads as landed. A repeat of the id never writes
// again: the computer answers it from the first arrival.

import { GetGitActionOutcome } from '../../stores/bindings';
import { isTransportClassError } from '../../stores/transportStatus.svelte';
import { userFacingError } from '../../utils/userFacingError';
import { randomId } from '../../utils/randomId';
import type { GitActionOutcome, GitActionResult, WorkspaceRef } from '../../types/git';

/** The git writes the computer records attempts of (GitActionResult.action). */
export type GitAttemptAction = 'commit' | 'push' | 'pull' | 'pr';

export type GitAttemptAnswer =
  /** The write answered, directly or through its recorded outcome. */
  | { kind: 'done'; result: GitActionResult }
  /** The write failed; `message` is its own error. */
  | { kind: 'failed'; message: string; error?: unknown }
  /** The answer was lost and the computer could not be asked yet: the write
   * may have landed. Ask again with `attemptId` (askGitAttempt). */
  | { kind: 'lost'; attemptId: string; message: string; error: unknown }
  /** The computer has no record of the attempt: it never arrived, or the
   * computer restarted or no longer keeps it. */
  | { kind: 'unknown' };

/** Asks what attempt `attemptId` of `action` in `workspace` did. */
export async function askGitAttempt(
  workspace: WorkspaceRef,
  action: GitAttemptAction,
  attemptId: string,
): Promise<GitAttemptAnswer> {
  let outcome: GitActionOutcome;
  try {
    outcome = await GetGitActionOutcome(workspace, action, attemptId) as GitActionOutcome;
  } catch (err) {
    return { kind: 'lost', attemptId, message: userFacingError(err), error: err };
  }
  switch (outcome.state) {
    case 'succeeded':
      return outcome.result ? { kind: 'done', result: outcome.result } : { kind: 'unknown' };
    case 'failed':
      return { kind: 'failed', message: outcome.message || `The ${action} failed.` };
    default:
      return { kind: 'unknown' };
  }
}

/** Runs one attempt of a git write; `write` makes the call with the attempt
 * id. A lost answer is settled by asking for the attempt's outcome. */
export async function runGitAttempt(
  workspace: WorkspaceRef,
  action: GitAttemptAction,
  write: (attemptId: string) => Promise<unknown>,
): Promise<GitAttemptAnswer> {
  const attemptId = randomId();
  let result: GitActionResult;
  try {
    result = await write(attemptId) as GitActionResult;
  } catch (err) {
    if (isTransportClassError(err)) return askGitAttempt(workspace, action, attemptId);
    return { kind: 'failed', message: err instanceof Error ? err.message : String(err), error: err };
  }
  if (result.error) return { kind: 'failed', message: result.error };
  return { kind: 'done', result };
}

/** What a dialog says while a write's answer is lost and its outcome cannot
 * be asked for yet; pressing `press` again asks. */
export function lostAnswerAskAgainMessage(noun: string, press: string, answer: { message: string }): string {
  return `The connection dropped before the ${noun} answered, so it may have landed. Press ${press} to ask the computer what happened. ${answer.message}`;
}

/** What a toast says when a write's answer was lost and its outcome cannot
 * be asked for. */
export function lostAnswerMessage(noun: string, answer: { message: string }): string {
  return `The connection dropped before the ${noun} answered, so it may have landed; the git status shows whether it did. ${answer.message}`;
}

/** What a toast says when the computer has no record of a lost write. */
export function unknownAttemptMessage(noun: string): string {
  return `The computer has no record of this ${noun}: it never arrived, or the computer restarted since. The git status shows whether it landed.`;
}
