import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { openForgeAttachment } from './forgeAttachmentActions';
import { DOWNLOAD_URL_LIFETIME_MS } from './blobDownload';
import { buildForgeAttachmentHref, parseForgeAttachmentHref } from './forgeAttachments';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { PRRef } from './prReference';

const nativeShell = vi.hoisted(() => ({ value: false }));
const webviewHosted = vi.hoisted(() => ({ value: false }));
const hostScope = vi.hoisted(() => ({ value: false }));
const toasts = vi.hoisted(() => [] as Array<[string, string]>);
const toastErrors = vi.hoisted(() => [] as unknown[]);
const externalOpens = vi.hoisted(() => [] as string[]);
const release = vi.hoisted(() => vi.fn());
const acquired = vi.hoisted(() => ({ value: Promise.resolve({}) as Promise<unknown> }));
const original = vi.hoisted(() => vi.fn(async () => new Blob(['full'], { type: 'image/png' })));

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
  hasScope: () => hostScope.value,
}));
vi.mock('../transport/backends', () => ({
  withBackendTarget: <T>(_backend: string, run: () => T): T => run(),
}));
vi.mock('../stores/attachedBackends.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../stores/attachedBackends.svelte')>()),
  attachedBackendEntry: (key: string) => (key === 'gpu' ? { id: 'gpu', name: 'gpu-box' } : undefined),
  backendDisplayName: (entry: { name: string }) => entry.name,
}));
vi.mock('../stores/toast.svelte', () => ({
  addToast: (kind: string, message: string) => { toasts.push([kind, message]); return 'id'; },
  addErrorToast: (message: string, err: unknown) => { toasts.push(['error', message]); toastErrors.push(err); return 'id'; },
}));
vi.mock('./externalLinks', () => ({
  handleExternalURL: async (url: string) => { externalOpens.push(url); return true; },
}));
vi.mock('./forgeAttachmentCache', () => ({
  acquirePaintedForgeAttachment: () => ({ value: acquired.value, release }),
  fetchForgeAttachmentBytes: (...args: unknown[]) => original(...(args as [])),
}));

const HEX = '0123456789abcdef0123456789abcdef';
const MR: PRRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 };
const WEB = 'https://gitlab.example.test/group/widget/-/merge_requests/3';
const FORGE_URL = `https://gitlab.example.test/group/widget/uploads/${HEX}/report.pdf`;
const WIRE_REF = { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'widget', Number: 3 };

function parsed(webBase = WEB) {
  const href = buildForgeAttachmentHref({
    href: `/uploads/${HEX}/report.pdf`,
    pr: MR,
    backend: 'gpu',
    webBase,
  });
  return parseForgeAttachmentHref(href)!;
}

function spyAnchorClicks(): Array<[string, string]> {
  const clicks: Array<[string, string]> = [];
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    clicks.push([this.getAttribute('href') ?? '', this.getAttribute('download') ?? '']);
  });
  return clicks;
}

describe('activating a forge attachment', () => {
  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    hostScope.value = false;
    toasts.length = 0;
    toastErrors.length = 0;
    externalOpens.length = 0;
    release.mockClear();
    original.mockClear();
    acquired.value = Promise.resolve({
      url: 'blob:forge-1',
      mimeType: 'application/octet-stream',
      kind: 'file',
      sizeBytes: 3,
      filename: 'report.pdf',
    });
    document.body.innerHTML = '';
  });
  afterEach(() => {
    vi.restoreAllMocks();
    resetBindingMocks();
    document.body.innerHTML = '';
  });

  it('opens the forge in the phone shell, where the user is already signed in', async () => {
    nativeShell.value = true;
    hostScope.value = true;
    await openForgeAttachment(parsed());
    expect(externalOpens).toEqual([FORGE_URL]);
    expect(toasts).toEqual([]);
  });

  it("opens the forge from the desktop webview showing another computer's PR", async () => {
    webviewHosted.value = true;
    const clicks = spyAnchorClicks();
    await openForgeAttachment(parsed());
    expect(externalOpens).toEqual([FORGE_URL]);
    // No blob download is attempted: the webview has nothing to service it.
    expect(clicks).toEqual([]);
    expect(toasts).toEqual([]);
  });

  it('saves on the owning computer, and says which, from a webview with no forge URL', async () => {
    webviewHosted.value = true;
    const save = setBindingMock('SaveForgeAttachment', async () => '/home/u/Downloads/report.pdf');
    const clicks = spyAnchorClicks();
    await openForgeAttachment(parsed(''));
    expect(externalOpens).toEqual([]);
    expect(clicks).toEqual([]);
    expect(save).toHaveBeenCalledWith(WIRE_REF, `/uploads/${HEX}/report.pdf`);
    expect(toasts).toEqual([['success', 'Saved on gpu-box: /home/u/Downloads/report.pdf']]);
  });

  it('writes the file on the computer that owns the PR when this page may act there', async () => {
    hostScope.value = true;
    const save = setBindingMock('SaveForgeAttachment', async () => '/home/u/Downloads/report.pdf');
    await openForgeAttachment(parsed());
    expect(save).toHaveBeenCalledWith(WIRE_REF, `/uploads/${HEX}/report.pdf`);
    expect(toasts).toEqual([['success', 'Saved to /home/u/Downloads/report.pdf']]);
  });

  it('surfaces a refused save instead of failing silently', async () => {
    hostScope.value = true;
    setBindingMock('SaveForgeAttachment', async () => {
      throw new Error("GitLab CLI (`glab`) is not authenticated. Run 'glab auth login' to continue");
    });
    await openForgeAttachment(parsed());
    expect(toasts[0][0]).toBe('error');
    expect(toasts[0][1]).toContain('glab auth login');
    expect(toastErrors).toEqual([expect.any(Error)]);
  });

  it('downloads through the browser when there is no desktop to write to', async () => {
    const downloads = spyAnchorClicks();
    await openForgeAttachment(parsed());
    expect(downloads).toEqual([['blob:forge-1', 'report.pdf']]);
    expect(original).not.toHaveBeenCalled();
    expect(release).toHaveBeenCalledTimes(1);
    // Nothing is left behind in the document.
    expect(document.body.querySelector('a')).toBeNull();
  });

  it('downloads the original, not the display-size derivative the timeline painted', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout'] });
    acquired.value = Promise.resolve({
      url: 'blob:derivative',
      mimeType: 'image/png',
      kind: 'image',
      sizeBytes: 9000,
      filename: 'shot.png',
      derived: true,
    });
    const created: Blob[] = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
      created.push(blob as Blob);
      return 'blob:original';
    });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const downloads = spyAnchorClicks();
    try {
      await openForgeAttachment(parsed());
      expect(original).toHaveBeenCalledWith('gpu', MR, `/uploads/${HEX}/report.pdf`, 0);
      expect(downloads).toEqual([['blob:original', 'shot.png']]);
      expect(await created[0]!.text()).toBe('full');
      expect(release).toHaveBeenCalledTimes(1);
      vi.advanceTimersByTime(DOWNLOAD_URL_LIFETIME_MS);
      expect(revoke).toHaveBeenCalledWith('blob:original');
    } finally {
      vi.useRealTimers();
    }
  });

  it('reports a failed download and still releases its cache claim', async () => {
    acquired.value = Promise.reject(new Error('gh api attachment download failed: HTTP 404'));
    await openForgeAttachment(parsed());
    expect(toasts[0][0]).toBe('error');
    expect(toasts[0][1]).toContain('HTTP 404');
    expect(toastErrors).toEqual([expect.any(Error)]);
    expect(release).toHaveBeenCalledTimes(1);
  });
});
