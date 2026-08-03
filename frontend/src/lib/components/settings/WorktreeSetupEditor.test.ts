import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import WorktreeSetupEditor from './WorktreeSetupEditor.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

type WireConfig = { copy: string[]; run: string[][]; timeout: string };

const EMPTY: WireConfig = { copy: [], run: [], timeout: '' };

function seed(stored: WireConfig = EMPTY, onSave?: (config: WireConfig) => WireConfig) {
  let current = stored;
  setBindingMock('GetProjectWorktreeSetup', async () => current);
  setBindingMock('SetProjectWorktreeSetup', async (..._args: never[]) => {
    const config = _args[1] as unknown as WireConfig;
    current = onSave ? onSave(config) : config;
    return current;
  });
}

// Renders and waits out the initial load — every control stays disabled until
// the stored recipe has arrived.
async function mount() {
  const result = render(WorktreeSetupEditor, { props: { projectId: 'p1' } });
  await waitFor(() =>
    expect((result.getByTestId('worktree-setup-copy-add') as HTMLButtonElement).disabled).toBe(
      false,
    ),
  );
  return result;
}

describe('<WorktreeSetupEditor>', () => {
  beforeEach(() => {
    seed();
  });

  it('renders a stored recipe with argv rendered as command lines', async () => {
    seed({
      copy: ['.env', 'config/*.local'],
      run: [
        ['pnpm', 'install', '--frozen-lockfile'],
        ['sh', '-c', 'ln -s "$AO_PROJECT_ROOT/.env" .env'],
      ],
      timeout: '15m',
    });
    const { getByTestId } = await mount();

    expect((getByTestId('worktree-setup-copy-input-0') as HTMLInputElement).value).toBe('.env');
    expect((getByTestId('worktree-setup-copy-input-1') as HTMLInputElement).value).toBe(
      'config/*.local',
    );
    expect((getByTestId('worktree-setup-run-input-0') as HTMLInputElement).value).toBe(
      'pnpm install --frozen-lockfile',
    );
    expect((getByTestId('worktree-setup-run-input-1') as HTMLInputElement).value).toBe(
      `sh -c 'ln -s "$AO_PROJECT_ROOT/.env" .env'`,
    );
    expect((getByTestId('worktree-setup-timeout') as HTMLInputElement).value).toBe('15m');
  });

  // The row is typed as a command line; what crosses the wire is always argv.
  it('saves command lines as argv', async () => {
    const { getByTestId } = await mount();
    await fireEvent.click(getByTestId('worktree-setup-run-add'));
    await fireEvent.input(getByTestId('worktree-setup-run-input-0'), {
      target: { value: `sh -c 'make install'` },
    });
    await fireEvent.click(getByTestId('worktree-setup-copy-add'));
    await fireEvent.input(getByTestId('worktree-setup-copy-input-0'), {
      target: { value: '.env' },
    });
    await fireEvent.click(getByTestId('worktree-setup-save'));

    await waitFor(() =>
      expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls.length).toBe(1),
    );
    expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls[0]).toEqual([
      'p1',
      { copy: ['.env'], run: [['sh', '-c', 'make install']], timeout: '' },
    ]);
  });

  it('blocks the save on an unterminated quote and names the row', async () => {
    const { getByTestId, queryByTestId } = await mount();
    await fireEvent.click(getByTestId('worktree-setup-run-add'));
    await fireEvent.input(getByTestId('worktree-setup-run-input-0'), {
      target: { value: `sh -c 'oops` },
    });

    expect(getByTestId('worktree-setup-run-error-0').textContent).toContain('Unterminated');
    expect((getByTestId('worktree-setup-save') as HTMLButtonElement).disabled).toBe(true);

    await fireEvent.input(getByTestId('worktree-setup-run-input-0'), {
      target: { value: `sh -c 'ok'` },
    });
    expect(queryByTestId('worktree-setup-run-error-0')).toBeNull();
    expect((getByTestId('worktree-setup-save') as HTMLButtonElement).disabled).toBe(false);
  });

  it('blocks the save on a malformed or non-positive timeout', async () => {
    const { getByTestId, queryByTestId } = await mount();
    const timeout = getByTestId('worktree-setup-timeout');

    await fireEvent.input(timeout, { target: { value: 'later' } });
    expect(getByTestId('worktree-setup-timeout-error')).toBeTruthy();
    expect((getByTestId('worktree-setup-save') as HTMLButtonElement).disabled).toBe(true);

    await fireEvent.input(timeout, { target: { value: '0s' } });
    expect(getByTestId('worktree-setup-timeout-error').textContent).toContain('greater than zero');

    await fireEvent.input(timeout, { target: { value: '90s' } });
    expect(queryByTestId('worktree-setup-timeout-error')).toBeNull();
    expect((getByTestId('worktree-setup-save') as HTMLButtonElement).disabled).toBe(false);

    // Blank is the documented default, not an error.
    await fireEvent.input(timeout, { target: { value: '' } });
    expect(queryByTestId('worktree-setup-timeout-error')).toBeNull();
  });

  // Set -> clear -> set again: clearing must not leave the previous recipe
  // behind, and the editor must re-seed from what the backend actually stored.
  it('round-trips through a cleared recipe', async () => {
    seed({ copy: ['.env'], run: [['make', 'install']], timeout: '5m' });
    const { getByTestId, queryByTestId } = await mount();

    await fireEvent.click(getByTestId('worktree-setup-copy-remove-0'));
    await fireEvent.click(getByTestId('worktree-setup-run-remove-0'));
    await fireEvent.input(getByTestId('worktree-setup-timeout'), { target: { value: '' } });
    await fireEvent.click(getByTestId('worktree-setup-save'));

    await waitFor(() =>
      expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls.length).toBe(1),
    );
    expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls[0][1]).toEqual({
      copy: [],
      run: [],
      timeout: '',
    });
    await waitFor(() => expect(queryByTestId('worktree-setup-run-input-0')).toBeNull());

    await fireEvent.click(getByTestId('worktree-setup-run-add'));
    await fireEvent.input(getByTestId('worktree-setup-run-input-0'), {
      target: { value: 'make install' },
    });
    await fireEvent.click(getByTestId('worktree-setup-save'));
    await waitFor(() =>
      expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls.length).toBe(2),
    );
    expect(getBindingMock('SetProjectWorktreeSetup')?.mock.calls[1][1]).toEqual({
      copy: [],
      run: [['make', 'install']],
      timeout: '',
    });
  });

  it('re-seeds from what the backend stored, not from the draft', async () => {
    // The backend drops blank rows; the editor must show that, not its draft.
    seed(EMPTY, () => ({ copy: ['.env'], run: [], timeout: '10m' }));
    const { getByTestId } = await mount();
    await fireEvent.click(getByTestId('worktree-setup-copy-add'));
    await fireEvent.input(getByTestId('worktree-setup-copy-input-0'), {
      target: { value: '.env  ' },
    });
    await fireEvent.click(getByTestId('worktree-setup-save'));

    await waitFor(() =>
      expect((getByTestId('worktree-setup-timeout') as HTMLInputElement).value).toBe('10m'),
    );
    expect((getByTestId('worktree-setup-copy-input-0') as HTMLInputElement).value).toBe('.env');
    // Nothing left to save once the draft matches what was stored.
    expect((getByTestId('worktree-setup-save') as HTMLButtonElement).disabled).toBe(true);
  });

  it('surfaces a save failure inline and keeps the draft', async () => {
    setBindingMock('GetProjectWorktreeSetup', async () => EMPTY);
    setBindingMock('SetProjectWorktreeSetup', async () => {
      throw new Error('run[0]: argv must contain a non-empty executable at index 0');
    });
    const { getByTestId } = await mount();
    await fireEvent.click(getByTestId('worktree-setup-run-add'));
    await fireEvent.input(getByTestId('worktree-setup-run-input-0'), {
      target: { value: 'make install' },
    });
    await fireEvent.click(getByTestId('worktree-setup-save'));

    await waitFor(() =>
      expect(getByTestId('worktree-setup-save-error').textContent).toContain('non-empty executable'),
    );
    expect((getByTestId('worktree-setup-run-input-0') as HTMLInputElement).value).toBe(
      'make install',
    );
  });

  it('reverts the draft to the last saved recipe', async () => {
    seed({ copy: ['.env'], run: [], timeout: '5m' });
    const { getByTestId } = await mount();
    await fireEvent.input(getByTestId('worktree-setup-copy-input-0'), {
      target: { value: 'changed' },
    });
    expect((getByTestId('worktree-setup-revert') as HTMLButtonElement).disabled).toBe(false);
    await fireEvent.click(getByTestId('worktree-setup-revert'));
    expect((getByTestId('worktree-setup-copy-input-0') as HTMLInputElement).value).toBe('.env');
    expect((getByTestId('worktree-setup-revert') as HTMLButtonElement).disabled).toBe(true);
  });
});
