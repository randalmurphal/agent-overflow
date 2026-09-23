import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fileSaveAction, savedFileMessage } from './fileSaveAction';

const nativeShell = vi.hoisted(() => ({ value: false }));
const webviewHosted = vi.hoisted(() => ({ value: false }));
const hostScope = vi.hoisted(() => ({ value: false }));

vi.mock('../native/platform', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../native/platform')>()),
  isNativeShell: () => nativeShell.value,
}));
vi.mock('../transport/pageHost', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../transport/pageHost')>()),
  isWebviewHosted: () => webviewHosted.value,
}));
vi.mock('../transport/scopes', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../transport/scopes')>()),
  hasScope: (scope: string) => scope === 'host' && hostScope.value,
}));
vi.mock('../stores/attachedBackends.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../stores/attachedBackends.svelte')>()),
  attachedBackendEntry: (key: string) => (key === 'gpu' ? { id: 'gpu', name: 'gpu-box' } : undefined),
  backendDisplayName: (entry: { name: string }) => entry.name,
}));

const BROWSER_URL = 'https://gitlab.example.test/group/widget/uploads/0123/report.pdf';

describe('deciding what saving a file does on this page', () => {
  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    hostScope.value = false;
  });

  it('saves into Downloads when the page runs on the owning computer', () => {
    hostScope.value = true;
    expect(fileSaveAction('gpu', BROWSER_URL)).toBe('save-here');
    expect(fileSaveAction('gpu', null)).toBe('save-here');
    // The desktop webview on the host is still the host.
    webviewHosted.value = true;
    expect(fileSaveAction('gpu', null)).toBe('save-here');
  });

  it('opens the browser URL from the phone shell even when a grant claims host', () => {
    nativeShell.value = true;
    hostScope.value = true;
    expect(fileSaveAction('gpu', BROWSER_URL)).toBe('open-externally');
  });

  it("opens the browser URL from the desktop webview showing another computer's file", () => {
    webviewHosted.value = true;
    expect(fileSaveAction('gpu', BROWSER_URL)).toBe('open-externally');
  });

  it('saves on the owning computer from a webview when there is no URL to open', () => {
    webviewHosted.value = true;
    expect(fileSaveAction('gpu', null)).toBe('save-there');
    webviewHosted.value = false;
    nativeShell.value = true;
    expect(fileSaveAction('gpu', null)).toBe('save-there');
    hostScope.value = true;
    expect(fileSaveAction('gpu', null)).toBe('save-there');
  });

  it('downloads in a connected browser', () => {
    expect(fileSaveAction('gpu', BROWSER_URL)).toBe('download');
    expect(fileSaveAction('gpu', null)).toBe('download');
  });
});

describe('describing a saved file', () => {
  it('gives the bare path for a save on this computer', () => {
    expect(savedFileMessage('save-here', 'gpu', '/home/u/Downloads/a.png')).toBe(
      'Saved to /home/u/Downloads/a.png',
    );
  });

  it('names the computer for a save on another one, by its key when it has no entry', () => {
    expect(savedFileMessage('save-there', 'gpu', '/home/u/Downloads/a.png')).toBe(
      'Saved on gpu-box: /home/u/Downloads/a.png',
    );
    expect(savedFileMessage('save-there', 'lost', '/x/a.png')).toBe('Saved on lost: /x/a.png');
  });
});
