import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import EditorLink from './EditorLink.svelte';
import { setBindingMock, getBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';

describe('<EditorLink>', () => {
  beforeEach(() => {
    resetBindingMocks();
    // The toast registry is a module-level singleton; drain it so a
    // toast left by a sibling describe block can't satisfy a "find
    // this error" assertion below.
    for (const toast of [...getToasts()]) {
      removeToast(toast.id);
    }
  });

  it('renders inline with the path as text by default', () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(EditorLink, { props: { path: 'src/lib/foo.ts' } });
    const link = getByTestId('editor-link');
    // Inline mode uses a <button> rather than <a href="#"> — Svelte's
    // a11y rule rejects "#"; the visual treatment + accessibility live
    // in the class + aria-label.
    expect(link.tagName).toBe('BUTTON');
    expect(link.textContent).toBe('src/lib/foo.ts');
    expect(link.getAttribute('data-path')).toBe('src/lib/foo.ts');
  });

  it('renders as an icon button when asIcon=true', () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId, queryByTestId } = render(EditorLink, {
      props: { path: 'a/b.ts', asIcon: true },
    });
    expect(queryByTestId('editor-link')).toBeNull();
    const btn = getByTestId('editor-link-icon');
    expect(btn.tagName).toBe('BUTTON');
  });

  it('invokes OpenInEditor with path/line/col/workspacePath on click', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(EditorLink, {
      props: { path: 'src/foo.ts', line: 12, col: 4, workspacePath: '/work' },
    });
    await fireEvent.click(getByTestId('editor-link'));
    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['src/foo.ts', 12, 4, '/work']);
  });

  it('defaults line/col/workspacePath when not supplied', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(EditorLink, { props: { path: 'README.md' } });
    await fireEvent.click(getByTestId('editor-link'));
    await waitFor(() => {
      expect(mock).toHaveBeenCalledTimes(1);
    });
    expect(mock.mock.calls[0]).toEqual(['README.md', 0, 0, '']);
  });

  it('toasts the binding error message verbatim', async () => {
    setBindingMock('OpenInEditor', vi.fn(async () => {
      throw new Error('no editor available — set $EDITOR');
    }));
    const { getByTestId } = render(EditorLink, { props: { path: 'a.ts' } });
    await fireEvent.click(getByTestId('editor-link'));
    await waitFor(() => {
      const toasts = getToasts();
      const match = toasts.find((t) => t.message === 'no editor available — set $EDITOR');
      expect(match?.type).toBe('error');
    });
  });

  it('stops propagation when stopPropagation=true so a wrapping click does not fire', async () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const parentClick = vi.fn();
    const { getByTestId } = render(EditorLink, {
      props: { path: 'a.ts', asIcon: true, stopPropagation: true },
    });
    // Listen on document.body so the event has to bubble through the
    // testing-library container to reach us. The button's
    // stopPropagation should prevent that.
    document.body.addEventListener('click', parentClick);
    try {
      await fireEvent.click(getByTestId('editor-link-icon'));
      expect(parentClick).not.toHaveBeenCalled();
    } finally {
      document.body.removeEventListener('click', parentClick);
    }
  });

  it('lets the click bubble when stopPropagation=false (default)', async () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const parentClick = vi.fn();
    const { getByTestId } = render(EditorLink, {
      props: { path: 'a.ts', asIcon: true },
    });
    document.body.addEventListener('click', parentClick);
    try {
      await fireEvent.click(getByTestId('editor-link-icon'));
      expect(parentClick).toHaveBeenCalledTimes(1);
    } finally {
      document.body.removeEventListener('click', parentClick);
    }
  });

  it('prevents default click behavior so a parent form submit does not fire', async () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(EditorLink, { props: { path: 'a.ts' } });
    const link = getByTestId('editor-link');
    const event = new MouseEvent('click', { bubbles: true, cancelable: true });
    link.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    // Ensure the binding was still invoked despite preventDefault.
    await waitFor(() => {
      const mock = getBindingMock('OpenInEditor');
      expect(mock?.mock.calls.length).toBe(1);
    });
  });

  it('overrides label when provided', () => {
    setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    const { getByTestId } = render(EditorLink, {
      props: { path: 'a/b/c.ts', label: 'c.ts' },
    });
    expect(getByTestId('editor-link').textContent).toBe('c.ts');
  });
});
