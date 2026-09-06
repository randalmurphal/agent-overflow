import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import FirstRunScreen from './FirstRunScreen.svelte';

const mocks = vi.hoisted(() => ({ adopt: vi.fn(), scan: vi.fn(), parse: vi.fn() }));
vi.mock('../../native/boot', () => ({ adoptPairingEndpoint: mocks.adopt }));
vi.mock('../../native/qr', () => ({ scanPairingQr: mocks.scan }));
vi.mock('../../transport/backendAttach', () => ({ payloadFromLink: mocks.parse }));
const payload = { v: 1, backendId: 'host', backendName: 'GPU', endpoint: 'https://gpu.example', token: 'invitation' };

describe('phone invitation entry', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    mocks.adopt.mockReturnValue('');
    mocks.parse.mockReturnValue(payload);
  });

  it('passes a pasted invitation through the same validation and endpoint adoption as scanning', async () => {
    const onScanned = vi.fn();
    const ui = render(FirstRunScreen, { onScanned });
    await fireEvent.click(ui.getByRole('button', { name: 'Use a link' }));
    expect(ui.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    await fireEvent.input(ui.getByLabelText('Pairing link'), { target: { value: ' https://gpu.example/#pair=invite ' } });
    await fireEvent.submit(ui.getByLabelText('Pairing link').closest('form')!);
    expect(mocks.parse).toHaveBeenCalledWith('https://gpu.example/#pair=invite');
    expect(mocks.adopt).toHaveBeenCalledWith(payload);
    expect(onScanned).toHaveBeenCalledWith(payload);
  });

  it('keeps malformed or refused links editable without continuing', async () => {
    const onScanned = vi.fn();
    const ui = render(FirstRunScreen, { onScanned });
    await fireEvent.click(ui.getByRole('button', { name: 'Use a link' }));
    const field = ui.getByLabelText('Pairing link');
    const form = field.closest('form')!;
    mocks.parse.mockImplementationOnce(() => { throw new Error('Invalid invitation'); });
    await fireEvent.input(field, { target: { value: 'broken link' } });
    await fireEvent.submit(form);
    expect(ui.getByRole('alert').textContent).toBe('Invalid invitation');
    expect(mocks.adopt).not.toHaveBeenCalled();
    mocks.adopt.mockReturnValueOnce('This computer requires HTTPS.');
    await fireEvent.input(field, { target: { value: 'http://gpu/#pair=invite' } });
    await fireEvent.submit(form);
    expect(ui.getByRole('alert').textContent).toBe('This computer requires HTTPS.');
    expect(onScanned).not.toHaveBeenCalled();
    await fireEvent.submit(form);
    expect(ui.queryByRole('alert')).toBeNull();
    expect(onScanned).toHaveBeenCalledWith(payload);
  });

  it('preserves scan cancellation and the scanned invitation path', async () => {
    const onScanned = vi.fn();
    const ui = render(FirstRunScreen, { onScanned });
    mocks.scan.mockResolvedValueOnce(null).mockResolvedValueOnce('https://gpu/#pair=invite');
    await fireEvent.click(ui.getByRole('button', { name: 'Scan code' }));
    await waitFor(() => expect(ui.getByRole('button', { name: 'Scan code' })).toBeTruthy());
    expect(mocks.parse).not.toHaveBeenCalled();
    expect(ui.queryByRole('alert')).toBeNull();
    await fireEvent.click(ui.getByRole('button', { name: 'Scan code' }));
    await waitFor(() => expect(onScanned).toHaveBeenCalledWith(payload));
    expect(mocks.adopt).toHaveBeenCalledWith(payload);
  });
});
