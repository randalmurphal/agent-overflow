import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { deriveCompletionStatus } from './toolCompletionStatus';

function makeItem(overrides: Partial<Item>): Item {
  return {
    id: 'i1',
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

describe('deriveCompletionStatus', () => {
  it('returns null while running or streaming so the caller can render its existing affordance', () => {
    expect(deriveCompletionStatus(makeItem({ status: 'running' }))).toBeNull();
    expect(deriveCompletionStatus(makeItem({ status: 'streaming' }))).toBeNull();
  });

  it('returns success for a clean inline tool_call completion', () => {
    expect(deriveCompletionStatus(makeItem({ status: 'completed' }))).toBe('success');
  });

  it('collapses errored/killed/declined into failure', () => {
    expect(deriveCompletionStatus(makeItem({ status: 'errored' }))).toBe('failure');
    expect(deriveCompletionStatus(makeItem({ status: 'killed' }))).toBe('failure');
    expect(deriveCompletionStatus(makeItem({ status: 'declined' }))).toBe('failure');
  });

  it('treats status=completed with is_error=true as failure (Claude inline error shape)', () => {
    const item = makeItem({
      status: 'completed',
      payloadMeta: JSON.stringify({ is_error: true }),
    });
    expect(deriveCompletionStatus(item)).toBe('failure');
  });

  it('treats status=completed with isError=true as failure (camelCase header shape)', () => {
    const item = makeItem({
      status: 'completed',
      payloadMeta: JSON.stringify({ isError: true }),
    });
    expect(deriveCompletionStatus(item)).toBe('failure');
  });

  it('treats non-zero exitCode as failure even when status=completed', () => {
    const item = makeItem({
      status: 'completed',
      payloadMeta: JSON.stringify({ exitCode: 7 }),
    });
    expect(deriveCompletionStatus(item)).toBe('failure');
  });

  it('treats non-zero exit_code (snake_case) as failure', () => {
    const item = makeItem({
      status: 'completed',
      payloadMeta: JSON.stringify({ exit_code: 1 }),
    });
    expect(deriveCompletionStatus(item)).toBe('failure');
  });

  it('treats exit code 0 as success', () => {
    const item = makeItem({
      status: 'completed',
      payloadMeta: JSON.stringify({ exitCode: 0 }),
    });
    expect(deriveCompletionStatus(item)).toBe('success');
  });

  it('ignores garbage payload meta and falls back to status alone', () => {
    expect(deriveCompletionStatus(makeItem({ status: 'completed', payloadMeta: 'not json' }))).toBe('success');
    expect(deriveCompletionStatus(makeItem({ status: 'completed', payloadMeta: '"not an object"' }))).toBe('success');
  });

  it('accepts a pre-parsed meta input and skips re-parsing payloadMeta', () => {
    const item = makeItem({ status: 'completed', payloadMeta: JSON.stringify({ exitCode: 0 }) });
    expect(deriveCompletionStatus(item, { meta: { exitCode: 5 } })).toBe('failure');
    expect(deriveCompletionStatus(item, { meta: null })).toBe('success');
  });

  describe('backgrounded launch rows are stable', () => {
    it('returns null for a backgrounded tool_call regardless of running status', () => {
      const item = makeItem({ kind: 'tool_call', isBackground: true, status: 'running' });
      expect(deriveCompletionStatus(item)).toBeNull();
    });

    it('returns null for a backgrounded tool_call even when status flips to completed', () => {
      // Per spec the launch row stays running, but defensively: even if a
      // bug or future change flips the launch row to completed, the helper
      // must NOT produce a badge — completion belongs on the sibling row.
      const item = makeItem({ kind: 'tool_call', isBackground: true, status: 'completed' });
      expect(deriveCompletionStatus(item)).toBeNull();
    });

    it('returns null for a backgrounded tool_call even when status is errored', () => {
      const item = makeItem({ kind: 'tool_call', isBackground: true, status: 'errored' });
      expect(deriveCompletionStatus(item)).toBeNull();
    });

    it('returns success for a tool_completion sibling carrying isBackground=true (the row that DOES get the badge)', () => {
      const item = makeItem({ kind: 'tool_completion', isBackground: true, status: 'completed' });
      expect(deriveCompletionStatus(item)).toBe('success');
    });

    it('returns failure for a tool_completion sibling with errored status', () => {
      const item = makeItem({ kind: 'tool_completion', isBackground: true, status: 'errored' });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });

    it('returns failure when a tool_completion sibling carries non-zero exit_code (snake_case from Codex background)', () => {
      // Real Codex backgrounded exec_command shape — the launch row
      // stays running with `…`, then a sibling tool_completion lands
      // carrying the exit code in payload meta. The badge must reflect
      // it.
      const item = makeItem({
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        payloadMeta: JSON.stringify({ exit_code: 137 }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });

    it('returns failure when a tool_completion sibling carries is_error=true (Claude task failure)', () => {
      const item = makeItem({
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        payloadMeta: JSON.stringify({ is_error: true }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });
  });

  describe('precedence', () => {
    it('terminal failure status beats meta exit_code=0', () => {
      const item = makeItem({
        status: 'errored',
        payloadMeta: JSON.stringify({ exitCode: 0 }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });

    it('is_error=true beats absent exit code', () => {
      const item = makeItem({
        status: 'completed',
        payloadMeta: JSON.stringify({ is_error: true }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });

    it('non-zero exit code wins regardless of is_error=false', () => {
      // Pins that we don't read is_error=false as a positive "all
      // good" signal that overrides exitCode. Only is_error=true
      // contributes; the absence/falsity of the flag falls through
      // to the exit-code check.
      const item = makeItem({
        status: 'completed',
        payloadMeta: JSON.stringify({ is_error: false, exitCode: 7 }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });

    it('is_error=true overrides exitCode=0 (failure beats clean exit)', () => {
      // Mirror of the above: when status=completed and exitCode=0 but
      // is_error=true, the row still reads as failure. Pins the
      // is_error→failure branch independent of the exit code path.
      const item = makeItem({
        status: 'completed',
        payloadMeta: JSON.stringify({ is_error: true, exitCode: 0 }),
      });
      expect(deriveCompletionStatus(item)).toBe('failure');
    });
  });
});
