import { beforeEach, describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import { createThreadMessageUiState } from './threadMessageUiState.svelte';
import { __resetUnsendDeclinesForTest, isUnsendDeclined, noteUnsendDeclined } from './unsendDeclines.svelte';

beforeEach(() => __resetUnsendDeclinesForTest());

function fixture(threadId: string | null = 't-1') {
  const provisional = makeItem({ id: 'optimistic:send', kind: 'user_text', meta: '{"sendId":"send"}' });
  const confirmed = { ...provisional, id: 'user:send' };
  const items = new Map([[provisional.id, provisional]]);
  const ui = createThreadMessageUiState({ getItemById: id => items.get(id), loadedItems: () => items.values(), threadId: () => threadId });
  const confirm = () => { items.delete(provisional.id); items.set(confirmed.id, confirmed); ui.disposeItems([provisional]); };
  return { provisional, confirmed, items, ui, confirm };
}

const preview = (url: string) => ({ id: 'image', filename: 'image.png', mimeType: 'image/png', url, width: 0, height: 0, originalBytes: 1, menuTag: {} });

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

  it('keeps a declined un-send across confirmation, retention and a thread switch, for its thread only', () => {
    const { provisional, confirmed, ui, confirm } = fixture();
    ui.setUnsendDeclined(provisional.id);
    ui.setUnsendDeclined(provisional.id);
    confirm();
    expect(ui.isUnsendDeclined(confirmed.id)).toBe(true);
    ui.prune(new Set());
    ui.clear();
    expect(ui.isUnsendDeclined(confirmed.id)).toBe(true);
    // A pane that loads the thread again reads the same fact.
    const returned = fixture();
    returned.confirm();
    expect(returned.ui.isUnsendDeclined(returned.confirmed.id)).toBe(true);
    expect(fixture('t-2').ui.isUnsendDeclined(confirmed.id)).toBe(false);
    const draft = fixture(null);
    draft.ui.setUnsendDeclined(draft.provisional.id);
    expect(draft.ui.isUnsendDeclined(draft.provisional.id)).toBe(false);
  });

  it('bounds the declines it keeps, dropping the least recently declined thread first', () => {
    for (let i = 0; i < 64; i++) noteUnsendDeclined(`t-${i}`, 'send:a');
    noteUnsendDeclined('t-0', 'send:b');
    noteUnsendDeclined('t-64', 'send:a');
    expect(isUnsendDeclined('t-0', 'send:a')).toBe(true);
    expect(isUnsendDeclined('t-1', 'send:a')).toBe(false);
    expect(isUnsendDeclined('t-64', 'send:a')).toBe(true);
    for (let i = 0; i < 65; i++) noteUnsendDeclined('t-0', `send:${i}`);
    expect(isUnsendDeclined('t-0', 'send:0')).toBe(false);
    expect(isUnsendDeclined('t-0', 'send:64')).toBe(true);
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
