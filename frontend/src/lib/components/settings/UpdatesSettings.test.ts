import { describe, expect, it, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import UpdatesSettings from './UpdatesSettings.svelte';
import { getUpdateState, resetForTest } from '../../stores/updates.svelte';

// The panel is a pure projection of the updates store — every RPC it can fire
// is behind a button press or the Advanced disclosure — so these tests drive
// the store directly rather than stubbing bindings.

describe('<UpdatesSettings>', () => {
  beforeEach(() => {
    resetForTest();
  });

  describe('unsupported copy', () => {
    it('keeps the sentences spaced when a current version is known', () => {
      // Regression: the second sentence used to live inside an {#if}, whose
      // block boundary ate the separating space ("…this build.You’re running").
      const s = getUpdateState();
      s.supported = false;
      s.currentVersion = '0.0.9';
      const { container } = render(UpdatesSettings);
      expect(container.textContent).toContain(
        'In-app updates aren’t available for this build. You’re running version 0.0.9.',
      );
    });

    it('renders the first sentence alone when no version is known', () => {
      const s = getUpdateState();
      s.supported = false;
      s.currentVersion = '';
      const { container } = render(UpdatesSettings);
      expect(container.textContent).toContain('In-app updates aren’t available for this build.');
      expect(container.textContent).not.toContain('You’re running version');
    });
  });

  describe('last-apply-failure notice', () => {
    const notice = 'Update to 2.0.0 didn’t apply — still running 1.0.0.';

    it('renders as an error callout above the current-version card', () => {
      const s = getUpdateState();
      s.currentVersion = '1.0.0';
      s.lastApplyFailure = notice;
      const { getByRole, getByText } = render(UpdatesSettings);
      const callout = getByRole('alert');
      expect(callout.textContent).toContain(notice);
      // Ordering is load-bearing: the notice explains the version the card
      // right below it is reporting.
      const card = getByText('Current version');
      expect(callout.compareDocumentPosition(card) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it('stays visible while an update is downloading', () => {
      const s = getUpdateState();
      s.lastApplyFailure = notice;
      s.phase = 'downloading';
      const { getByRole } = render(UpdatesSettings);
      expect(getByRole('alert').textContent).toContain(notice);
    });

    it('renders nothing when empty', () => {
      getUpdateState().currentVersion = '1.0.0';
      const { queryByRole } = render(UpdatesSettings);
      expect(queryByRole('alert')).toBeNull();
    });

    it('is hidden on a session that does not support updates', () => {
      const s = getUpdateState();
      s.supported = false;
      s.lastApplyFailure = notice;
      const { queryByRole, container } = render(UpdatesSettings);
      expect(queryByRole('alert')).toBeNull();
      expect(container.textContent).not.toContain(notice);
    });
  });

  describe('error-phase recovery', () => {
    it('leaves Check for Updates enabled so a failed install can be retried', () => {
      const s = getUpdateState();
      s.phase = 'error';
      s.error = 'Update install failed: swap failed';
      const { getByRole } = render(UpdatesSettings);
      const button = getByRole('button', { name: 'Check for Updates' }) as HTMLButtonElement;
      expect(button.disabled).toBe(false);
    });
  });

  describe('restarting phase', () => {
    it('replaces the Restart button with the in-progress copy and fences the check', () => {
      // On the WSL build the Windows launcher owns the swap and can spend a
      // couple of minutes on it after RestartToUpdate returns; no update
      // action may be reachable in that window.
      const s = getUpdateState();
      s.phase = 'restarting';
      const { container, queryByRole, getByRole } = render(UpdatesSettings);
      expect(container.textContent).toContain('Restarting to apply the update…');
      expect(queryByRole('button', { name: 'Restart to update' })).toBeNull();
      const check = getByRole('button', { name: 'Check for Updates' }) as HTMLButtonElement;
      expect(check.disabled).toBe(true);
    });
  });
});
