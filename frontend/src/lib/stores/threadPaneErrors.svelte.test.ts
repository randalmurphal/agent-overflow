// stores/threadPaneErrors.svelte.test.ts
//
// threadPaneErrors.svelte.ts through the pane: one message per KIND, every
// stored kind rendered as its own banner row in a fixed order, and per-kind
// dismissal. Kinds never displace each other (user ruling 2026-08-25).

import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';

describe('threadPaneErrors', () => {
  beforeEach(installThreadPaneTestEnv);

  // The pane's error surface stores one message PER KIND and renders
  // EVERY stored kind as its own stacked banner row (user ruling
  // 2026-08-25). This replaced the earlier one-slot resolution, whose
  // no-clobber exception silently hid a general error while a
  // history-load banner was up. Kinds never displace each other; each
  // row dismisses independently.
  describe('pane error surface', () => {
    it('stores every kind and lists them in banner-stack order', () => {
      const pane = createThreadPane();
      pane.setHistoryLoadError('Failed to load thread items: boom');
      pane.setGeneralError('Failed to rename thread');
      pane.setSessionError('Session died');

      // session on top, then history-load, then general — fixed order, so
      // rows never reshuffle when a later error lands.
      expect(pane.paneErrorList).toEqual([
        { kind: 'session', message: 'Session died' },
        { kind: 'history-load', message: 'Failed to load thread items: boom' },
        { kind: 'general', message: 'Failed to rename thread' },
      ]);
    });

    it('a general error never touches a live history-load row', () => {
      const pane = createThreadPane();
      pane.setHistoryLoadError('Failed to load thread items: boom');
      pane.setGeneralError('Failed to rename thread');

      expect(pane.paneErrorList).toEqual([
        { kind: 'history-load', message: 'Failed to load thread items: boom' },
        { kind: 'general', message: 'Failed to rename thread' },
      ]);

      // Retry resolving clears only its own row.
      pane.setHistoryLoadError(null);
      expect(pane.paneErrorList).toEqual([
        { kind: 'general', message: 'Failed to rename thread' },
      ]);
    });

    it('a second write of one kind replaces that row only', () => {
      const pane = createThreadPane();
      pane.setSessionError('Session died');
      pane.setGeneralError('Failed to rename thread');
      pane.setSessionError('Session died again');
      expect(pane.paneErrorList).toEqual([
        { kind: 'session', message: 'Session died again' },
        { kind: 'general', message: 'Failed to rename thread' },
      ]);
    });

    it('clearSessionError leaves an orthogonal error visible', () => {
      const pane = createThreadPane();
      pane.setSessionError('Session died');
      pane.setGeneralError('Failed to rename thread');
      pane.clearSessionError();
      expect(pane.paneErrorList).toEqual([
        { kind: 'general', message: 'Failed to rename thread' },
      ]);
    });

    it('per-kind dismiss clears one row; the bare clear drops them all', () => {
      const pane = createThreadPane();
      pane.setHistoryLoadError('Failed to load thread items: boom');
      pane.setGeneralError('Failed to rename thread');
      pane.setSessionError('Session died');
      pane.clearPaneError('history-load');
      expect(pane.paneErrorList.map((e) => e.kind)).toEqual(['session', 'general']);
      pane.clearPaneError();
      expect(pane.paneErrorList).toEqual([]);
      expect(pane.generalError).toBeNull();
    });

    it('generalError/generalErrorKind report the newest stored error', () => {
      const pane = createThreadPane();
      pane.setPaneError('Retry me', 'history-load');
      pane.setPaneError('Untagged');
      expect(pane.generalError).toBe('Untagged');
      expect(pane.generalErrorKind).toBeNull();
      pane.setSessionError('Session died');
      expect(pane.generalErrorKind).toBe('session');
      pane.clearPaneError('session');
      expect(pane.generalError).toBe('Untagged');
    });
  });
});
