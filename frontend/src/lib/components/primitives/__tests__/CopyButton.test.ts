// Verifies the CopyButton primitive's contract:
//   - clicking calls navigator.clipboard.writeText with the resolved text
//   - aria-label flips to the copied label after a successful write
//   - sync getter and async getter forms both work
//   - empty text (string, sync getter, async getter) is a no-op
//   - the copied label resets back to the default after the 2s timer
//   - unmount mid-timer does not throw / leak the timer
//   - clipboard failure invokes the optional `onError` callback so the
//     primitive itself can stay leaf-pure (no `stores/` import)

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import CopyButton from '../CopyButton.svelte';

const writeText = vi.fn(async (_text: string) => {});

beforeEach(() => {
  writeText.mockReset();
  writeText.mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', {
    value: { writeText },
    configurable: true,
    writable: true,
  });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('<CopyButton>', () => {
  it('writes the text prop to the clipboard on click', async () => {
    const { getByRole } = render(CopyButton, { props: { text: 'hello world' } });
    await fireEvent.click(getByRole('button'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('hello world'));
  });

  it('flips to the copied label after a successful write', async () => {
    const { getByRole } = render(CopyButton, { props: { text: 'x' } });
    const button = getByRole('button');
    expect(button.getAttribute('aria-label')).toBe('Copy');
    await fireEvent.click(button);
    await waitFor(() => expect(button.getAttribute('aria-label')).toBe('Copied'));
  });

  it('resolves a sync getter form', async () => {
    let count = 0;
    const getText = () => {
      count += 1;
      return 'lazy';
    };
    const { getByRole } = render(CopyButton, { props: { text: getText } });
    expect(count).toBe(0); // not invoked at render time
    await fireEvent.click(getByRole('button'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('lazy'));
    expect(count).toBe(1);
  });

  it('resolves an async getter form', async () => {
    const { getByRole } = render(CopyButton, {
      props: { text: () => Promise.resolve('async value') },
    });
    await fireEvent.click(getByRole('button'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('async value'));
  });

  it('is a no-op when text resolves empty', async () => {
    const { getByRole } = render(CopyButton, { props: { text: '' } });
    await fireEvent.click(getByRole('button'));
    // Wait a tick to ensure the click handler ran fully.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(writeText).not.toHaveBeenCalled();
  });

  it('is a no-op when a sync getter resolves empty', async () => {
    const { getByRole } = render(CopyButton, { props: { text: () => '' } });
    await fireEvent.click(getByRole('button'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(writeText).not.toHaveBeenCalled();
  });

  it('is a no-op when an async getter resolves empty', async () => {
    const { getByRole } = render(CopyButton, {
      props: { text: () => Promise.resolve('') },
    });
    await fireEvent.click(getByRole('button'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(writeText).not.toHaveBeenCalled();
  });

  it('resets the label back to the default after 2 seconds', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { getByRole } = render(CopyButton, { props: { text: 'x' } });
    const button = getByRole('button');
    await fireEvent.click(button);
    await vi.waitFor(() => expect(button.getAttribute('aria-label')).toBe('Copied'));
    await vi.advanceTimersByTimeAsync(2000);
    expect(button.getAttribute('aria-label')).toBe('Copy');
  });

  it('cleans up its timer on unmount without throwing', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { getByRole, unmount } = render(CopyButton, { props: { text: 'x' } });
    await fireEvent.click(getByRole('button'));
    await vi.waitFor(() => expect(getByRole('button').getAttribute('aria-label')).toBe('Copied'));
    expect(() => unmount()).not.toThrow();
    // Advance past the reset window. If clearTimeout didn't fire on
    // unmount we would still see the timer callback execute, but with
    // the component gone there's no observable side effect — we just
    // need to confirm nothing throws on the now-detached state.
    await vi.advanceTimersByTimeAsync(2500);
  });

  it('invokes onError when navigator.clipboard.writeText rejects', async () => {
    writeText.mockRejectedValueOnce(new Error('denied'));
    const onError = vi.fn();
    const { getByRole } = render(CopyButton, {
      props: { text: 'x', onError },
    });
    const button = getByRole('button');
    await fireEvent.click(button);
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
    // Failure leaves the icon in its default Copy state — no fake flip.
    expect(button.getAttribute('aria-label')).toBe('Copy');
  });

  it('invokes onError when an async text getter rejects', async () => {
    const onError = vi.fn();
    const { getByRole } = render(CopyButton, {
      props: {
        text: async () => {
          throw new Error('load failed');
        },
        onError,
      },
    });
    const button = getByRole('button');
    await fireEvent.click(button);
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
    expect(writeText).not.toHaveBeenCalled();
    expect(button.getAttribute('aria-label')).toBe('Copy');
  });

  it('does not throw when clipboard fails and no onError is provided', async () => {
    writeText.mockRejectedValueOnce(new Error('denied'));
    const { getByRole } = render(CopyButton, { props: { text: 'x' } });
    const button = getByRole('button');
    await fireEvent.click(button);
    // Just wait a tick so the async handler resolves; no assertion beyond
    // "did not throw" — the absence of an error is the contract.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(button.getAttribute('aria-label')).toBe('Copy');
  });

  // `write` is how a markdown surface opts into the dual-flavor
  // clipboard (`copyMarkdownToClipboard`) without the primitives layer
  // importing the markdown pipeline.
  describe('write injection', () => {
    it('routes the resolved text through an injected writer', async () => {
      const write = vi.fn(async (_value: string) => true);
      const { getByRole } = render(CopyButton, {
        props: { text: async () => 'md **body**', write },
      });
      const button = getByRole('button');
      await fireEvent.click(button);
      await waitFor(() => expect(write).toHaveBeenCalledWith('md **body**', expect.any(MouseEvent)));
      expect(writeText).not.toHaveBeenCalled();
      await waitFor(() => expect(button.getAttribute('aria-label')).toBe('Copied'));
    });

    it('treats a false result from the injected writer as a failure', async () => {
      const onError = vi.fn();
      const { getByRole } = render(CopyButton, {
        props: { text: 'x', write: async () => false, onError },
      });
      const button = getByRole('button');
      await fireEvent.click(button);
      await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
      expect(button.getAttribute('aria-label')).toBe('Copy');
    });
  });
});
