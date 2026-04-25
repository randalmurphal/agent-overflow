import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import RemoteEndpointForm from './RemoteEndpointForm.svelte';

describe('<RemoteEndpointForm>', () => {
  it('renders the existing values when in edit mode', async () => {
    const { getByLabelText } = render(RemoteEndpointForm, {
      props: {
        mode: 'edit',
        initialName: 'My machine',
        initialURL: 'ws://10.0.0.5:54321/',
        initialToken: 'tok-existing',
        initialError: null,
        saving: false,
        onSubmit: () => {},
        onCancel: () => {},
      },
    });
    const nameInput = getByLabelText('Nickname (optional)') as HTMLInputElement;
    const urlInput = getByLabelText('URL') as HTMLInputElement;
    const tokenInput = getByLabelText('Token') as HTMLInputElement;
    expect(nameInput.value).toBe('My machine');
    expect(urlInput.value).toBe('ws://10.0.0.5:54321/');
    expect(tokenInput.value).toBe('tok-existing');
  });

  it('calls onSubmit with trimmed values when the form is valid', async () => {
    const onSubmit = vi.fn();
    const { findByRole, getByLabelText } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: null,
        saving: false,
        onSubmit,
        onCancel: () => {},
      },
    });
    await fireEvent.input(getByLabelText('Nickname (optional)'), {
      target: { value: '  Tailnet  ' },
    });
    await fireEvent.input(getByLabelText('URL'), {
      target: { value: '  ws://10.0.0.5:54321/  ' },
    });
    await fireEvent.input(getByLabelText('Token'), {
      target: { value: '  tok-x  ' },
    });
    await fireEvent.click(await findByRole('button', { name: 'Add' }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(onSubmit).toHaveBeenCalledWith({
      name: 'Tailnet',
      url: 'ws://10.0.0.5:54321/',
      token: 'tok-x',
    });
  });

  it('rejects http:// in the URL with a client-side error and does not call onSubmit', async () => {
    const onSubmit = vi.fn();
    const { findByRole, getByLabelText, findByText } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: null,
        saving: false,
        onSubmit,
        onCancel: () => {},
      },
    });
    await fireEvent.input(getByLabelText('URL'), {
      target: { value: 'http://example.com/' },
    });
    await fireEvent.input(getByLabelText('Token'), {
      target: { value: 'tok' },
    });
    await fireEvent.click(await findByRole('button', { name: 'Add' }));
    await findByText(/URL must start with ws:\/\/ or wss:\/\//i);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('rejects an empty token before calling onSubmit', async () => {
    const onSubmit = vi.fn();
    const { getByLabelText } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: null,
        saving: false,
        onSubmit,
        onCancel: () => {},
      },
    });
    await fireEvent.input(getByLabelText('URL'), {
      target: { value: 'ws://10.0.0.5:54321/' },
    });
    // Don't fill in the token. The browser-level `required`
    // attribute blocks form submission, so dispatch the submit event
    // directly to bypass the native validation and exercise the
    // client validator.
    const form = getByLabelText('URL').closest('form') as HTMLFormElement;
    const evt = new SubmitEvent('submit', { cancelable: true, bubbles: true });
    form.dispatchEvent(evt);
    await waitFor(() => {
      expect(onSubmit).not.toHaveBeenCalled();
    });
  });

  it('calls onCancel when the Cancel button is clicked', async () => {
    const onCancel = vi.fn();
    const { findByRole } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: null,
        saving: false,
        onSubmit: () => {},
        onCancel,
      },
    });
    await fireEvent.click(await findByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('renders an error passed via initialError', async () => {
    const { findByText } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: 'Server rejected the request',
        saving: false,
        onSubmit: () => {},
        onCancel: () => {},
      },
    });
    await findByText(/Server rejected the request/i);
  });

  it('disables the submit button while saving', async () => {
    const { findByRole } = render(RemoteEndpointForm, {
      props: {
        mode: 'add',
        initialName: '',
        initialURL: '',
        initialToken: '',
        initialError: null,
        saving: true,
        onSubmit: () => {},
        onCancel: () => {},
      },
    });
    const submit = (await findByRole('button', { name: 'Add' })) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
  });

  it('renders Save instead of Add when mode is edit', async () => {
    const { findByRole } = render(RemoteEndpointForm, {
      props: {
        mode: 'edit',
        initialName: 'x',
        initialURL: 'ws://h:1/',
        initialToken: 'tok',
        initialError: null,
        saving: false,
        onSubmit: () => {},
        onCancel: () => {},
      },
    });
    await findByRole('button', { name: 'Save' });
  });
});
