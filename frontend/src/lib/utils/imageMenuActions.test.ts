import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DOWNLOAD_URL_LIFETIME_MS } from './blobDownload';
import {
  attachmentImageMenuTag,
  canSaveMenuImage,
  copyLocalImageMarkdown,
  copyLocalImagePath,
  copyMenuImage,
  downloadName,
  forgeImageMenuTag,
  localImageMenuTag,
  saveMenuImage,
  saveMenuImageLabel,
  taggedMenuImage,
  type ImageMenuTarget,
  type LocalMenuImage,
} from './imageMenuActions';
import { buildForgeAttachmentHref, parseForgeAttachmentHref } from './forgeAttachments';
import type { ResolvedForgeAttachment } from './forgeAttachmentCache';
import { buildLocalImageHref } from './pathLinkExtension';
import { getPinnedBackend } from '../transport/backends';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { mockAttachmentDownload, mockLocalImage } from '../../test/mocks/attachmentTransfer';

const nativeShell = vi.hoisted(() => ({ value: false }));
const webviewHosted = vi.hoisted(() => ({ value: false }));
const scopes = vi.hoisted(() => ({ host: false, write: true, git: true, files: true }));
const forgeCache = vi.hoisted(() => ({ acquire: vi.fn(), release: vi.fn(), original: vi.fn() }));
const openForge = vi.hoisted(() => vi.fn(async () => {}));
const toasts = vi.hoisted(() => [] as Array<[string, string]>);
const toastErrors = vi.hoisted(() => [] as unknown[]);

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
  hasScope: (scope: string) =>
    scope === 'host'
      ? scopes.host
      : scope === 'attachments:write'
        ? scopes.write
        : scope === 'files:read'
          ? scopes.files
          : scope === 'git:operate' && scopes.git,
}));
vi.mock('./forgeAttachmentCache', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./forgeAttachmentCache')>()),
  acquirePaintedForgeAttachment: (...args: unknown[]) => forgeCache.acquire(...args),
  fetchForgeAttachmentOriginal: (...args: unknown[]) => forgeCache.original(...args),
}));
vi.mock('./forgeAttachmentActions', () => ({
  openForgeAttachment: (...args: unknown[]) => openForge(...(args as [])),
}));
vi.mock('../stores/toast.svelte', () => ({
  addToast: (kind: string, message: string) => {
    toasts.push([kind, message]);
    return 'id';
  },
  addErrorToast: (message: string, err: unknown) => {
    toasts.push(['error', message]);
    toastErrors.push(err);
    return 'id';
  },
}));

const REF = { kind: 'attachment' as const, threadId: 'thread-1', attachmentId: 'att-1', filename: 'shot.webp' };

const GITHUB_HREF = buildForgeAttachmentHref({
  href: 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666',
  pr: { forge: 'github', namespace: 'octo', repo: 'widget', number: 7 },
  backend: 'gpu',
  webBase: 'https://github.com/octo/widget/pull/7',
});
const FORGE: ImageMenuTarget = { kind: 'forge', attachment: parseForgeAttachmentHref(GITHUB_HREF)! };

function forgeResolves(value: Partial<ResolvedForgeAttachment>): void {
  forgeCache.acquire.mockImplementation(() => ({
    value: Promise.resolve({
      url: 'blob:forge-1',
      mimeType: 'image/png',
      kind: 'image',
      sizeBytes: 3,
      filename: 'shot.png',
      blob: new Blob(['png'], { type: 'image/png' }),
      ...value,
    } as ResolvedForgeAttachment),
    release: forgeCache.release,
  }));
}

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true, writable: true });
}

describe('attachment image tag', () => {
  it('round-trips through the DOM from any descendant', () => {
    const button = document.createElement('button');
    for (const [name, value] of Object.entries(
      attachmentImageMenuTag({ id: 'att-1', threadId: 'thread-1', filename: 'shot.webp' }),
    )) {
      button.setAttribute(name, value);
    }
    const img = document.createElement('img');
    button.appendChild(img);
    expect(taggedMenuImage(img)).toEqual({ element: button, target: REF });
  });

  it('answers null off a tag, and for a tag missing its thread', () => {
    const plain = document.createElement('div');
    expect(taggedMenuImage(plain)).toBeNull();
    expect(taggedMenuImage(null)).toBeNull();
    const partial = document.createElement('div');
    const { 'data-image-menu-thread': _thread, ...withoutThread } = attachmentImageMenuTag({
      id: 'att-1',
      threadId: 'thread-1',
      filename: 'shot.webp',
    });
    for (const [name, value] of Object.entries(withoutThread)) partial.setAttribute(name, value);
    expect(taggedMenuImage(partial)).toBeNull();
  });
});

describe('copying an attachment image', () => {
  const originalClipboard = navigator.clipboard;
  afterEach(() => {
    setClipboard(originalClipboard);
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    resetBindingMocks();
  });

  it('fetches the original bytes and re-encodes a non-PNG before writing', async () => {
    const original = new Blob(['webp-bytes'], { type: 'image/webp' });
    const download = mockAttachmentDownload(() => original);
    const encoded = new Blob(['png-bytes'], { type: 'image/png' });
    const decode = vi.fn(async () => ({ width: 4, height: 3, close: vi.fn() }) as unknown as ImageBitmap);
    vi.stubGlobal('createImageBitmap', decode);
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
      () => ({ drawImage: vi.fn() }) as unknown as CanvasRenderingContext2D,
    );
    vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((cb: BlobCallback) => cb(encoded));
    const written: Blob[] = [];
    const write = vi.fn(async (items: ClipboardItem[]) => {
      written.push(await items[0].getType('image/png'));
    });
    setClipboard({ write });

    const copy = copyMenuImage(REF);
    // Reached before the mint round trip, inside the click's task.
    expect(write).toHaveBeenCalledTimes(1);
    await copy;

    expect(download).toHaveBeenCalledWith('thread-1', 'att-1');
    expect((decode.mock.calls[0] as unknown[])[0]).toMatchObject({ type: 'image/webp', size: original.size });
    expect(written).toEqual([encoded]);
  });

  it('names a refused download as the cause', async () => {
    mockAttachmentDownload(() => {
      throw new Error('attachment: not an image');
    });
    setClipboard({
      write: async (items: ClipboardItem[]) => {
        try {
          await items[0].getType('image/png');
        } catch {
          throw new Error('NotAllowedError');
        }
      },
    });
    await expect(copyMenuImage(REF)).rejects.toThrow(
      'Could not copy the image: Could not load image: attachment: not an image',
    );
  });
});

describe('saving an attachment image', () => {
  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    scopes.host = false;
    scopes.write = true;
    toasts.length = 0;
    toastErrors.length = 0;
    resetBindingMocks();
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('has the backend write it into Downloads when this page is on the owning computer', async () => {
    scopes.host = true;
    const save = setBindingMock('SaveAttachment', async () => '/home/u/Downloads/shot.webp');
    await saveMenuImage(REF);
    expect(save).toHaveBeenCalledWith('thread-1', 'att-1');
    expect(toasts).toEqual([['success', 'Saved to /home/u/Downloads/shot.webp']]);
  });

  it('writes it on the owning computer, and says so, from a webview or the phone', async () => {
    for (const shell of ['webview', 'phone'] as const) {
      toasts.length = 0;
      webviewHosted.value = shell === 'webview';
      nativeShell.value = shell === 'phone';
      const save = setBindingMock('SaveAttachment', async () => '/home/u/Downloads/shot.webp');
      const clicks = vi.spyOn(HTMLAnchorElement.prototype, 'click');
      await saveMenuImage(REF);
      expect(save).toHaveBeenCalledWith('thread-1', 'att-1');
      expect(clicks).not.toHaveBeenCalled();
      expect(toasts).toHaveLength(1);
      expect(toasts[0][0]).toBe('success');
      expect(toasts[0][1]).toMatch(/^Saved on .+: \/home\/u\/Downloads\/shot\.webp$/);
    }
  });

  it('downloads the original bytes under the attachment name in a connected browser', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout'] });
    const original = new Blob(['webp-bytes'], { type: 'image/webp' });
    mockAttachmentDownload(() => original);
    const save = setBindingMock('SaveAttachment', async () => '/unused');
    const created: Blob[] = [];
    vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
      created.push(blob as Blob);
      return 'blob:download-1';
    });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const clicks: Array<[string, string]> = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicks.push([this.getAttribute('href') ?? '', this.getAttribute('download') ?? '']);
    });

    await saveMenuImage(REF);

    expect(save).not.toHaveBeenCalled();
    expect(clicks).toEqual([['blob:download-1', 'shot.webp']]);
    expect(created[0]).toMatchObject({ type: 'image/webp', size: original.size });
    expect(document.body.querySelector('a')).toBeNull();
    expect(toasts).toEqual([]);
    // The URL outlives the click, then is released.
    expect(revoke).not.toHaveBeenCalled();
    vi.advanceTimersByTime(DOWNLOAD_URL_LIFETIME_MS);
    expect(revoke).toHaveBeenCalledWith('blob:download-1');
  });

  it('gives a download named without an extension the one its bytes call for', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout'] });
    mockAttachmentDownload(() => new Blob(['png-bytes'], { type: 'image/png' }));
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:download-2');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const names: string[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      names.push(this.getAttribute('download') ?? '');
    });
    await saveMenuImage({ ...REF, filename: 'att-1' });
    await saveMenuImage({ ...REF, filename: '' });
    expect(names).toEqual(['att-1.png', 'image.png']);
  });

  it('surfaces a refused save instead of failing silently', async () => {
    scopes.host = true;
    setBindingMock('SaveAttachment', async () => {
      throw new Error('attachment: not an image: "att-1" is a file attachment');
    });
    await saveMenuImage(REF);
    expect(toasts).toEqual([['error', 'attachment: not an image: "att-1" is a file attachment']]);
    expect(toastErrors).toEqual([expect.any(Error)]);
  });

  it('surfaces a failed download', async () => {
    mockAttachmentDownload(() => {
      throw new Error('gone');
    });
    await saveMenuImage(REF);
    expect(toasts).toEqual([['error', 'Could not load image: gone']]);
    expect(toastErrors).toEqual([expect.any(Error)]);
  });
});

describe('whether this page may save', () => {
  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    scopes.host = false;
    scopes.write = false;
  });

  it('needs attachments:write only where the backend writes the file', () => {
    // Connected browser: a download reads through the same route the
    // lightbox does, so no write grant is involved.
    expect(canSaveMenuImage(REF)).toBe(true);
    // Phone shell or webview on another computer: the backend writes.
    nativeShell.value = true;
    expect(canSaveMenuImage(REF)).toBe(false);
    scopes.write = true;
    expect(canSaveMenuImage(REF)).toBe(true);
  });
});

describe('the browser download name', () => {
  it('keeps a name that has an extension and supplies one that does not', () => {
    expect(downloadName('shot.webp', 'image/webp')).toBe('shot.webp');
    expect(downloadName('shot.PNG', 'image/png')).toBe('shot.PNG');
    expect(downloadName('att-1', 'image/jpeg')).toBe('att-1.jpg');
    expect(downloadName('  ', 'image/gif')).toBe('image.gif');
    expect(downloadName('att-1', '')).toBe('att-1');
  });
});

describe('forge images', () => {
  const originalClipboard = navigator.clipboard;
  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    scopes.host = false;
    scopes.git = true;
    forgeCache.acquire.mockReset();
    forgeCache.release.mockReset();
    forgeCache.original.mockReset();
    openForge.mockClear();
    toasts.length = 0;
  });
  afterEach(() => {
    setClipboard(originalClipboard);
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    document.body.innerHTML = '';
  });

  it('tags an element with the nonce-gated href and reads it back', () => {
    const img = document.createElement('img');
    for (const [name, value] of Object.entries(forgeImageMenuTag(GITHUB_HREF))) img.setAttribute(name, value);
    expect(taggedMenuImage(img)).toEqual({ element: img, target: FORGE });
  });

  it('refuses a forge tag whose href this page did not mint', () => {
    const img = document.createElement('img');
    const forged = GITHUB_HREF.replace(/nonce=[^&]+/, 'nonce=not-this-page');
    for (const [name, value] of Object.entries(forgeImageMenuTag(forged))) img.setAttribute(name, value);
    expect(taggedMenuImage(img)).toBeNull();
  });

  it('copies the bytes the page already holds, re-encoded, and releases its claim', async () => {
    const original = new Blob(['webp-bytes'], { type: 'image/webp' });
    forgeResolves({ blob: original, mimeType: 'image/webp' });
    const encoded = new Blob(['png-bytes'], { type: 'image/png' });
    const decode = vi.fn(async () => ({ width: 4, height: 3, close: vi.fn() }) as unknown as ImageBitmap);
    vi.stubGlobal('createImageBitmap', decode);
    vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(
      () => ({ drawImage: vi.fn() }) as unknown as CanvasRenderingContext2D,
    );
    vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation((cb: BlobCallback) => cb(encoded));
    const written: Blob[] = [];
    const write = vi.fn(async (items: ClipboardItem[]) => {
      written.push(await items[0].getType('image/png'));
    });
    setClipboard({ write });

    const copy = copyMenuImage(FORGE);
    expect(write).toHaveBeenCalledTimes(1);
    await copy;

    const parsed = FORGE.kind === 'forge' ? FORGE.attachment : null;
    expect(forgeCache.acquire).toHaveBeenCalledWith(parsed!.backend, parsed!.pr, parsed!.href);
    expect(decode).toHaveBeenCalledWith(original);
    expect(written).toEqual([encoded]);
    expect(forgeCache.original).not.toHaveBeenCalled();
    expect(forgeCache.release).toHaveBeenCalledTimes(1);
  });

  it('copies the original, not the display-size derivative the page painted', async () => {
    forgeResolves({ blob: new Blob(['small'], { type: 'image/png' }), derived: true });
    const full = new Blob(['full-size'], { type: 'image/png' });
    forgeCache.original.mockResolvedValue(full);
    const written: Blob[] = [];
    setClipboard({
      write: async (items: ClipboardItem[]) => {
        written.push(await items[0].getType('image/png'));
      },
    });

    await copyMenuImage(FORGE);

    const parsed = FORGE.kind === 'forge' ? FORGE.attachment : null;
    expect(forgeCache.original).toHaveBeenCalledWith(parsed!.backend, parsed!.pr, parsed!.href);
    // A PNG goes on the clipboard as it is, so what was written is the original.
    expect(written).toEqual([full]);
    expect(forgeCache.release).toHaveBeenCalledTimes(1);
  });

  it('releases its claim and names the cause when the bytes are not an image', async () => {
    forgeResolves({ kind: 'video', mimeType: 'video/mp4', blob: new Blob(['v'], { type: 'video/mp4' }) });
    setClipboard({
      write: async (items: ClipboardItem[]) => {
        await items[0].getType('image/png');
      },
    });
    await expect(copyMenuImage(FORGE)).rejects.toThrow('Could not copy the image: this attachment is not an image');
    expect(forgeCache.release).toHaveBeenCalledTimes(1);
  });

  it('saves through the forge attachment activation', async () => {
    await saveMenuImage(FORGE);
    expect(openForge).toHaveBeenCalledWith(FORGE.kind === 'forge' ? FORGE.attachment : null);
  });

  it('needs git:operate only where the owning computer writes the file', () => {
    scopes.git = false;
    // Connected browser: an ordinary download of the bytes already held.
    expect(canSaveMenuImage(FORGE)).toBe(true);
    // On the owning computer: SaveForgeAttachment, which is git:operate.
    scopes.host = true;
    expect(canSaveMenuImage(FORGE)).toBe(false);
    scopes.git = true;
    expect(canSaveMenuImage(FORGE)).toBe(true);
  });

  it('says Open on the forge where the action opens the browser instead of saving', () => {
    expect(saveMenuImageLabel(FORGE)).toBe('Save Image');
    nativeShell.value = true;
    expect(saveMenuImageLabel(FORGE)).toBe('Open on GitHub');
    // Opening a page needs no grant.
    scopes.git = false;
    expect(canSaveMenuImage(FORGE)).toBe(true);
    // A thread attachment has no forge page: its save is always a save.
    expect(saveMenuImageLabel(REF)).toBe('Save Image');
  });
});

describe('local images', () => {
  const originalClipboard = navigator.clipboard;
  const SOURCE = 'docs/shot one.png';
  const LOCAL_HREF = buildLocalImageHref('/repo/docs/shot one.png', '/repo', SOURCE);
  const LOCAL: LocalMenuImage = {
    kind: 'local',
    backend: 'gpu',
    path: '/repo/docs/shot one.png',
    workspacePath: '/repo',
    sourceHref: SOURCE,
    alt: 'the diagram',
  };

  function tagged(tag: Record<string, string>): HTMLElement {
    const host = document.createElement('span');
    for (const [name, value] of Object.entries(tag)) host.setAttribute(name, value);
    host.appendChild(document.createElement('img'));
    return host;
  }

  beforeEach(() => {
    nativeShell.value = false;
    webviewHosted.value = false;
    scopes.host = false;
    scopes.files = true;
    toasts.length = 0;
    toastErrors.length = 0;
    resetBindingMocks();
  });
  afterEach(() => {
    setClipboard(originalClipboard);
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    resetBindingMocks();
    document.body.innerHTML = '';
  });

  describe('the tag', () => {
    it('round-trips the href the parse built, the computer and the alt text', () => {
      const host = tagged(localImageMenuTag({ backend: 'gpu', href: LOCAL_HREF, alt: 'the diagram' }));
      expect(taggedMenuImage(host.firstElementChild)).toEqual({ element: host, target: LOCAL });
    });

    it('keeps HOME, whose key is empty, and a reference the parse kept no source for', () => {
      const href = buildLocalImageHref('/repo/a.png', '/repo');
      const host = tagged(localImageMenuTag({ backend: '', href, alt: '' }));
      expect(taggedMenuImage(host)?.target).toEqual({
        kind: 'local',
        backend: '',
        path: '/repo/a.png',
        workspacePath: '/repo',
        sourceHref: '',
        alt: '',
      });
    });

    it('refuses a tag with no computer, and an href this page did not mint', () => {
      const { 'data-image-menu-backend': _backend, ...withoutBackend } = localImageMenuTag({
        backend: 'gpu',
        href: LOCAL_HREF,
        alt: 'x',
      });
      expect(taggedMenuImage(tagged(withoutBackend))).toBeNull();
      const forged = LOCAL_HREF.replace(/nonce=[^&]+/, 'nonce=not-this-page');
      expect(taggedMenuImage(tagged(localImageMenuTag({ backend: 'gpu', href: forged, alt: 'x' })))).toBeNull();
    });
  });

  describe('Copy Image', () => {
    it("copies the file's original from the thread's computer, not a derivative", async () => {
      const pins: Array<string | null> = [];
      const rpc = mockLocalImage(() => {
        pins.push(getPinnedBackend());
        return { blob: new Blob(['full-size'], { type: 'image/png' }) };
      });
      const written: Blob[] = [];
      const write = vi.fn(async (items: ClipboardItem[]) => {
        written.push(await items[0].getType('image/png'));
      });
      setClipboard({ write });

      const copy = copyMenuImage(LOCAL);
      // Reached before the RPC, inside the click's task.
      expect(write).toHaveBeenCalledTimes(1);
      await copy;

      expect(rpc).toHaveBeenCalledWith('/repo/docs/shot one.png', '/repo', 0);
      expect(pins).toEqual(['gpu']);
      expect(written).toHaveLength(1);
      expect(await written[0].text()).toBe('full-size');
    });

    it('names a refused read as the cause', async () => {
      mockLocalImage(() => {
        throw new Error('local image: outside the workspace');
      });
      setClipboard({
        write: async (items: ClipboardItem[]) => {
          await items[0].getType('image/png');
        },
      });
      await expect(copyMenuImage(LOCAL)).rejects.toThrow(
        'Could not copy the image: local image: outside the workspace',
      );
    });
  });

  describe('Copy Path and Copy Markdown', () => {
    function clipboardText(): ReturnType<typeof vi.fn> {
      const writeText = vi.fn(async (_text: string) => {});
      setClipboard({ writeText });
      return writeText;
    }

    it('copy the reference as the agent wrote it, and confirm', async () => {
      const writeText = clipboardText();

      const path = copyLocalImagePath(LOCAL);
      // Reached before anything is awaited, inside the click's task.
      expect(writeText).toHaveBeenCalledWith('docs/shot one.png');
      await path;
      const markdown = copyLocalImageMarkdown(LOCAL);
      expect(writeText).toHaveBeenLastCalledWith('![the diagram](docs/shot one.png)');
      await markdown;

      expect(toasts).toEqual([
        ['success', 'Path copied'],
        ['success', 'Markdown copied'],
      ]);
    });

    it('fall back to the resolved path when the parse kept no reference', async () => {
      const writeText = clipboardText();
      const bare = { ...LOCAL, sourceHref: '' };
      await copyLocalImagePath(bare);
      await copyLocalImageMarkdown(bare);
      expect(writeText.mock.calls).toEqual([
        ['/repo/docs/shot one.png'],
        ['![the diagram](/repo/docs/shot one.png)'],
      ]);
    });

    it('report a refused write as an error toast, not a success', async () => {
      vi.spyOn(console, 'error').mockImplementation(() => {});
      setClipboard({
        writeText: async () => {
          throw new DOMException('Document is not focused.', 'NotAllowedError');
        },
      });
      await copyLocalImagePath(LOCAL);
      await copyLocalImageMarkdown(LOCAL);
      expect(toasts).toEqual([
        ['error', 'Could not copy the path'],
        ['error', 'Could not copy the markdown'],
      ]);
    });
  });

  describe('Save Image', () => {
    it("has the thread's computer copy the file into Downloads when this page is on it", async () => {
      scopes.host = true;
      const pins: Array<string | null> = [];
      const save = setBindingMock('SaveLocalImage', async () => {
        pins.push(getPinnedBackend());
        return '/home/u/Downloads/shot one.png';
      });
      await saveMenuImage(LOCAL);
      expect(save).toHaveBeenCalledWith('/repo/docs/shot one.png', '/repo');
      expect(pins).toEqual(['gpu']);
      expect(toasts).toEqual([['success', 'Saved to /home/u/Downloads/shot one.png']]);
    });

    it('writes it on that computer, and says so, from a webview or the phone', async () => {
      for (const shell of ['webview', 'phone'] as const) {
        toasts.length = 0;
        webviewHosted.value = shell === 'webview';
        nativeShell.value = shell === 'phone';
        const save = setBindingMock('SaveLocalImage', async () => '/home/u/Downloads/shot one.png');
        const read = mockLocalImage();
        await saveMenuImage(LOCAL);
        expect(save).toHaveBeenCalledWith('/repo/docs/shot one.png', '/repo');
        expect(read).not.toHaveBeenCalled();
        expect(toasts).toHaveLength(1);
        expect(toasts[0][0]).toBe('success');
        expect(toasts[0][1]).toMatch(/^Saved on .+: \/home\/u\/Downloads\/shot one\.png$/);
      }
    });

    it("downloads the file's original under its own name in a connected browser", async () => {
      vi.useFakeTimers({ toFake: ['setTimeout'] });
      const read = mockLocalImage(() => ({ blob: new Blob(['full-size'], { type: 'image/png' }) }));
      const save = setBindingMock('SaveLocalImage', async () => '/unused');
      const created: Blob[] = [];
      vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
        created.push(blob as Blob);
        return 'blob:download-local';
      });
      vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
      const clicks: Array<[string, string]> = [];
      vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
        clicks.push([this.getAttribute('href') ?? '', this.getAttribute('download') ?? '']);
      });

      await saveMenuImage(LOCAL);
      await saveMenuImage({ ...LOCAL, path: '/repo/docs/shot' });

      expect(save).not.toHaveBeenCalled();
      expect(read.mock.calls).toEqual([
        ['/repo/docs/shot one.png', '/repo', 0],
        ['/repo/docs/shot', '/repo', 0],
      ]);
      expect(await created[0].text()).toBe('full-size');
      // The name is the file's own, given the extension its bytes call for.
      expect(clicks).toEqual([
        ['blob:download-local', 'shot one.png'],
        ['blob:download-local', 'shot.png'],
      ]);
      expect(toasts).toEqual([]);
    });

    it('surfaces a refused save and a failed download', async () => {
      scopes.host = true;
      setBindingMock('SaveLocalImage', async () => {
        throw new Error('local image: not found');
      });
      await saveMenuImage(LOCAL);
      scopes.host = false;
      mockLocalImage(() => {
        throw new Error('local image: not found');
      });
      await saveMenuImage(LOCAL);
      expect(toasts).toEqual([
        ['error', 'local image: not found'],
        ['error', 'local image: not found'],
      ]);
      expect(toastErrors).toEqual([expect.any(Error), expect.any(Error)]);
    });

    it('needs files:read only where the owning computer writes the file', () => {
      scopes.files = false;
      // Connected browser: a download reads through the route the timeline does.
      expect(canSaveMenuImage(LOCAL)).toBe(true);
      // On the owning computer: SaveLocalImage, which is files:read.
      scopes.host = true;
      expect(canSaveMenuImage(LOCAL)).toBe(false);
      scopes.files = true;
      expect(canSaveMenuImage(LOCAL)).toBe(true);
    });

    it('is always labelled Save Image: a local file has no page to open instead', () => {
      expect(saveMenuImageLabel(LOCAL)).toBe('Save Image');
      nativeShell.value = true;
      expect(saveMenuImageLabel(LOCAL)).toBe('Save Image');
    });
  });
});
