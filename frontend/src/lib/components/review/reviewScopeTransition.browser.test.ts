import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';

let app: ReturnType<typeof mount> | undefined;
let target: HTMLDivElement;

function patch(path: string, text: string): string {
  return `diff --git a/${path} b/${path}\n--- a/${path}\n+++ b/${path}\n@@ -1 +1 @@\n-old\n+${text}\n`;
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  target = document.createElement('div');
  target.style.cssText = 'height:700px;display:flex;position:relative';
  document.body.append(target);
  setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
  setBindingMock('GetGitStatus', async () => ({}));
  setBindingMock('GetWorkspaceCurrentDiff', async () => patch('workspace.ts', 'workspace content'));
  setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: true, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
  setBindingMock('ListThreadEditDiffs', async () => ({
    entries: [1, 2].map(i => ({ itemId: `tool:${i}`, payloadId: `pl-${i}`, turnIndex: 1,
      title: 'Edit', paths: ['repeated.ts'], insertions: 1, deletions: 1, createdAt: i })),
    turnLabels: [{ turnIndex: 1, label: 'Edit twice' }],
  }));
  setBindingMock('GetTurnEditsDiff', async () => ({
    data: patch('repeated.ts', 'first edit') + patch('repeated.ts', 'second edit'),
  }));
  setBindingMock('VerifyEditDiffs', async () => ({ expandablePaths: [] }));
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

afterEach(async () => {
  if (app) await unmount(app);
  app = undefined;
  target.remove();
  __resetReviewPaneStateForTest();
});

for (const width of [400, 1100]) {
  it(`survives Edits → delayed Workspace without duplicate rows at ${width}px`, async () => {
    target.style.width = `${width}px`;
    app = mount(ReviewPane, { target, props: { ctx: makeStubPanelContext() } });
    await vi.waitFor(() => expect(target.textContent).toContain('workspace content'));
    const scope = target.querySelector<HTMLSelectElement>('[data-testid="review-scope-select"]')!;
    const select = (value: string) => {
      scope.value = value;
      scope.dispatchEvent(new Event('change', { bubbles: true }));
    };
    for (let round = 0; round < 2; round++) {
      select('edits');
      await vi.waitFor(() => expect(target.textContent).toContain('second edit'));
      let resolve!: (value: string) => void;
      const pending = new Promise<string>(done => { resolve = done; });
      const read = setBindingMock('GetWorkspaceCurrentDiff', () => pending);
      select('workspace');
      try {
        await vi.waitFor(() => expect(read).toHaveBeenCalled());
        // Let Svelte and the real virtualizer render the pending-load state.
        await new Promise(done => requestAnimationFrame(() => requestAnimationFrame(done)));
        expect(target.textContent).not.toContain('failed to render');
        expect(target.textContent).not.toContain('requires a unique key');
      } finally {
        resolve(patch('workspace.ts', `workspace content ${round}`));
      }
      await vi.waitFor(() => expect(target.textContent).toContain(`workspace content ${round}`));
      expect(target.textContent).not.toContain('second edit');
    }
  });
}
