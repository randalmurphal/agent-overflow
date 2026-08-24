import { beforeEach, describe, expect, it } from 'vitest';
import {
  beginThreadInterrupt,
  finishThreadInterrupt,
  isThreadInterruptPending,
  resetThreadInterruptStateForTest,
} from './threadInterruptState.svelte';

describe('threadInterruptState', () => {
  beforeEach(() => resetThreadInterruptStateForTest());

  it('allows one interrupt transaction per thread', () => {
    const token = beginThreadInterrupt('thread-a');
    expect(token).not.toBeNull();
    expect(beginThreadInterrupt('thread-a')).toBeNull();
    expect(beginThreadInterrupt('thread-b')).not.toBeNull();
    expect(isThreadInterruptPending('thread-a')).toBe(true);
    expect(isThreadInterruptPending('thread-b')).toBe(true);
  });

  it('does not let an older completion clear a newer transaction', () => {
    const first = beginThreadInterrupt('thread-a')!;
    finishThreadInterrupt('thread-a', first);
    const second = beginThreadInterrupt('thread-a')!;

    finishThreadInterrupt('thread-a', first);
    expect(isThreadInterruptPending('thread-a')).toBe(true);

    finishThreadInterrupt('thread-a', second);
    expect(isThreadInterruptPending('thread-a')).toBe(false);
  });
});
