import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { setPageGrantsFromBootstrap } from '../transport/scopes';
import { openInEditor } from './openInEditor';

describe('openInEditor', () => {
  beforeEach(() => {
    resetBindingMocks();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
    resetBindingMocks();
  });

  it('forwards every argument in a local session', async () => {
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    await openInEditor('src/main.ts', 12, 4, '/repo', 'cursor');

    expect(open).toHaveBeenCalledWith('src/main.ts', 12, 4, '/repo', 'cursor');
  });

  it('rejects before the RPC across on-host to elsewhere to on-host transitions', async () => {
    const open = setBindingMock('OpenInEditor', vi.fn(async () => undefined));

    await openInEditor('first.ts', 0, 0, '/repo', '');
    setPageGrantsFromBootstrap(true);
    await expect(openInEditor('remote.ts', 0, 0, '/repo', '')).rejects.toThrow(
      'Opening files in an editor needs the app running on this computer.',
    );
    setPageGrantsFromBootstrap(false);
    await openInEditor('last.ts', 0, 0, '/repo', '');

    expect(open.mock.calls).toEqual([
      ['first.ts', 0, 0, '/repo', ''],
      ['last.ts', 0, 0, '/repo', ''],
    ]);
  });
});
