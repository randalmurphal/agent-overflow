import { describe, expect, it, beforeEach } from 'vitest';
import {
  getThreadStatus,
  projectApprovalRequest,
  projectApprovalResolution,
  projectThreadItem,
  resetForTest,
} from './threadStatuses.svelte';
import { makeItem } from '../../test/helpers/chat';

describe('threadStatuses store', () => {
  beforeEach(() => {
    resetForTest();
  });

  it('tracks multiple active items before returning to idle', () => {
    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));

    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'completed',
      isBackground: false,
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('lets pending approvals dominate and resolves by requestId fallback', () => {
    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'streaming',
    }));
    expect(getThreadStatus('thread-1')).toBe('running');

    projectApprovalRequest('thread-1', 'req-1');
    expect(getThreadStatus('thread-1')).toBe('pending-approval');

    projectApprovalResolution(undefined, 'req-1');
    expect(getThreadStatus('thread-1')).toBe('running');

    projectThreadItem(makeItem({
      id: 'text-1',
      kind: 'assistant_text',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');
  });

  it('holds error until a new turn signal supersedes it', () => {
    projectThreadItem(makeItem({
      id: 'error-1',
      kind: 'error',
      role: 'system',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('error');

    projectThreadItem(makeItem({
      id: 'user-1',
      kind: 'user_text',
      role: 'user',
      status: 'completed',
    }));
    expect(getThreadStatus('thread-1')).toBe('idle');

    projectThreadItem(makeItem({
      id: 'tool-1',
      kind: 'tool_call',
      status: 'running',
      isBackground: false,
    }));
    expect(getThreadStatus('thread-1')).toBe('running');
  });
});
