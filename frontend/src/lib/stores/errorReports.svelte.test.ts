import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { captureError, capturedErrorText, resetErrorReportsForTest } from './errorReports.svelte';
import { TransportError } from '../transport/wsClient';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetScopesForTest, setCarriedSessionScopes, setPageGrantsFromBootstrap } from '../transport/scopes';
import { getPinnedBackend } from '../transport/backends';

function backendError(chain: string[], backend = ''): TransportError {
  return new TransportError('method_error', 'failed', {
    detail: { ref: 'rKVz', method: 'ForkThread', at: 1, chain },
    backend,
  });
}

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

describe('captureError', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetErrorReportsForTest();
    setPageGrantsFromBootstrap(false);
  });
  afterEach(() => {
    resetErrorReportsForTest();
    __resetScopesForTest();
  });

  it('reads the backend log when the error is captured', async () => {
    setBindingMock('Version', async () => '1.2.3');
    const read = setBindingMock('GetErrorLogLines', async () => ({ lines: ['before', 'failed (id: rKVz)'], found: true }));
    const captured = captureError(backendError(['fork: copy session']), { context: 'Could not fork' });
    expect(capturedErrorText(captured)).toContain('Backend log: still being read when this was copied');

    await settle();
    expect(read).toHaveBeenCalledWith('rKVz');
    const text = capturedErrorText(captured);
    expect(text).toContain('```\nbefore\nfailed (id: rKVz)\n```');
    expect(text).toContain('- app: 1.2.3');
  });

  it('reads the log from the backend that answered, with its grant', async () => {
    setBindingMock('Version', async () => '1.2.3');
    const targets: (string | null)[] = [];
    const read = setBindingMock('GetErrorLogLines', async () => {
      targets.push(getPinnedBackend());
      return { lines: ['office: failed (id: rKVz)'], found: true };
    });
    setCarriedSessionScopes('b-office', ['threads:operate']);
    setCarriedSessionScopes('b-viewer', ['threads:read']);

    const office = captureError(backendError(['fork: copy session'], 'b-office'));
    const viewer = captureError(backendError(['fork: copy session'], 'b-viewer'));
    const clientSide = captureError(new Error('disk full'));
    await settle();

    expect(read).toHaveBeenCalledTimes(1);
    expect(targets).toEqual(['b-office']);
    expect(capturedErrorText(office)).toContain('office: failed (id: rKVz)');
    expect(capturedErrorText(viewer)).toContain("on the backend's computer, under ref rKVz");
    expect(capturedErrorText(clientSide)).not.toContain('Backend log');
  });

  it('reports a failed log read and leaves out a version it could not read', async () => {
    setBindingMock('Version', async () => { throw new Error('offline'); });
    setBindingMock('GetErrorLogLines', async () => { throw new TransportError('scope_required', 'not granted'); });
    const captured = captureError(backendError(['fork: copy session']));
    await settle();
    const text = capturedErrorText(captured);
    expect(text).toMatch(/Backend log: could not be read \(.+\)/);
    expect(text).not.toContain('- app:');
  });

  it('does not ask for the version a session is not granted', async () => {
    __resetScopesForTest();
    setPageGrantsFromBootstrap(true);
    const version = setBindingMock('Version', async () => '1.2.3');
    const captured = captureError(new Error('disk full'));
    await settle();
    expect(version).not.toHaveBeenCalled();
    expect(capturedErrorText(captured)).not.toContain('- app:');
  });
});
