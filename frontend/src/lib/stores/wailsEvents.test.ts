// The event hub's delivery contract: the payload, and the connection it
// arrived on. Driven through the runtime mock, which stamps the origin
// the same way the production shim does.
import { describe, expect, it, vi } from 'vitest';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { wailsEventOn } from './wailsEvents';

const BACKEND = '62c8a1de-0a3f-4f4b-9d0a-2b6b1a5b0f11';

describe('wailsEventOn', () => {
  it('unwraps the payload and names the connection it arrived on', () => {
    setBackendIdentityFromBootstrap(BACKEND, 'gen-1');
    const handler = vi.fn();
    const off = wailsEventOn<{ id: string }>('thread:updated', handler);

    emitWailsEvent('thread:updated', { id: 'thread-a' });

    expect(handler).toHaveBeenCalledWith({ id: 'thread-a' }, { backendId: BACKEND });
    off();
    emitWailsEvent('thread:updated', { id: 'thread-b' });
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it('reports an unknown origin rather than assuming the attached backend', () => {
    const handler = vi.fn();
    const off = wailsEventOn('thread:updated', handler);

    emitWailsEvent('thread:updated', { id: 'thread-a' });

    // No identity: a subscriber must not read the missing stamp as "the
    // backend I am attached to".
    expect(handler).toHaveBeenCalledWith({ id: 'thread-a' }, { backendId: '' });
    off();
  });
});
