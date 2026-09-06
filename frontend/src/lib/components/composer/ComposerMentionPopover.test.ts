import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ComposerMentionPopover from './ComposerMentionPopover.svelte';
import type { WorkspaceFile } from '../../types/workspaceFile';

// happy-dom lacks ResizeObserver; Popover primitive constructs one when
// opened. Minimal stub so construction doesn't throw.
class StubResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

beforeAll(() => {
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver =
    StubResizeObserver as unknown as typeof ResizeObserver;
});

const results: WorkspaceFile[] = [
  { path: 'src/main.ts', kind: 'file', parentPath: 'src' },
  { path: 'src/helper.ts', kind: 'file', parentPath: 'src' },
];

// The popover now routes through the Popover primitive, which computes
// position + visibility from the anchor's rect. Tests need a real
// anchor element in the DOM so the popover is positioned (and thus
// visible to @testing-library's default role queries).
function makeAnchor(): HTMLButtonElement {
  const el = document.createElement('button');
  el.type = 'button';
  document.body.appendChild(el);
  return el;
}

const anchors: HTMLButtonElement[] = [];
function anchor(): HTMLButtonElement {
  const el = makeAnchor();
  anchors.push(el);
  return el;
}

afterEach(() => {
  while (anchors.length) anchors.pop()!.remove();
});

describe('<ComposerMentionPopover>', () => {
  it('renders nothing when closed', () => {
    const { queryByRole } = render(ComposerMentionPopover, {
      props: { backend: '', anchor: anchor(), open: false, query: '', results, activeIndex: 0, onSelect: vi.fn() },
    });
    expect(queryByRole('listbox')).toBeNull();
  });

  it('shows the results with aria roles', () => {
    const { getAllByRole, getByRole } = render(ComposerMentionPopover, {
      props: { backend: '', anchor: anchor(), open: true, query: 'src', results, activeIndex: 0, onSelect: vi.fn() },
    });
    expect(getByRole('listbox')).toBeInTheDocument();
    const options = getAllByRole('option');
    expect(options.length).toBe(2);
    expect(options[0].getAttribute('aria-selected')).toBe('true');
    expect(options[1].getAttribute('aria-selected')).toBe('false');
  });

  it('clicking an option calls onSelect with the file', async () => {
    const onSelect = vi.fn();
    const { getAllByRole } = render(ComposerMentionPopover, {
      props: { backend: '', anchor: anchor(), open: true, query: 'src', results, activeIndex: 0, onSelect },
    });
    await fireEvent.click(getAllByRole('option')[1]);
    expect(onSelect).toHaveBeenCalledWith(results[1]);
  });

  it('renders empty state when no results and not loading', () => {
    const { getByText } = render(ComposerMentionPopover, {
      props: { backend: '', anchor: anchor(), open: true, query: 'zzz', results: [], activeIndex: 0, onSelect: vi.fn() },
    });
    expect(getByText(/No matches/)).toBeInTheDocument();
  });

  it('renders loading state', () => {
    const { getByText } = render(ComposerMentionPopover, {
      props: { backend: '',
        anchor: anchor(),
        open: true,
        query: 'abc',
        results: [],
        activeIndex: 0,
        loading: true,
        onSelect: vi.fn(),
      },
    });
    expect(getByText(/Searching/)).toBeInTheDocument();
  });
});
