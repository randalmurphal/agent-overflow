import { describe, expect, it } from 'vitest';
import { collectGarbage } from '../../test/helpers/gc';
import { HOME_BACKEND } from '../transport/backendKey';
import { beginCatalogRead, openCatalogReadsForTest, recordCatalogMutation } from './computerCatalogReads';

describe('an ended catalog read', () => {
  it('holds none of the mutations it recorded, even while the read itself is still referenced', async () => {
    const read = beginCatalogRead<{ id: string }>(HOME_BACKEND, 'threads');
    let captured: { id: string } | null = { id: 'captured' };
    const ref = new WeakRef(captured);
    recordCatalogMutation<{ id: string }>(HOME_BACKEND, 'threads', ((row) => (rows) => [...rows, row])(captured));
    captured = null;
    expect(read.replay([]).map((row) => row.id)).toEqual(['captured']);
    read.end();
    expect(openCatalogReadsForTest(HOME_BACKEND, 'threads')).toBe(0);
    await collectGarbage();
    expect(ref.deref()).toBeUndefined();
    expect(() => read.replay([])).toThrow(/after it ended/);
  });
});
