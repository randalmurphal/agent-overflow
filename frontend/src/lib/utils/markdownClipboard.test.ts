// Copy-path contract: both flavors go to the clipboard where the
// environment can carry them, and the markdown-only path is what runs
// everywhere else. The second half matters as much as the first — the
// app ships under WebKitGTK, WKWebView and WebView2, and a copy that
// breaks because `ClipboardItem` is missing is worse than a copy that
// pastes markdown.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyMarkdownFlavors,
  copyMarkdownToClipboard,
  markdownClipboardFlavors,
} from './markdownClipboard';

type WriteSpy = ReturnType<typeof vi.fn<(items: ClipboardItem[]) => Promise<void>>>;
type TextSpy = ReturnType<typeof vi.fn<(text: string) => Promise<void>>>;

/** Records what each ClipboardItem was constructed with. */
const constructed: Record<string, Blob>[] = [];

class RecordingClipboardItem {
  readonly types: string[];
  constructor(items: Record<string, Blob>) {
    constructed.push(items);
    this.types = Object.keys(items);
  }
}

function installClipboard(options: { writeSucceeds?: boolean; hasClipboardItem?: boolean } = {}): {
  write: WriteSpy;
  writeText: TextSpy;
} {
  const writeSucceeds = options.writeSucceeds ?? true;
  const write = vi.fn<(items: ClipboardItem[]) => Promise<void>>(async () => {
    if (!writeSucceeds) throw new Error('mock: write rejected');
  });
  const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});

  Object.defineProperty(navigator, 'clipboard', {
    value: { write, writeText },
    configurable: true,
    writable: true,
  });

  if (options.hasClipboardItem ?? true) {
    vi.stubGlobal('ClipboardItem', RecordingClipboardItem);
  } else {
    vi.stubGlobal('ClipboardItem', undefined);
  }
  return { write, writeText };
}

async function textOf(blob: Blob | undefined): Promise<string> {
  if (!blob) return '';
  return blob.text();
}

const MARKDOWN = '# Title\n\nSome **bold** text.';

beforeEach(() => {
  constructed.length = 0;
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('copyMarkdownToClipboard — rich path', () => {
  it('hands both flavors to one ClipboardItem', async () => {
    const { write, writeText } = installClipboard();

    await expect(copyMarkdownToClipboard(MARKDOWN)).resolves.toBe(true);

    expect(write).toHaveBeenCalledTimes(1);
    expect(writeText).not.toHaveBeenCalled();
    expect(constructed).toHaveLength(1);

    const item = constructed[0];
    expect(Object.keys(item).sort()).toEqual(['text/html', 'text/plain']);
    await expect(textOf(item['text/plain'])).resolves.toBe(MARKDOWN);
    await expect(textOf(item['text/html'])).resolves.toBe(
      '<h1>Title</h1><p>Some <strong>bold</strong> text.</p>',
    );
  });

  it('labels each blob with its own MIME type', async () => {
    installClipboard();
    await copyMarkdownToClipboard(MARKDOWN);
    expect(constructed[0]['text/plain'].type).toBe('text/plain');
    expect(constructed[0]['text/html'].type).toBe('text/html');
  });
});

describe('copyMarkdownToClipboard — degraded environments', () => {
  it('falls back to a plain-text write when ClipboardItem is unavailable', async () => {
    const { write, writeText } = installClipboard({ hasClipboardItem: false });

    await expect(copyMarkdownToClipboard(MARKDOWN)).resolves.toBe(true);

    expect(write).not.toHaveBeenCalled();
    expect(writeText).toHaveBeenCalledWith(MARKDOWN);
    expect(constructed).toHaveLength(0);
  });

  it('falls back to a plain-text write when clipboard.write is unavailable', async () => {
    const writeText = vi.fn<(text: string) => Promise<void>>(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    vi.stubGlobal('ClipboardItem', RecordingClipboardItem);

    await expect(copyMarkdownToClipboard(MARKDOWN)).resolves.toBe(true);

    expect(writeText).toHaveBeenCalledWith(MARKDOWN);
    expect(constructed).toHaveLength(0);
  });

  it('falls back to a plain-text write when the rich write rejects', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const { write, writeText } = installClipboard({ writeSucceeds: false });

    await expect(copyMarkdownToClipboard(MARKDOWN)).resolves.toBe(true);

    expect(write).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith(MARKDOWN);
  });

  it('surfaces a rejected rich write rather than swallowing it', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    installClipboard({ writeSucceeds: false });

    await copyMarkdownToClipboard(MARKDOWN);

    expect(errorSpy).toHaveBeenCalled();
  });

  it('skips the html flavor when there is nothing renderable to carry', async () => {
    // Raw HTML is dropped by the serializer (it is never rendered
    // on-screen either), so this source has no html flavor at all.
    const { write, writeText } = installClipboard();
    const rawOnly = '<script>alert(1)</script>';

    await expect(copyMarkdownToClipboard(rawOnly)).resolves.toBe(true);

    expect(write).not.toHaveBeenCalled();
    expect(writeText).toHaveBeenCalledWith(rawOnly);
  });

  it('reports failure when even the plain-text write fails', async () => {
    installClipboard({ hasClipboardItem: false });
    Object.defineProperty(navigator, 'clipboard', {
      value: {
        writeText: vi.fn(async () => {
          throw new Error('mock: writeText rejected');
        }),
      },
      configurable: true,
      writable: true,
    });

    await expect(copyMarkdownToClipboard(MARKDOWN)).resolves.toBe(false);
  });
});

describe('markdownClipboardFlavors / applyMarkdownFlavors', () => {
  it('pairs the markdown with its html rendering', () => {
    expect(markdownClipboardFlavors('**b**')).toEqual({
      plain: '**b**',
      html: '<p><strong>b</strong></p>',
    });
  });

  it('writes both flavors into a DataTransfer', () => {
    const data = new DataTransfer();
    applyMarkdownFlavors(data, markdownClipboardFlavors('# H'));
    expect(data.getData('text/plain')).toBe('# H');
    expect(data.getData('text/html')).toBe('<h1>H</h1>');
  });

  it('leaves text/html unset when there is no html to write', () => {
    const data = new DataTransfer();
    applyMarkdownFlavors(data, { plain: 'raw', html: '' });
    expect(data.getData('text/plain')).toBe('raw');
    expect(data.getData('text/html')).toBe('');
  });
});
