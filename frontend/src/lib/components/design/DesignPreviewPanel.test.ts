import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DesignPreviewPanel from './DesignPreviewPanel.svelte';
import { makePanelContext } from '../../stores/panelContext.svelte';
import type { Thread } from '../../types/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { DESIGN_RELOAD_MAIN_EVENT } from '../../stores/eventNames';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';

// Mock the iframe-capture round-trip so tests don't need a real layout
// engine. Only one helper is involved now: requestIframeCapture returns
// a single PNG base64, used by the user-facing send-to-thread upload.
// The agent's read_screenshot path is backend-driven and bypasses the
// iframe entirely.
vi.mock('../../utils/captureHtml', () => ({
  requestIframeCapture: vi.fn(async () => 'ZmFrZS1wbmc='),
}));
import { requestIframeCapture } from '../../utils/captureHtml';

// Stub Element.animate (Toast subtree depends on it for transitions).
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    title: 'Design thread',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    projectId: 'proj-design',
    mode: 'design',
    ...overrides,
  });
}

async function buildPane() {
  setBindingMock('EnsureDesignWorkdir', async () => {});
  return buildRegisteredPane(makeThread());
}

// Wait for the EnsureDesignWorkdir effect to resolve and the iframe
// to mount. The component intentionally does not render the iframe
// until the workdir is confirmed (so a fresh thread doesn't 404),
// so every test that asserts on the iframe must wait for it.
async function waitForIframe(container: HTMLElement): Promise<HTMLIFrameElement> {
  return await waitFor(() => {
    const iframe = container.querySelector(
      'iframe[data-testid="design-preview-iframe"]',
    ) as HTMLIFrameElement | null;
    if (!iframe) throw new Error('iframe not yet mounted');
    return iframe;
  });
}

describe('<DesignPreviewPanel>', () => {
  beforeEach(() => {
    vi.mocked(requestIframeCapture).mockClear();
    vi.mocked(requestIframeCapture).mockResolvedValue('ZmFrZS1wbmc=');
  });

  it('renders an iframe pointing at /design/{threadId}/main/ once the workdir is ensured', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const iframe = await waitForIframe(container);
    const src = iframe.getAttribute('src') ?? '';
    expect(src.startsWith('/design/thread-1/main/?cb=')).toBe(true);
    // Sandbox is locked to allow-scripts only — no allow-same-origin
    // because the agent-rendered HTML is untrusted.
    expect(iframe.getAttribute('sandbox')).toBe('allow-scripts');
    expect(iframe.getAttribute('sandbox')).not.toMatch(/allow-same-origin/);
  });

  it('does not mount the iframe until EnsureDesignWorkdir resolves', async () => {
    const pane = await buildPane();
    // Override the resolved-immediately default installed by buildPane
    // with a pending promise so we can observe the placeholder state.
    let resolveEnsure: (() => void) | null = null;
    setBindingMock(
      'EnsureDesignWorkdir',
      () => new Promise<void>((res) => {
        resolveEnsure = res;
      }),
    );
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    // Pending — iframe should not be in the DOM yet.
    expect(container.querySelector('iframe[data-testid="design-preview-iframe"]')).toBeNull();
    expect(container.textContent).toMatch(/Preparing preview/);
    resolveEnsure!();
    await waitForIframe(container);
  });

  it('refresh button bumps the cache-bust counter', async () => {
    const pane = await buildPane();
    const { container, getByTestId } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;
    await fireEvent.click(getByTestId('design-refresh'));
    await waitFor(() => {
      const next = container.querySelector('iframe')!.getAttribute('src')!;
      expect(next).not.toBe(initialSrc);
      expect(next.startsWith('/design/thread-1/main/?cb=')).toBe(true);
    });
  });

  it('viewport toggle updates the iframe width', async () => {
    const pane = await buildPane();
    const { container, getByRole } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    let iframe = await waitForIframe(container);
    expect(iframe.style.width).toBe('100%');

    await fireEvent.click(getByRole('button', { name: /mobile/i }));
    iframe = container.querySelector('iframe')!;
    expect(iframe.style.width).toBe('375px');
    expect(pane.designViewport).toBe('mobile');

    await fireEvent.click(getByRole('button', { name: /tablet/i }));
    iframe = container.querySelector('iframe')!;
    expect(iframe.style.width).toBe('768px');
    expect(pane.designViewport).toBe('tablet');
  });

  it('responds to the reload-main event by bumping the cache-bust', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;

    window.dispatchEvent(
      new CustomEvent(DESIGN_RELOAD_MAIN_EVENT, { detail: { threadId: 'thread-1' } }),
    );
    await waitFor(() => {
      const next = container.querySelector('iframe')!.getAttribute('src')!;
      expect(next).not.toBe(initialSrc);
    });
  });

  it('ignores reload-main events for other threads', async () => {
    const pane = await buildPane();
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const initialIframe = await waitForIframe(container);
    const initialSrc = initialIframe.getAttribute('src')!;
    window.dispatchEvent(
      new CustomEvent(DESIGN_RELOAD_MAIN_EVENT, { detail: { threadId: 'someone-else' } }),
    );
    // Allow effects to settle.
    await Promise.resolve();
    await Promise.resolve();
    expect(container.querySelector('iframe')!.getAttribute('src')).toBe(initialSrc);
  });

  it('forwards iframe diagnostic postMessages via IngestDiagnosticBatch (debounced)', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const iframe = await waitForIframe(container);
    vi.useFakeTimers();
    try {

      window.dispatchEvent(
        new MessageEvent('message', {
          data: {
            aoDesign: 'diagnostics',
            items: [
              { severity: 'error', message: 'boom', source: 'iframe', line: 12 },
              { severity: 'warn', message: 'careful' },
            ],
          },
          source: iframe.contentWindow,
        }),
      );

      // No flush yet — still inside the debounce window.
      expect(ingest).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(300);

      expect(ingest).toHaveBeenCalledTimes(1);
      const arg = ingest.mock.calls[0][0] as { threadId: string; diagnostics: unknown[] };
      expect(arg.threadId).toBe('thread-1');
      expect(arg.diagnostics).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops postMessages without an aoDesign tag', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    const iframe = await waitForIframe(container);
    vi.useFakeTimers();
    try {
      window.dispatchEvent(
        new MessageEvent('message', {
          data: { irrelevant: true, items: [{ severity: 'error', message: 'spam' }] },
          source: iframe.contentWindow,
        }),
      );
      await vi.advanceTimersByTimeAsync(300);
      expect(ingest).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('drops postMessages whose source is not the mounted iframe', async () => {
    const pane = await buildPane();
    const ingest = setBindingMock('IngestDiagnosticBatch', async () => {});
    const { container } = render(DesignPreviewPanel, { props: { ctx: makePanelContext(pane, () => {}) } });
    await waitForIframe(container);
    vi.useFakeTimers();
    try {
      // Dispatch with no source — should be ignored even though shape matches.
      window.dispatchEvent(
        new MessageEvent('message', {
          data: {
            aoDesign: 'diagnostics',
            items: [{ severity: 'error', message: 'spoofed' }],
          },
        }),
      );
      await vi.advanceTimersByTimeAsync(300);
      expect(ingest).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  // Send-to-thread bundle: capture iframe → GetDesignWorkdirInfo →
  // CreateThread (chat) → UploadAttachment → SaveDraft → switch.
  // The test pins the binding contract end-to-end so a future refactor
  // that drops one of these calls (or reorders the dependency between
  // upload-then-draft) gets caught at unit-test scope rather than at
  // the integration test or, worse, in the user's hands when the new
  // chat thread comes up missing the screenshot.
  describe('send to thread', () => {
    function mockSendToThreadDeps() {
      setBindingMock('GetDesignWorkdirInfo', async () => ({
        mainPath: '/var/lib/agent-overflow/design/thread-1/main',
        files: ['index.html', 'style.css'],
      }));
      const createMock = setBindingMock('CreateThread', async () => ({
        id: 'new-chat',
        title: 'Design thread – follow-up',
        provider: 'claude',
        workspacePath: '/tmp',
        projectPath: '/tmp',
        projectId: 'proj-design',
        mode: 'chat',
        model: 'claude-sonnet-4-6',
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      }));
      const uploadMock = setBindingMock('UploadAttachment', async () => ({
        id: 'att-1',
        filename: 'design-preview.png',
        mimeType: 'image/png',
      }));
      const saveDraftMock = setBindingMock('SaveDraft', async () => {});
      const deleteMock = setBindingMock('DeleteThread', async () => {});
      return { createMock, uploadMock, saveDraftMock, deleteMock };
    }

    it('captures the iframe, creates a chat thread, uploads the screenshot, and seeds the draft', async () => {
      const pane = await buildPane();
      const { createMock, uploadMock, saveDraftMock } = mockSendToThreadDeps();
      const { container, getByTestId } = render(DesignPreviewPanel, {
        props: { ctx: makePanelContext(pane, () => {}) },
      });
      await waitForIframe(container);

      await fireEvent.click(getByTestId('design-send-to-thread'));

      // Wait on the last binding in the chain so the assertions don't
      // race the awaited handler.
      await waitFor(() => expect(saveDraftMock).toHaveBeenCalled());

      // 1. iframe capture happened — same primitive read_screenshot
      // uses, so we already know the postMessage round-trip works.
      expect(vi.mocked(requestIframeCapture)).toHaveBeenCalled();

      // 2. CreateThread was called with chat mode + the source thread's
      // project / workspace inheritance. Inheriting workspaceOverride
      // from the source matters: the new chat thread is rooted in the
      // same project repo, not in the design workdir, so the agent's
      // CWD lines up with the project codebase the user wants to
      // discuss the design alongside.
      expect(createMock).toHaveBeenCalledTimes(1);
      const createArgs = createMock.mock.calls[0][0] as {
        projectId: string;
        mode: string;
        workspaceOverride: string;
        title: string;
      };
      expect(createArgs.projectId).toBe('proj-design');
      expect(createArgs.mode).toBe('chat');
      expect(createArgs.workspaceOverride).toBe('/tmp');
      expect(createArgs.title).toContain('Design thread');

      // 3. UploadAttachment was scoped to the NEW thread (not the
      // source design thread). The design thread already has a workdir
      // for its own screenshots; the chat thread needs its own
      // attachment row for the message draft.
      expect(uploadMock).toHaveBeenCalledTimes(1);
      expect(uploadMock.mock.calls[0][0]).toBe('new-chat');
      expect(uploadMock.mock.calls[0][1]).toBe('design-preview.png');
      expect(uploadMock.mock.calls[0][2]).toBe('image/png');
      expect(uploadMock.mock.calls[0][3]).toBe('ZmFrZS1wbmc=');

      // 4. SaveDraft seeded the new thread's composer with the path,
      // the manifest, and the uploaded attachment id.
      expect(saveDraftMock).toHaveBeenCalledTimes(1);
      const draftArgs = saveDraftMock.mock.calls[0];
      expect(draftArgs[0]).toBe('new-chat');
      const body = draftArgs[1] as string;
      expect(body).toContain('/var/lib/agent-overflow/design/thread-1/main');
      expect(body).toContain('index.html');
      expect(body).toContain('style.css');
      expect(body).toMatch(/screenshot/i);
      expect(draftArgs[2]).toEqual(['att-1']);
    });

    it('still creates the chat thread when capture rejects, just without an attachment', async () => {
      // Capture is best-effort: modern-screenshot is fragile inside
      // our sandbox=allow-scripts iframe (its internal __SANDBOX__
      // helper iframe gets blocked by opaque-origin cross-frame
      // restrictions). The text body — path + manifest — is the
      // load-bearing context for the new chat thread; the screenshot
      // is a nice-to-have. So a capture rejection must NOT cancel the
      // whole pipeline. This test pins that contract: the new thread
      // is created, draft is seeded, but no UploadAttachment fires
      // and the draft has zero attachment ids.
      const pane = await buildPane();
      const { createMock, uploadMock, saveDraftMock, deleteMock } = mockSendToThreadDeps();
      vi.mocked(requestIframeCapture).mockRejectedValueOnce(new Error('iframe gone'));
      const { container, getByTestId } = render(DesignPreviewPanel, {
        props: { ctx: makePanelContext(pane, () => {}) },
      });
      await waitForIframe(container);

      await fireEvent.click(getByTestId('design-send-to-thread'));

      await waitFor(() => expect(saveDraftMock).toHaveBeenCalled());
      expect(createMock).toHaveBeenCalledTimes(1);
      // Skipped — no PNG bytes to upload.
      expect(uploadMock).not.toHaveBeenCalled();
      // Draft was still seeded with the path + manifest, just with an
      // empty attachment list.
      const draftArgs = saveDraftMock.mock.calls[0];
      expect(draftArgs[0]).toBe('new-chat');
      expect(draftArgs[1]).toMatch(/Design context/);
      expect(draftArgs[2]).toEqual([]);
      // Nothing to roll back — every committed write succeeded.
      expect(deleteMock).not.toHaveBeenCalled();
    });

    it('rolls back the orphan thread when SaveDraft fails after CreateThread succeeds', async () => {
      const pane = await buildPane();
      const { createMock, uploadMock, deleteMock } = mockSendToThreadDeps();
      // Override SaveDraft to reject AFTER CreateThread + UploadAttachment land.
      setBindingMock('SaveDraft', async () => {
        throw new Error('disk full');
      });
      const { container, getByTestId } = render(DesignPreviewPanel, {
        props: { ctx: makePanelContext(pane, () => {}) },
      });
      await waitForIframe(container);

      await fireEvent.click(getByTestId('design-send-to-thread'));

      // Wait for the rollback DeleteThread to fire — the load-bearing
      // assertion. Without the rollback the orphan thread sits in the
      // sidebar with no draft / no attachment.
      await waitFor(() => expect(deleteMock).toHaveBeenCalled());
      expect(createMock).toHaveBeenCalled();
      expect(uploadMock).toHaveBeenCalled();
      // DeleteThread targets the just-created thread id.
      expect(deleteMock.mock.calls[0][0]).toBe('new-chat');
    });

    it('debounces rapid double-clicks via the sendingToThread flag', async () => {
      const pane = await buildPane();
      const { createMock } = mockSendToThreadDeps();
      // Hold the capture promise so the second click lands while the
      // first is still in-flight.
      let resolveCapture: ((value: string) => void) | null = null;
      vi.mocked(requestIframeCapture).mockImplementation(
        () => new Promise<string>((res) => {
          resolveCapture = res;
        }),
      );
      const { container, getByTestId } = render(DesignPreviewPanel, {
        props: { ctx: makePanelContext(pane, () => {}) },
      });
      await waitForIframe(container);

      const button = getByTestId('design-send-to-thread') as HTMLButtonElement;
      await fireEvent.click(button);
      // Wait for the first click to set sendingToThread=true (button
      // disabled). Without this synchronization the second click could
      // fire before the first's microtask flushed.
      await waitFor(() => expect(button.disabled).toBe(true));
      await fireEvent.click(button);

      resolveCapture!('ZmFrZS1wbmc=');
      await waitFor(() => expect(createMock).toHaveBeenCalled());
      // The second click was a no-op because sendingToThread was true.
      expect(createMock).toHaveBeenCalledTimes(1);
    });
  });

});
