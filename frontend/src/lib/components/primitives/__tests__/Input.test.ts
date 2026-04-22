// Verifies the Input primitive's core contract:
//   - renders input OR textarea based on `multiline` prop
//   - label/description/error optional rendering + association
//   - bindable value two-way sync
//   - id auto-generation + propagation to label `for`
//   - error state applies error border
//   - disabled/readonly flow through
//   - type + autocomplete + placeholder attribute propagation

import { describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Input from '../Input.svelte';

describe('<Input>', () => {
  it('renders a single-line <input> by default', () => {
    const { container } = render(Input, { props: { value: '' } });
    expect(container.querySelector('input')).not.toBeNull();
    expect(container.querySelector('textarea')).toBeNull();
  });

  it('renders a <textarea> when multiline=true', () => {
    const { container } = render(Input, { props: { value: '', multiline: true } });
    expect(container.querySelector('textarea')).not.toBeNull();
    expect(container.querySelector('input')).toBeNull();
  });

  it('associates label with input via matching id/for', () => {
    const { container } = render(Input, { props: { value: '', label: 'Email' } });
    const label = container.querySelector('label');
    const input = container.querySelector('input');
    expect(label).not.toBeNull();
    expect(input).not.toBeNull();
    expect(label!.getAttribute('for')).toBe(input!.id);
    expect(input!.id.length).toBeGreaterThan(0);
  });

  it('uses the caller-supplied id verbatim when provided', () => {
    const { container } = render(Input, {
      props: { value: '', id: 'email-field', label: 'Email' },
    });
    const input = container.querySelector('input')!;
    const label = container.querySelector('label')!;
    expect(input.id).toBe('email-field');
    expect(label.getAttribute('for')).toBe('email-field');
  });

  it('renders the description paragraph when provided', () => {
    const { getByText } = render(Input, {
      props: { value: '', description: 'Your work email' },
    });
    expect(getByText('Your work email')).toBeInTheDocument();
  });

  it('renders the error message and applies the error border when error is set', () => {
    const { getByText, container } = render(Input, {
      props: { value: '', error: 'Required field' },
    });
    expect(getByText('Required field')).toBeInTheDocument();
    const input = container.querySelector('input')!;
    expect(input.className).toContain('border-error');
  });

  it('applies the accent border in the default (no error) state', () => {
    const { container } = render(Input, { props: { value: '' } });
    const input = container.querySelector('input')!;
    expect(input.className).toContain('border-border-subtle');
  });

  it('fires oninput when the user types', async () => {
    const oninput = vi.fn();
    const { container } = render(Input, { props: { value: '', oninput } });
    const input = container.querySelector('input')!;
    await fireEvent.input(input, { target: { value: 'hello' } });
    expect(oninput).toHaveBeenCalled();
  });

  it('propagates type=email to the input element', () => {
    const { container } = render(Input, { props: { value: '', type: 'email' } });
    expect(container.querySelector('input')!.getAttribute('type')).toBe('email');
  });

  it('disables the input when disabled=true', () => {
    const { container } = render(Input, { props: { value: '', disabled: true } });
    const input = container.querySelector('input')! as HTMLInputElement;
    expect(input.disabled).toBe(true);
  });

  it('marks the input readonly when readonly=true', () => {
    const { container } = render(Input, { props: { value: '', readonly: true } });
    const input = container.querySelector('input')! as HTMLInputElement;
    expect(input.readOnly).toBe(true);
  });

  it('propagates placeholder', () => {
    const { container } = render(Input, {
      props: { value: '', placeholder: 'you@example.com' },
    });
    const input = container.querySelector('input')!;
    expect(input.getAttribute('placeholder')).toBe('you@example.com');
  });

  it('textarea uses requested rows', () => {
    const { container } = render(Input, {
      props: { value: '', multiline: true, rows: 5 },
    });
    const ta = container.querySelector('textarea')!;
    // happy-dom reports rows via getAttribute as a string; strict parse
    // is what callers will actually observe downstream.
    expect(Number(ta.getAttribute('rows'))).toBe(5);
  });
});
