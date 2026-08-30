// stores/threadPaneCompanions.test.ts
//
// threadPaneCompanions.ts through the pane: which companion surfaces open
// for a pane, what opening each one requires, and the close-them-all sweep
// a pane clear runs. The registry itself is companionPanes.svelte.test.ts.

import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv, seedThreadPaneLayout } from '../../test/helpers/threadPane';
import { isCompanionOpen, openCompanion } from './companionPanes.svelte';

describe('threadPaneCompanions', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('companion panes', () => {
    it('closes every companion when the pane switches to a different thread', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);
      openCompanion(pane.paneId, 'review');
      expect(pane.showPlanSidebar).toBe(true);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(true);

      await pane.switchThread(makeThread({ id: 'thread-b' }));

      expect(pane.showPlanSidebar).toBe(false);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(false);
      // Switching back does not resurrect them either — companions are
      // per-thread surfaces the user reopens explicitly.
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      expect(pane.showPlanSidebar).toBe(false);
    });

    it('closes take-control when switching to another claude-tui thread (no re-attach)', async () => {
      // The terminal mirror is pinned to the thread it was opened for. It
      // must never silently re-attach to the incoming thread's session —
      // keystrokes would land in the wrong PTY.
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a', provider: 'claude-tui' }));
      openCompanion(pane.paneId, 'take-control');

      await pane.switchThread(makeThread({ id: 'thread-b', provider: 'claude-tui' }));

      expect(isCompanionOpen(pane.paneId, 'take-control')).toBe(false);
    });

    it('keeps companions open on a same-thread re-switch', async () => {
      // A forced in-place reload (same-thread re-switch) reloads items via
      // switchThread(currentThread); an open plan/review pane must
      // survive that.
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      const threadA = makeThread({ id: 'thread-a' });
      await pane.switchThread(threadA);
      pane.setShowPlanSidebar(true);
      openCompanion(pane.paneId, 'review');

      await pane.switchThread(makeThread({ id: 'thread-a' }));

      expect(pane.showPlanSidebar).toBe(true);
      expect(isCompanionOpen(pane.paneId, 'review')).toBe(true);
    });

    it('closes companions when "+ New" starts a draft placeholder in the pane', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      pane.startDraftPlaceholder({
        id: 'p-1',
        path: '/tmp/p1',
        name: 'p1',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      });

      expect(pane.showPlanSidebar).toBe(false);
    });

    it('closes companions when the pane is cleared', async () => {
      const pane = createThreadPane();
      seedThreadPaneLayout(pane.paneId);
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      pane.setShowPlanSidebar(true);

      pane.clear();

      expect(pane.showPlanSidebar).toBe(false);
    });

    it('does not auto-open design preview for a fresh design thread', async () => {
      const pane = await buildPane(
        makeThread({ id: 'thread-a', mode: 'design' }),
      );

      expect(pane.showDesignPreviewPanel).toBe(false);
    });

    it('does not auto-open design preview when options hydrate while closed', async () => {
      const pane = await buildPane(
        makeThread({ id: 'thread-a', mode: 'design' }),
      );
      seedThreadPaneLayout(pane.paneId);
      setBindingMock('LatestDesignOptionSet', async () => ({
        setId: 'set-1',
        optionIds: ['alpha'],
      }));

      await pane.applyDesignOptionsUpdate('thread-a', 'set-1');

      expect(pane.activeOptionSet).toEqual({
        setId: 'set-1',
        optionPaths: ['options/set-1/alpha'],
      });
      expect(pane.showDesignPreviewPanel).toBe(false);

      pane.toggleDesignPreviewPanel();
      expect(pane.showDesignPreviewPanel).toBe(true);
    });

    it('closes design preview when switching to a different design thread', async () => {
      const threadA = makeThread({ id: 'thread-a', mode: 'design' });
      const threadB = makeThread({ id: 'thread-b', mode: 'design' });
      const pane = await buildPane(threadA);
      seedThreadPaneLayout(pane.paneId);
      pane.setShowDesignPreviewPanel(true);

      setBindingMock('SwitchThread', async () => threadB);
      await pane.switchThread(threadB);

      expect(pane.showDesignPreviewPanel).toBe(false);
    });

    it('closes design preview when switching to a non-design thread', async () => {
      const threadA = makeThread({ id: 'thread-a', mode: 'design' });
      const threadB = makeThread({ id: 'thread-b', mode: 'chat' });
      const pane = await buildPane(threadA);
      seedThreadPaneLayout(pane.paneId);
      pane.setShowDesignPreviewPanel(true);

      setBindingMock('SwitchThread', async () => threadB);
      await pane.switchThread(threadB);

      expect(pane.showDesignPreviewPanel).toBe(false);
      expect(isCompanionOpen(pane.paneId, 'design-preview')).toBe(false);
    });
  });
});
