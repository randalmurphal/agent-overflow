import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { createComposerDraftStore, resetComposerDraftSnapshotsForTest } from './composerDraft.svelte';
import { registerComposerDraft, getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { moveDraftProject } from './draftProjectMove';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeThread } from '../../test/helpers/chat';
import { noteProject, noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import type { Draft, DraftSnapshot } from '../types/draft';
import type { Project, Thread } from '../types/models';
import { setThreadEnvMode, hasStagedWorktreeIntent, resetForTest as resetWorktreeIntents } from './worktreeIntent.svelte';

vi.mock('./panes.svelte', async (importOriginal) => ({
  ...await importOriginal<typeof import('./panes.svelte')>(),
  mountThreadInPane: vi.fn(async (thread: Thread, pane: ThreadPane) => {
    pane.replaceThread(thread);
    await getComposerDraftForPane(pane.paneId)?.setThread(thread.id);
    return pane;
  }),
}));

const project: Project = { id: 'destination-project', name: 'Destination', path: '/destination', archived: false, sortPosition: 0, createdAt: 0, updatedAt: 0 };
const source = makeThread({ id: 'source', projectId: 'source-project', isDraft: true, mode: 'plan', model: 'gpt-5.4', provider: 'codex', reasoningEffort: 'high', fastMode: true, runtimeMode: 'read-only', contextWindow: 200000 });
const destination = { ...source, id: 'destination', projectId: project.id, projectPath: project.path, workspacePath: project.path, worktreePath: '' };
const empty = (id: string): Draft => ({ threadId: id, content: '', attachmentIds: [], terminalChips: [], sourceProposedPlan: null, updatedAt: 0 });
let saved: Map<string, Draft>;
let dispose: () => void;
let pane: ThreadPane;
let draft: ReturnType<typeof createComposerDraftStore>;

beforeEach(async () => {
  resetComposerDraftSnapshotsForTest();
  resetWorktreeIntents();
  saved = new Map([[source.id, { ...empty(source.id), content: 'original prompt' }]]);
  noteProject(source.projectId!, HOME_BACKEND);
  noteProject(project.id, HOME_BACKEND);
  noteThread(source.id, HOME_BACKEND);
  setBindingMock('GetDraft', async (id: string) => saved.get(id) ?? empty(id));
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('SaveDraft', async (id: string, content: string, attachmentIds: string[], terminalChips: Draft['terminalChips'], sourceProposedPlan: Draft['sourceProposedPlan']) => {
    saved.set(id, { threadId: id, content, attachmentIds, terminalChips, sourceProposedPlan, updatedAt: 0 });
  });
  setBindingMock('CreateThread', async () => destination);
  setBindingMock('MoveDraftToThread', async (id: string, to: string, snapshot: DraftSnapshot) => {
    saved.set(to, { ...snapshot, threadId: to, updatedAt: 0 });
    saved.set(id, empty(id));
    return saved.get(to);
  });
  setBindingMock('DeleteEmptyDraftThread', async () => true);
  pane = createThreadPane({ paneId: 'draft-move-test' });
  pane.replaceThread(source);
  draft = createComposerDraftStore();
  dispose = registerComposerDraft(pane.paneId, draft);
  await draft.setThread(source.id);
});
afterEach(async () => { dispose(); await draft.setThread(null); pane.clear(); resetWorktreeIntents(); });

describe('draft project moves', () => {
  it('retains the empty source with staged worktree settings and starts a normal destination', async () => {
    setThreadEnvMode(source, 'new-worktree');
    const remove = setBindingMock('DeleteEmptyDraftThread', async () => true);
    await moveDraftProject(pane, project, vi.fn());
    expect(saved.get(source.id)?.content).toBe('');
    expect(remove).not.toHaveBeenCalled();
    expect(hasStagedWorktreeIntent(source)).toBe(true);
    expect(hasStagedWorktreeIntent(destination)).toBe(false);
  });

  it('moves unsaved text and selected settings, then hydrates the destination', async () => {
    const create = setBindingMock('CreateThread', async () => destination);
    draft.setContent('the latest prompt');
    expect(await moveDraftProject(pane, project, vi.fn())).toBe(true);
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ projectId: project.id, mode: 'plan', provider: 'codex', model: 'gpt-5.4', reasoningEffort: 'high', fastMode: true, runtimeMode: 'read-only' }));
    expect(create.mock.calls[0][0]).toMatchObject({ worktreePath: undefined, workspaceOverride: undefined });
    expect(draft.content).toBe('the latest prompt');
    expect(pane.thread?.id).toBe(destination.id);
    expect(saved.get(source.id)?.content).toBe('');
    expect(draft.moving).toBe(false);
  });

  it('recovers a lost reply regardless of serialized context property order', async () => {
    draft.addTerminalChip({ id: 'capture', label: 'shell', content: 'output', preview: 'output', createdAt: 1 });
    setBindingMock('MoveDraftToThread', async (id: string, to: string, snapshot: DraftSnapshot) => {
      saved.set(to, { ...snapshot, threadId: to, updatedAt: 1, terminalChips: snapshot.terminalChips.map((chip) => ({ createdAt: chip.createdAt, content: chip.content, preview: chip.preview, label: chip.label, id: chip.id })) });
      saved.set(id, empty(id));
      throw new Error('reply lost');
    });
    expect(await moveDraftProject(pane, project, vi.fn())).toBe(true);
    expect(pane.thread?.id).toBe(destination.id);
    expect(draft.content).toBe('original prompt');
    expect(draft.moving).toBe(false);
  });

  it('carries explicit false settings instead of inheriting destination defaults', async () => {
    pane.replaceThread({ ...source, fastMode: undefined, autoCompactStandardPercent: 72, autoCompactExtendedPercent: 85 });
    const create = setBindingMock('CreateThread', async () => destination);
    await moveDraftProject(pane, project, vi.fn());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ fastMode: false, autoCompactStandardPercent: 72, autoCompactExtendedPercent: 85 }));
  });

  it('carries a paste that materializes its placeholder while the move waits', async () => {
    pane.startDraftPlaceholder({ ...project, id: source.projectId!, path: '/source' }, 'plan', { provider: 'codex', model: 'gpt-5.4' });
    await draft.setThread(null);
    let created!: (thread: Thread) => void;
    let calls = 0;
    setBindingMock('CreateThread', () => ++calls === 1 ? new Promise<Thread>((resolve) => { created = resolve; }) : Promise.resolve(destination));
    const materializing = pane.ensureMaterializedThread();
    const image = { id: 'pasted-image', threadId: source.id, filename: 'paste.png', kind: 'image' as const, mimeType: 'image/png', size: 10, relativePath: 'paste.png', createdAt: 1 };
    setBindingMock('ListAttachments', async (id: string) => [{ ...image, threadId: id }]);
    draft.registerUploadWaiter(async () => {
      await materializing;
      draft.setContent('pasted prompt [Image #1]');
      draft.addAttachment(image);
    });
    const moving = moveDraftProject(pane, project, vi.fn());
    created(source);
    expect(await moving).toBe(true);
    expect(pane.thread?.id).toBe(destination.id);
    expect(draft.content).toBe('pasted prompt [Image #1]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['pasted-image']);
  });

  it('waits for uploads before saving and moving the draft', async () => {
    let release!: () => void;
    const waiting = new Promise<void>((resolve) => { release = resolve; });
    const unregister = draft.registerUploadWaiter(() => waiting);
    const move = setBindingMock('MoveDraftToThread', async () => { throw new Error('stop after snapshot'); });
    const moving = moveDraftProject(pane, project, vi.fn());
    expect(draft.moving).toBe(true);
    expect(move).not.toHaveBeenCalled();
    draft.addAttachment({ id: 'image', threadId: source.id, filename: 'image.png', kind: 'image', mimeType: 'image/png', size: 10, relativePath: 'image.png', createdAt: 1 });
    draft.setContent('prompt [Image #1]');
    release();
    await expect(moving).rejects.toThrow('stop after snapshot');
    expect(move).toHaveBeenCalledWith(source.id, destination.id, expect.objectContaining({ content: 'prompt [Image #1]', attachmentIds: ['image'] }));
    expect(draft.attachments).toHaveLength(1);
    expect(draft.moving).toBe(false);
    unregister();
  });

  it('preserves source text when saving or moving fails', async () => {
    setBindingMock('SaveDraft', async () => { throw new Error('save refused'); });
    const create = setBindingMock('CreateThread', vi.fn());
    draft.setContent('unsaved text');
    await expect(moveDraftProject(pane, project, vi.fn())).rejects.toThrow('save refused');
    expect(create).not.toHaveBeenCalled();
    expect(draft.content).toBe('unsaved text');
    expect(pane.thread?.id).toBe(source.id);
    expect(draft.moving).toBe(false);
    setBindingMock('SaveDraft', async () => {});
  });

  it('does not redirect navigation that happens during an upload', async () => {
    let release!: () => void;
    draft.registerUploadWaiter(() => new Promise<void>((resolve) => { release = resolve; }));
    const create = setBindingMock('CreateThread', vi.fn());
    const moving = moveDraftProject(pane, project, vi.fn());
    pane.replaceThread(makeThread({ id: 'other-thread' }));
    await draft.setThread('other-thread');
    expect(draft.moving).toBe(false);
    release();
    expect(await moving).toBe(false);
    expect(create).not.toHaveBeenCalled();
    expect(pane.thread?.id).toBe('other-thread');
  });

  it('keeps a new move and upload waiter when old owners release', async () => {
    const oldRelease = draft.beginMove();
    await draft.setThread('other');
    const newRelease = draft.beginMove();
    oldRelease();
    expect(draft.moving).toBe(true);
    newRelease();
    expect(draft.moving).toBe(false);
    const oldWaiter = vi.fn(async () => {});
    const newWaiter = vi.fn(async () => {});
    const oldUnregister = draft.registerUploadWaiter(oldWaiter);
    const newUnregister = draft.registerUploadWaiter(newWaiter);
    oldUnregister();
    await draft.settleUploads();
    expect(oldWaiter).not.toHaveBeenCalled();
    expect(newWaiter).toHaveBeenCalledOnce();
    newUnregister();
    await draft.settleUploads();
    expect(newWaiter).toHaveBeenCalledOnce();
  });

  it('rejects overlapping moves and moving a sent conversation', async () => {
    const release = draft.beginMove();
    await expect(moveDraftProject(pane, project, vi.fn())).rejects.toThrow('already changing projects');
    release();
    pane.replaceThread({ ...source, isDraft: false });
    await expect(moveDraftProject(pane, project, vi.fn())).rejects.toThrow('Only an unsent draft');
  });
});
