import { expect, it } from 'vitest';
import { MAX_CATALOG_ROWS, makeCatalogRecord, type CatalogRowCache } from './catalog';
import type { Thread } from '../types/models';

const thread = (id: string, title = `title-${id}`): Thread => ({
  id, title, provider: 'claude', workspacePath: '/w', projectPath: '/w', mode: 'chat', model: 'm',
  createdAt: 1, updatedAt: 1, archived: false,
});

/** A row that counts how often its title is read, which every encode does. */
function counted(id: string, title = `title-${id}`): { row: Thread; reads: () => number } {
  let reads = 0;
  const row = thread(id);
  Object.defineProperty(row, 'title', { enumerable: true, get: () => { reads++; return title; } });
  return { row, reads: () => reads };
}

it('encodes a row once for as long as it is the same object', () => {
  const cache: CatalogRowCache = new WeakMap();
  const kept = counted('kept');
  const first = makeCatalogRecord('g', 'threads', [kept.row, thread('other')], 's', cache);
  const readsAfterFirst = kept.reads();
  expect(readsAfterFirst).toBeGreaterThan(0);

  const second = makeCatalogRecord('g', 'threads', [kept.row, thread('other', 'renamed')], 's', cache);
  expect(kept.reads()).toBe(readsAfterFirst);
  expect(first.rows).toEqual([thread('kept'), thread('other')]);
  expect(second.rows).toEqual([thread('kept'), thread('other', 'renamed')]);
});

it('encodes every row on every write without a cache', () => {
  const row = counted('each');
  makeCatalogRecord('g', 'threads', [row.row], 's');
  const readsAfterFirst = row.reads();
  makeCatalogRecord('g', 'threads', [row.row], 's');
  expect(row.reads()).toBe(readsAfterFirst * 2);
});

it('remembers a row the catalog leaves out', () => {
  const cache: CatalogRowCache = new WeakMap();
  const oversized = counted('big', 'x'.repeat(65537));
  const invalid = { ...thread('bad'), provider: 'other' } as unknown as Thread;
  expect(makeCatalogRecord('g', 'threads', [oversized.row, invalid, thread('ok')], 's', cache).rows).toEqual([thread('ok')]);
  const readsAfterFirst = oversized.reads();
  expect(makeCatalogRecord('g', 'threads', [oversized.row, invalid, thread('ok')], 's', cache).rows).toEqual([thread('ok')]);
  expect(oversized.reads()).toBe(readsAfterFirst);
});

it('holds remembered rows to the row and size limits', () => {
  const cache: CatalogRowCache = new WeakMap();
  const many = Array.from({ length: MAX_CATALOG_ROWS + 5 }, (_, i) => thread(`t${i}`));
  expect(makeCatalogRecord('g', 'threads', many, 's', cache).rows).toHaveLength(MAX_CATALOG_ROWS);
  expect(makeCatalogRecord('g', 'threads', many, 's', cache).rows).toHaveLength(MAX_CATALOG_ROWS);

  // 80 rows of 60,000 characters pass the catalog's 4 MB before they end.
  const large = Array.from({ length: 80 }, (_, i) => thread(`l${i}`, 'y'.repeat(60_000)));
  const first = makeCatalogRecord('g', 'threads', large, 's', cache).rows.length;
  expect(first).toBeGreaterThan(0);
  expect(first).toBeLessThan(80);
  expect(makeCatalogRecord('g', 'threads', large, 's', cache).rows).toHaveLength(first);
});
