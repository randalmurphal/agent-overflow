import { afterEach, expect, it, vi } from 'vitest';
import { isPendingFlushRow, isReaderAuthoredUserText } from './userMessageMeta';
import type { Item } from '../types/models';

const row = (meta: string | undefined, overrides: Partial<Item> = {}): Item => ({
  id: 'user:1', threadId: 't', kind: 'user_text', turnIndex: 1, itemIndex: 0, createdAt: 0, summary: 'hi',
  meta, ...overrides,
} as Item);

afterEach(() => vi.restoreAllMocks());

it('answers from the row meta', () => {
  expect(isPendingFlushRow(row('{"pendingFlush":true}'))).toBe(true);
  expect(isPendingFlushRow(row('{"pendingFlush":"true"}'))).toBe(false);
  expect(isPendingFlushRow(row('{"sendId":"s"}'))).toBe(false);
  expect(isPendingFlushRow(row(undefined))).toBe(false);
  expect(isPendingFlushRow(row('not json'))).toBe(false);
  expect(isPendingFlushRow(row('{"pendingFlush":true}', { kind: 'assistant_text' }))).toBe(false);

  expect(isReaderAuthoredUserText(row('{"sendId":"s"}'))).toBe(true);
  expect(isReaderAuthoredUserText(row(undefined))).toBe(true);
  expect(isReaderAuthoredUserText(row('{"wire_only":true}'))).toBe(false);
  expect(isReaderAuthoredUserText(row('{"sendId":"s"}', { parentId: 'agent' }))).toBe(false);
  expect(isReaderAuthoredUserText(row('{"sendId":"s"}', { kind: 'assistant_text' }))).toBe(false);
});

it('parses a row once for both questions while its meta is the same', () => {
  const parse = vi.spyOn(JSON, 'parse');
  const item = row('{"pendingFlush":true,"wire_only":true}');
  expect(isPendingFlushRow(item)).toBe(true);
  expect(isReaderAuthoredUserText(item)).toBe(false);
  expect(isPendingFlushRow(item)).toBe(true);
  expect(parse).toHaveBeenCalledTimes(1);

  // Another row with the same meta is its own row.
  expect(isPendingFlushRow(row('{"pendingFlush":true,"wire_only":true}'))).toBe(true);
  expect(parse).toHaveBeenCalledTimes(2);
});

it('answers again when the same row object gets a new meta', () => {
  const item = row('{"pendingFlush":true}');
  expect(isPendingFlushRow(item)).toBe(true);
  expect(isReaderAuthoredUserText(item)).toBe(true);
  item.meta = '{"wire_only":true}';
  expect(isPendingFlushRow(item)).toBe(false);
  expect(isReaderAuthoredUserText(item)).toBe(false);
  item.meta = undefined;
  expect(isReaderAuthoredUserText(item)).toBe(true);
});
