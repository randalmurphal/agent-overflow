// The rewrite ARMS, in the app rather than in the parser.
//
// `previewLinkExtension.test.ts` proves what the tokenizer does when it is
// handed a target, and `ChatMarkdown.compactStaticLinkUrls.test.ts` proves
// both renderers spell that target the same way. Neither answers the
// question that decides whether any of it is ever seen: does ChatMarkdown
// build the extension for the thread it is rendering, and does it rebuild
// when the machine's list changes?
//
// So this file renders the real component against the real store, with a
// staged second backend and the `devserver:list` frame arriving after the
// first paint. It is also where the ordering with the path-link extension
// is pinned: that one claims every `[…](…)` link it is offered, so a
// preview link behind it would render plain.

import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import ChatMarkdown from './ChatMarkdown.svelte';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../../test/helpers/backends';
import { grantBackendScopes, resetToLocalPage, revokeBackendScopes } from '../../../test/helpers/scopes';
import { __resetScopesForTest } from '../../transport/scopes';
import { forgetThread, noteThread } from '../../transport/entityIndex';
import { initDevServers, resetDevServersForTest } from '../../stores/devServers.svelte';
import type { DevServerList } from '../../stores/devServers.svelte';

const THREAD = 'thread-on-laptop';
const SOURCE = 'Try [the app](http://localhost:5173/app) when it is up.';

function frame(overrides: Partial<DevServerList> = {}): DevServerList {
  return {
    servers: [{ port: 5173, allowed: true, source: 'attributed', listening: true }],
    previewHost: 'laptop.tail.ts.net',
    ...overrides,
  };
}

function previewAnchor(container: HTMLElement): HTMLElement | null {
  return container.querySelector<HTMLElement>('a[data-preview-port]');
}

describe('ChatMarkdown arms the preview rewrite', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    resetDevServersForTest();
    resetStagedBackends();
    resetToLocalPage();
    noteThread(THREAD, 'laptop');
  });

  afterEach(() => {
    forgetThread(THREAD);
    resetDevServersForTest();
    resetStagedBackends();
    revokeBackendScopes('laptop');
    resetToLocalPage();
    __resetScopesForTest();
  });

  it('leaves the link plain until that machine has answered once', async () => {
    stageBackend();
    const { container } = render(ChatMarkdown, { source: SOURCE, threadId: THREAD });
    await waitFor(() => expect(container.textContent).toContain('the app'));
    expect(previewAnchor(container)).toBeNull();
    expect(container.querySelector('a[href="http://localhost:5173/app"]')).not.toBeNull();
  });

  it('rewrites the link when the list frame arrives, without a remount', async () => {
    stageBackend();
    initDevServers();
    const { container } = render(ChatMarkdown, { source: SOURCE, threadId: THREAD });
    await waitFor(() => expect(container.textContent).toContain('the app'));

    emitWailsEvent('devserver:list', frame(), REMOTE_BACKEND_UUID);

    await waitFor(() => expect(previewAnchor(container)).not.toBeNull());
    const link = previewAnchor(container);
    expect(link?.dataset.previewState).toBe('open');
    expect(link?.dataset.previewPort).toBe('5173');
    expect(link?.dataset.previewPath).toBe('/app');
    expect(link?.dataset.previewThread).toBe(THREAD);
    expect(link?.dataset.previewMachine).toBe('Laptop');
    // The href is still what the agent wrote, so copying it copies that.
    expect(link?.getAttribute('href')).toBe('http://localhost:5173/app');
  });

  it('turns the same link inert when the machine stops sharing the port', async () => {
    stageBackend();
    initDevServers();
    const { container } = render(ChatMarkdown, { source: SOURCE, threadId: THREAD });
    emitWailsEvent('devserver:list', frame(), REMOTE_BACKEND_UUID);
    await waitFor(() => expect(previewAnchor(container)?.dataset.previewState).toBe('open'));

    emitWailsEvent(
      'devserver:list',
      frame({ servers: [{ port: 5173, allowed: false, source: 'seen', listening: true }] }),
      REMOTE_BACKEND_UUID,
    );

    await waitFor(() =>
      expect(previewAnchor(container)?.dataset.previewState).toBe('not-shared'),
    );
    // No sharing control: this session was never granted access:admin there.
    expect(container.querySelector('button[data-preview-allow]')).toBeNull();
  });

  it('offers the sharing control to a session that may share ports there', async () => {
    stageBackend();
    initDevServers();
    grantBackendScopes('laptop', ['access:admin']);
    const { container } = render(ChatMarkdown, { source: SOURCE, threadId: THREAD });
    emitWailsEvent(
      'devserver:list',
      frame({ servers: [{ port: 5173, allowed: false, source: 'seen', listening: true }] }),
      REMOTE_BACKEND_UUID,
    );

    await waitFor(() =>
      expect(container.querySelector('button[data-preview-allow]')).not.toBeNull(),
    );
    const button = container.querySelector<HTMLElement>('button[data-preview-allow]');
    expect(button?.dataset.previewAllow).toBe('5173');
    expect(button?.dataset.previewBackend).toBe('laptop');
  });

  it('leaves a surface with no thread alone, because localhost there is nobody’s', async () => {
    stageBackend();
    initDevServers();
    const { container } = render(ChatMarkdown, { source: SOURCE });
    emitWailsEvent('devserver:list', frame(), REMOTE_BACKEND_UUID);
    await waitFor(() => expect(container.textContent).toContain('the app'));
    expect(previewAnchor(container)).toBeNull();
  });

  it('still rewrites with the path-link extension armed on the same surface', async () => {
    stageBackend();
    initDevServers();
    const { container } = render(ChatMarkdown, {
      source: SOURCE,
      threadId: THREAD,
      workspacePath: '/repo',
      pathRefs: [{ path: '/repo/README.md' } as never],
    });
    emitWailsEvent('devserver:list', frame(), REMOTE_BACKEND_UUID);
    await waitFor(() => expect(previewAnchor(container)?.dataset.previewState).toBe('open'));
  });
});
