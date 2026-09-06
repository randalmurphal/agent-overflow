import { beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';

const state = vi.hoisted(() => ({
  allowed: true, capable: true,
  owner: { status: { status: 'connected' } } as { status: { status: string } } | undefined,
  target: '', opened: vi.fn(),
}));
vi.mock('../transport/scopes', () => ({ hasScope: (_: string, key: string) => state.allowed && key === 'gpu' }));
vi.mock('./transportStatus.svelte', () => ({ getTransportHelloFor: (key: string) => ({ capabilities: key === 'gpu' && state.capable ? ['preview.files.v1'] : [] }) }));
vi.mock('../transport/backends', () => ({
  backendById: (key: string) => key === 'gpu' ? state.owner : undefined,
  withBackendTarget: (key: string, call: () => unknown) => { state.target = key; return call(); },
}));
vi.mock('../utils/externalLinks', () => ({ handleExternalURL: state.opened }));

import { canPreviewFiles, openFilePreview } from './filePreviews';

describe('computer-owned HTML previews', () => {
  beforeEach(() => {
    resetBindingMocks();
    state.allowed = state.capable = true;
    state.owner = { status: { status: 'connected' } };
    state.target = '';
    state.opened.mockReset();
  });

  it('pins identical filesystem paths to the owning computer', async () => {
    const mint = setBindingMock('MintFilePreviewURL', vi.fn(async () => 'https://gpu:50000/index.html?ao_preview=ticket'));
    await openFilePreview('gpu', 'index.html', '/repo');
    expect(state.target).toBe('gpu');
    expect(mint).toHaveBeenCalledWith('index.html', '/repo');
    expect(state.opened).toHaveBeenCalledWith('https://gpu:50000/index.html?ao_preview=ticket');
  });

  it('does not offer unsupported or ungranted previews', () => {
    expect(canPreviewFiles('gpu')).toBe(true);
    expect(canPreviewFiles('')).toBe(false);
    state.capable = false;
    expect(canPreviewFiles('gpu')).toBe(false);
    state.capable = true;
    state.allowed = false;
    expect(canPreviewFiles('gpu')).toBe(false);
  });

  it('refuses a missing or offline computer before sending', async () => {
    const mint = setBindingMock('MintFilePreviewURL', vi.fn());
    state.owner = undefined;
    await expect(openFilePreview('gpu', 'index.html', '/repo')).rejects.toThrow('Reconnect');
    state.owner = { status: { status: 'reconnecting' } };
    await expect(openFilePreview('gpu', 'index.html', '/repo')).rejects.toThrow('Reconnect');
    expect(mint).not.toHaveBeenCalled();
  });

  it('ignores a completed mint from a forgotten and re-paired computer', async () => {
    setBindingMock('MintFilePreviewURL', vi.fn(async () => {
      state.owner = { status: { status: 'connected' } };
      return 'https://old:50000/';
    }));
    await expect(openFilePreview('gpu', 'index.html', '/repo')).rejects.toThrow('connection changed');
    expect(state.opened).not.toHaveBeenCalled();
  });
});
