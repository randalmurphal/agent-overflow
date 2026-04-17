import { describe, expect, it, beforeEach } from 'vitest';
import { render } from '@testing-library/svelte';
import DiffViewer from './DiffViewer.svelte';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import { loadSettings } from '../../../stores/settings.svelte';

beforeEach(async () => {
  setBindingMock('GetSettings', async () => ({ diffWordWrap: false }));
  await loadSettings();
});

describe('<DiffViewer>', () => {
  it('renders an empty-state message when diff is empty', () => {
    const { getByTestId } = render(DiffViewer, { diff: '' });
    expect(getByTestId('diff-viewer-empty').textContent).toContain('No changes.');
  });

  it('uses custom emptyMessage when provided', () => {
    const { getByTestId } = render(DiffViewer, { diff: '', emptyMessage: 'All clean here.' });
    expect(getByTestId('diff-viewer-empty').textContent).toContain('All clean here.');
  });

  it('renders lines synchronously for small diffs', () => {
    const diff = '+foo\n-bar\n context';
    const { getByTestId, queryByTestId } = render(DiffViewer, { diff });
    const viewer = getByTestId('diff-viewer');
    expect(viewer.textContent).toContain('+foo');
    expect(viewer.textContent).toContain('-bar');
    expect(viewer.textContent).toContain(' context');
    // No progress indicator for small diffs.
    expect(queryByTestId('diff-viewer-progress')).toBeNull();
  });

  it('shows a progress indicator when diff exceeds syncLimit', () => {
    // 10 lines, with syncLimit 5. On first render we show exactly syncLimit
    // lines plus the progress chip; the remaining 5 land via setTimeout(0).
    const diff = Array.from({ length: 10 }, (_, i) => `+line-${i}`).join('\n');
    const { getByTestId } = render(DiffViewer, {
      diff,
      syncLimit: 5,
      batchSize: 2,
    });
    const viewer = getByTestId('diff-viewer');
    expect(viewer.textContent).toContain('+line-0');
    expect(viewer.textContent).toContain('+line-4');
    expect(viewer.textContent).not.toContain('+line-5');
    expect(getByTestId('diff-viewer-progress')).toBeInTheDocument();
    expect(getByTestId('diff-viewer-progress').textContent).toContain('5 / 10');
  });

  it('renders every line when the total fits under syncLimit', () => {
    const diff = '+one\n+two\n+three';
    const { getByTestId, queryByTestId } = render(DiffViewer, {
      diff,
      syncLimit: 10,
    });
    expect(getByTestId('diff-viewer').textContent).toContain('+one');
    expect(getByTestId('diff-viewer').textContent).toContain('+three');
    expect(queryByTestId('diff-viewer-progress')).toBeNull();
  });

  it('respects the wordWrap prop override', () => {
    const { getByTestId, rerender } = render(DiffViewer, {
      diff: '+wrap-test',
      wordWrap: true,
    });
    let pre = getByTestId('diff-viewer').querySelector('pre');
    expect(pre).not.toBeNull();
    expect(pre!.className).toContain('whitespace-pre-wrap');

    rerender({ diff: '+wrap-test', wordWrap: false });
    pre = getByTestId('diff-viewer').querySelector('pre');
    expect(pre!.className).toContain('whitespace-pre');
    expect(pre!.className).not.toContain('whitespace-pre-wrap');
  });
});
