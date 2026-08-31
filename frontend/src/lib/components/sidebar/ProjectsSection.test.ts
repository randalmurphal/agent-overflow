// The sidebar's import entry point (F5). The catalogue surface itself is
// covered by SessionImportModal.test.ts — what matters here is that the
// header exposes the trigger, that it is inert in a view-only session (the
// visible half of the store's RPC refusal), and that it raises the store's
// `open` rather than owning a second copy of that state.
//
// The modal is deliberately NOT mounted from this section: Sidebar renders
// it only while expanded, so a mod+b collapse mid-run would unmount the
// surface. It hangs off App.svelte with the other store-gated overlays, and
// the assertion below pins that this section raises `open` and nothing else.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import ProjectsSection from './ProjectsSection.svelte';
import { setPageGrantsFromBootstrap } from '../../transport/scopes';
import {
  isSessionImportOpen,
  resetSessionImportForTest,
} from '../../stores/sessionImport.svelte';

function importButton(container: HTMLElement): HTMLButtonElement {
  const icon = container.querySelector('[data-testid="sidebar-import-sessions-icon"]');
  const button = icon?.closest('button');
  if (!button) throw new Error('import trigger not rendered');
  return button as HTMLButtonElement;
}

describe('ProjectsSection import trigger', () => {
  beforeEach(() => {
    resetSessionImportForTest();
    setPageGrantsFromBootstrap(false);
  });

  afterEach(() => {
    setPageGrantsFromBootstrap(false);
    resetSessionImportForTest();
  });

  it('renders an enabled trigger between the sort menu and Add Project', () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    const button = importButton(view.container);

    expect(button).toBeEnabled();
    expect(button).toHaveAttribute('aria-label', 'Import Sessions');
    // No override in a local session: the tooltip mirrors the aria-label.
    expect(button).toHaveAttribute('title', 'Import Sessions');

    const header = button.parentElement;
    const buttons = [...(header?.querySelectorAll('button') ?? [])];
    expect(buttons.indexOf(button)).toBe(1);
    expect(buttons[2]).toHaveAttribute('aria-label', 'Add Project');
  });

  it('raises the store flag and mounts nothing of its own', async () => {
    const view = render(ProjectsSection, { props: { pane: null } });
    expect(isSessionImportOpen()).toBe(false);

    await fireEvent.click(importButton(view.container));

    expect(isSessionImportOpen()).toBe(true);
    // The surface lives in App.svelte; a section that mounted its own copy
    // would take it down with the sidebar on collapse.
    expect(view.queryByTestId('session-import-body')).not.toBeInTheDocument();
  });

  it('is disabled with a Local only tooltip in a view-only session', async () => {
    setPageGrantsFromBootstrap(true);
    const view = render(ProjectsSection, { props: { pane: null } });
    const button = importButton(view.container);

    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('title', 'Local only');
    // The action still has a name for assistive tech; only the hover text
    // explains why it is unavailable.
    expect(button).toHaveAttribute('aria-label', 'Import Sessions');

    await fireEvent.click(button);
    expect(isSessionImportOpen()).toBe(false);
  });
});
