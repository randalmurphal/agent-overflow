import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import ComposerSlashPopover from './ComposerSlashPopover.svelte';

const commands = ['init', 'review', 'deploy-staging'];

describe('<ComposerSlashPopover>', () => {
  it('renders nothing when closed', () => {
    const { queryByRole } = render(ComposerSlashPopover, {
      props: { open: false, query: '', commands, activeIndex: 0, onSelect: vi.fn() },
    });
    expect(queryByRole('listbox')).toBeNull();
  });

  it('shows the listbox with every command as an option', () => {
    const { getByRole, getAllByRole } = render(ComposerSlashPopover, {
      props: { open: true, query: '', commands, activeIndex: 0, onSelect: vi.fn() },
    });
    expect(getByRole('listbox')).toBeInTheDocument();
    const options = getAllByRole('option');
    expect(options.length).toBe(3);
    expect(options[0].getAttribute('aria-selected')).toBe('true');
    expect(options[1].getAttribute('aria-selected')).toBe('false');
    // Options display with the leading slash so the user sees exactly what
    // will land in the composer.
    expect(options[0].textContent).toMatch(/\/init/);
  });

  it('highlights the active index', () => {
    const { getAllByRole } = render(ComposerSlashPopover, {
      props: { open: true, query: '', commands, activeIndex: 2, onSelect: vi.fn() },
    });
    const options = getAllByRole('option');
    expect(options[0].getAttribute('aria-selected')).toBe('false');
    expect(options[2].getAttribute('aria-selected')).toBe('true');
  });

  it('clicking an option calls onSelect with the command string', async () => {
    const onSelect = vi.fn();
    const { getAllByRole } = render(ComposerSlashPopover, {
      props: { open: true, query: 'rev', commands, activeIndex: 0, onSelect },
    });
    await fireEvent.click(getAllByRole('option')[1]);
    expect(onSelect).toHaveBeenCalledWith('review');
  });

  it('hover notifies the caller so the active index can follow the pointer', async () => {
    const onHover = vi.fn();
    const { getAllByRole } = render(ComposerSlashPopover, {
      props: {
        open: true,
        query: '',
        commands,
        activeIndex: 0,
        onSelect: vi.fn(),
        onHover,
      },
    });
    await fireEvent.mouseEnter(getAllByRole('option')[2]);
    expect(onHover).toHaveBeenCalledWith(2);
  });

  it('renders empty state when no commands are available', () => {
    const { getByText } = render(ComposerSlashPopover, {
      props: { open: true, query: '', commands: [], activeIndex: 0, onSelect: vi.fn() },
    });
    expect(getByText(/No commands available/)).toBeInTheDocument();
  });
});
