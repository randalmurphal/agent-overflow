// Real-Chromium suite for a ticket's typed field: Chromium's number and
// date inputs hold an empty value for text that is not a number or not a
// whole date, which is how a person clears the field. Enter on such text
// saves nothing and says what the field takes; Enter on an empty field
// clears it. Text being typed outlives the field going, and comes back
// with it taking no focus and moving no scroll.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import TicketField, { type FieldEditor } from './TicketField.svelte';
import { ticketWriteSurfaceContext } from './ticketWriteSurface';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function mountField(input: 'number' | 'date', text: string) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const saves: string[] = [];
  const editor: FieldEditor = { kind: 'type', input, text, onSave: (t) => { saves.push(t); } };
  const app = mount(TicketField, {
    target: host,
    context: ticketWriteSurfaceContext(() => true),
    props: { label: 'Points', field: 'points', value: text, editor, ticketKey: 'k', target: 'points', fieldName: (id: string) => id },
  });
  mounted.push({ app, host });
  return { host, saves };
}

async function typeInto(host: HTMLElement, keys: string): Promise<HTMLInputElement> {
  (host.querySelector('[data-testid="ticket-field-value"]') as HTMLElement).click();
  const find = () => host.querySelector<HTMLInputElement>('[data-testid="ticket-field-input"]');
  await waitFor(() => find() !== null, 'the field input');
  const input = find()!;
  await userEvent.clear(input);
  if (keys) await userEvent.type(input, keys);
  await userEvent.keyboard('{Enter}');
  return input;
}

describe('a typed ticket field', () => {
  it('saves nothing for text that is not a number and says what the field takes', async () => {
    const { host, saves } = mountField('number', '3');
    const input = await typeInto(host, '1e');
    expect(input.validity.badInput).toBe(true);
    expect(saves).toEqual([]);
    expect(host.querySelector('[data-testid="ticket-field-invalid"]')?.textContent).toBe('Type a number, or clear the field.');
    expect(host.querySelector('[data-testid="ticket-field-input"]')).not.toBeNull();
  });

  it('saves nothing for a number too large to hold', async () => {
    const { host, saves } = mountField('number', '3');
    const input = await typeInto(host, '1e400');
    expect(input.validity.badInput).toBe(true);
    expect(saves).toEqual([]);
    expect(host.querySelector('[data-testid="ticket-field-invalid"]')?.textContent).toBe('Type a number, or clear the field.');
  });

  it('saves a number and clears an emptied field', async () => {
    const first = mountField('number', '3');
    await typeInto(first.host, '8');
    expect(first.saves).toEqual(['8']);
    const second = mountField('number', '3');
    await typeInto(second.host, '');
    expect(second.saves).toEqual(['']);
  });

  it('saves nothing for a date not whole', async () => {
    const { host, saves } = mountField('date', '2026-01-02');
    const input = await typeInto(host, '03');
    expect(input.validity.badInput).toBe(true);
    expect(saves).toEqual([]);
    expect(host.querySelector('[data-testid="ticket-field-invalid"]')?.textContent).toBe('Pick a whole date, or clear the field.');
  });

  it('leaves the text being typed with its holder when the field goes while focused, and takes it up again', async () => {
    const held: { text: string | null } = { text: null };
    const editor: FieldEditor = { kind: 'type', input: 'text', text: '', onSave: () => {}, typing: { get text() { return held.text; }, set: (t) => { held.text = t; } } };
    const props = { label: 'Team', field: 'team', value: '', editor, ticketKey: 'k', target: 'team', fieldName: (id: string) => id };
    const host = document.createElement('div');
    document.body.appendChild(host);
    const app = mount(TicketField, { target: host, context: ticketWriteSurfaceContext(() => true), props });
    (host.querySelector('[data-testid="ticket-field-value"]') as HTMLElement).click();
    await waitFor(() => host.querySelector('[data-testid="ticket-field-input"]') !== null, 'the field input');
    const input = host.querySelector<HTMLInputElement>('[data-testid="ticket-field-input"]')!;
    await userEvent.type(input, 'OPS');
    expect(document.activeElement).toBe(input);
    // The field goes as a virtual row does, its input focused.
    unmount(app);
    host.remove();
    expect(held.text).toBe('OPS');
    const again = document.createElement('div');
    document.body.appendChild(again);
    mounted.push({ app: mount(TicketField, { target: again, context: ticketWriteSurfaceContext(() => true), props }), host: again });
    await waitFor(() => again.querySelector('[data-testid="ticket-field-input"]') !== null, 'the field input again');
    expect(again.querySelector<HTMLInputElement>('[data-testid="ticket-field-input"]')!.value).toBe('OPS');
  });

  it('takes up the text being typed when the field comes back, leaving focus and the scroll where the person has them', async () => {
    const held: { text: string | null } = { text: 'OPS' };
    const editor: FieldEditor = { kind: 'type', input: 'text', text: '', onSave: () => {}, typing: { get text() { return held.text; }, set: (t) => { held.text = t; } } };
    const props = { label: 'Team', field: 'team', value: '', editor, ticketKey: 'k', target: 'team', fieldName: (id: string) => id };
    // A scrolled list: the person is at its top, in another control, and
    // the field comes back below the fold, as a virtual row does.
    const scroller = document.createElement('div');
    scroller.style.cssText = 'height: 200px; overflow-y: auto;';
    const other = document.createElement('button');
    other.textContent = 'Other';
    const spacer = document.createElement('div');
    spacer.style.height = '1000px';
    const host = document.createElement('div');
    scroller.append(other, spacer, host);
    document.body.appendChild(scroller);
    other.focus();
    const top = scroller.scrollTop;
    mounted.push({ app: mount(TicketField, { target: host, context: ticketWriteSurfaceContext(() => true), props }), host: scroller });
    await waitFor(() => host.querySelector('[data-testid="ticket-field-input"]') !== null, 'the field input');
    // An effect's focus runs after the mount; a frame later it has run.
    await raf();
    expect(host.querySelector<HTMLInputElement>('[data-testid="ticket-field-input"]')!.value).toBe('OPS');
    expect(document.activeElement).toBe(other);
    expect(scroller.scrollTop).toBe(top);
    expect(held.text).toBe('OPS');
  });
});
