import { describe, expect, it } from 'vitest';
import { draftPlaceholderId, draftPlaceholderProjectId } from './draftPlaceholderId';

const PROJECT = '5b0c1e7a-2f3d-4c8e-9a61-0d4f7b2e8c13';

describe('draft placeholder ids', () => {
  it.each(['main', 'pane-3', 'side-chat-pane-2', 'restored:pane', ''])('round-trips the project through pane id %j', (paneId) => {
    for (const mode of ['chat', 'plan'] as const) {
      const id = draftPlaceholderId(paneId, PROJECT, mode);
      // The backend accepts placeholder ids by this prefix.
      expect(id.startsWith('draft:')).toBe(true);
      expect(draftPlaceholderProjectId(id)).toBe(PROJECT);
    }
  });

  it('mints a fresh id for each placeholder', () => {
    expect(draftPlaceholderId('main', PROJECT, 'chat')).not.toBe(draftPlaceholderId('main', PROJECT, 'chat'));
  });

  it.each([
    PROJECT,
    `pane-2:${PROJECT}:chat:0e6c2d8f-7a14-4b39-8c55-3f9a1d2b6e70`,
    'draft:',
    `draft:${PROJECT}`,
    `draft:${PROJECT}:chat:0e6c2d8f-7a14-4b39-8c55-3f9a1d2b6e70`,
  ])('names no project for %j', (threadId) => {
    expect(draftPlaceholderProjectId(threadId)).toBeUndefined();
  });
});
