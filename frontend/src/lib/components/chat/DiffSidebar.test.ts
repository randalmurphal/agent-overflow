// Smoke test for DiffSidebar. Mounts the component against a real
// pane (via buildPane) with `activeDiffPayload` armed, then drives
// the payload-fetch binding through its three branches: loading,
// empty patch, and fetch error.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, waitFor, fireEvent } from '@testing-library/svelte';
import DiffSidebar from './DiffSidebar.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('<DiffSidebar>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('renders the loading state while the payload preview is in flight', async () => {
    // GetPayloadPreview returns a never-resolving promise so the
    // sidebar stays in the loading branch.
    setBindingMock('GetPayloadPreview', () => new Promise(() => {}));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-loading')).toBeTruthy();
    });
  });

  it('renders the empty state when the patch has no parseable content', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '',
      nextOffset: 0,
      totalSize: 0,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-empty')).toBeTruthy();
    });
  });

  it('renders the error state with a Retry button when the payload fetch rejects', async () => {
    setBindingMock('GetPayloadPreview', vi.fn(async () => {
      throw new Error('payload missing');
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });

    const { getByTestId, getByText } = render(DiffSidebar, { props: { pane } });
    await waitFor(() => {
      expect(getByTestId('diff-sidebar-error')).toBeTruthy();
    });
    expect(getByText('Retry')).toBeTruthy();
  });

  it('clicking close dismisses the sidebar via pane.closeDiffSidebar', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: '',
      nextOffset: 0,
      totalSize: 0,
      isComplete: true,
    }));
    const pane = await buildPane();
    pane.openDiffSidebar({ payloadId: 'p1' });
    expect(pane.activeDiffPayload).not.toBeNull();

    const { getByTestId } = render(DiffSidebar, { props: { pane } });
    await fireEvent.click(getByTestId('diff-sidebar-close'));
    expect(pane.activeDiffPayload).toBeNull();
  });
});
