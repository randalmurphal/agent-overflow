import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { createThreadMessageUiState } from './threadMessageUiState.svelte';

function fixture() {
  const earlier = makeItem({ id: 'user:earlier', kind: 'user_text', meta: '{"sendId":"send"}' });
  const renamed = { ...earlier, id: 'user:send' };
  const items = new Map([[earlier.id, earlier]]);
  const ui = createThreadMessageUiState({ getItemById: id => items.get(id), loadedItems: () => items.values() });
  const rename = () => { items.delete(earlier.id); items.set(renamed.id, renamed); ui.disposeItems([earlier]); };
  return { earlier, renamed, items, ui, rename };
}

const preview = (url: string) => ({ id: 'image', filename: 'image.png', mimeType: 'image/png', url, width: 0, height: 0, originalBytes: 1, menuTag: {} });

describe('message presentation lifetime', () => {
  it('preserves expansion and previews when the send’s row changes id, then clears them on pruning', () => {
    const { earlier, renamed, ui, rename } = fixture();
    const revoke = vi.spyOn(URL, 'revokeObjectURL');
    const cache = ui.attachmentCacheFor(earlier.id);
    cache.set('image', preview('blob:original'));
    ui.setExpanded(earlier.id, true);
    const signature = ui.expansionSignature();
    rename();
    ui.prune(new Set([renamed.id]));
    expect(ui.isExpanded(renamed.id)).toBe(true);
    expect(ui.expansionSignature()).toBe(signature);
    expect(ui.attachmentCacheFor(renamed.id).get('image')?.url).toBe('blob:original');
    expect(revoke).not.toHaveBeenCalled();
    ui.prune(new Set());
    expect(ui.isExpanded(renamed.id)).toBe(false);
    expect(cache.get('image')).toBeUndefined();
    expect(revoke).toHaveBeenCalledWith('blob:original');
    const fresh = ui.attachmentCacheFor(renamed.id);
    cache.set('image', preview('blob:late'));
    expect(revoke).toHaveBeenCalledWith('blob:late');
    expect(fresh.get('image')).toBeUndefined();
    fresh.set('image', preview('blob:new'));
    ui.clear();
    ui.clear();
    expect(revoke.mock.calls.filter(([url]) => url === 'blob:new')).toHaveLength(1);
    revoke.mockRestore();
  });

  it('collapses and expands through either representation without carrying state to another thread', () => {
    const { earlier, renamed, ui, rename, items } = fixture();
    ui.setExpanded(earlier.id, true);
    rename();
    ui.setExpanded(renamed.id, false);
    expect(ui.isExpanded(renamed.id)).toBe(false);
    ui.setExpanded(renamed.id, true);
    const other = { ...renamed, id: 'user:other', threadId: 'other-thread' };
    items.set(other.id, other);
    expect(ui.isExpanded(other.id)).toBe(false);
    items.delete(renamed.id);
    ui.disposeItems([renamed]);
    items.set(renamed.id, renamed);
    expect(ui.isExpanded(renamed.id)).toBe(false);
  });

  it('keeps borrowed previews alive until disposal and revokes every owned URL once', () => {
    const { ui, earlier } = fixture();
    const revoke = vi.spyOn(URL, 'revokeObjectURL');
    const cache = ui.attachmentCacheFor(earlier.id);
    cache.set('image', preview('blob:old'));
    cache.set('image', preview('blob:new'));
    cache.set('image', preview('blob:new'));
    expect(revoke).not.toHaveBeenCalled();
    ui.clear();
    expect(revoke.mock.calls).toEqual([['blob:old'], ['blob:new']]);
    revoke.mockRestore();
  });
  it('does not confuse backend ids with send keys or ambiguous expansion lists', () => {
    const { earlier, items, ui } = fixture();
    const collision = makeItem({ id: 'send:["thread-1","send"]' });
    items.set(collision.id, collision);
    ui.setExpanded(earlier.id, true);
    expect(ui.isExpanded(collision.id)).toBe(false);
    ui.clear();
    ui.setExpanded('a,item:b', true);
    const oneItem = ui.expansionSignature();
    ui.clear();
    ui.setExpanded('a', true);
    ui.setExpanded('b', true);
    expect(ui.expansionSignature()).not.toBe(oneItem);
  });

});
