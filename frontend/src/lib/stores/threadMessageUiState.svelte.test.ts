import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { createThreadMessageUiState } from './threadMessageUiState.svelte';

function fixture() {
  const provisional = makeItem({ id: 'optimistic:send', kind: 'user_text', meta: '{"sendId":"send"}' });
  const confirmed = { ...provisional, id: 'user:send' };
  const items = new Map([[provisional.id, provisional]]);
  const ui = createThreadMessageUiState({ getItemById: id => items.get(id), loadedItems: () => items.values() });
  const confirm = () => { items.delete(provisional.id); items.set(confirmed.id, confirmed); ui.disposeItems([provisional]); };
  return { provisional, confirmed, items, ui, confirm };
}

const preview = (url: string) => ({ id: 'image', filename: 'image.png', mimeType: 'image/png', size: 1, url });

describe('message presentation lifetime', () => {
  it('preserves expansion and previews across confirmation, then clears them on pruning', () => {
    const { provisional, confirmed, ui, confirm } = fixture();
    const revoke = vi.spyOn(URL, 'revokeObjectURL');
    const cache = ui.attachmentCacheFor(provisional.id);
    cache.set('image', preview('blob:original'));
    ui.setExpanded(provisional.id, true);
    const signature = ui.expansionSignature();
    confirm();
    ui.prune(new Set([confirmed.id]));
    expect(ui.isExpanded(confirmed.id)).toBe(true);
    expect(ui.expansionSignature()).toBe(signature);
    expect(ui.attachmentCacheFor(confirmed.id).get('image')?.url).toBe('blob:original');
    expect(revoke).not.toHaveBeenCalled();
    ui.prune(new Set());
    expect(ui.isExpanded(confirmed.id)).toBe(false);
    expect(cache.get('image')).toBeUndefined();
    expect(revoke).toHaveBeenCalledWith('blob:original');
    const fresh = ui.attachmentCacheFor(confirmed.id);
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
    const { provisional, confirmed, ui, confirm, items } = fixture();
    ui.setExpanded(provisional.id, true);
    confirm();
    ui.setExpanded(confirmed.id, false);
    expect(ui.isExpanded(confirmed.id)).toBe(false);
    ui.setExpanded(confirmed.id, true);
    const other = { ...confirmed, id: 'user:other', threadId: 'other-thread' };
    items.set(other.id, other);
    expect(ui.isExpanded(other.id)).toBe(false);
    items.delete(confirmed.id);
    ui.disposeItems([confirmed]);
    items.set(confirmed.id, confirmed);
    expect(ui.isExpanded(confirmed.id)).toBe(false);
  });

  it('keeps borrowed previews alive until disposal and revokes every owned URL once', () => {
    const { ui, provisional } = fixture();
    const revoke = vi.spyOn(URL, 'revokeObjectURL');
    const cache = ui.attachmentCacheFor(provisional.id);
    cache.set('image', preview('blob:old'));
    cache.set('image', preview('blob:new'));
    cache.set('image', preview('blob:new'));
    expect(revoke).not.toHaveBeenCalled();
    ui.clear();
    expect(revoke.mock.calls).toEqual([['blob:old'], ['blob:new']]);
    revoke.mockRestore();
  });
  it('does not confuse backend ids with send keys or ambiguous expansion lists', () => {
    const { provisional, items, ui } = fixture();
    const collision = makeItem({ id: 'send:["thread-1","send"]' });
    items.set(collision.id, collision);
    ui.setExpanded(provisional.id, true);
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
