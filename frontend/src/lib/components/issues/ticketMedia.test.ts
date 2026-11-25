// A Jira image or attachment in ticket text (`media:ID`, written by
// internal/tracker/adf/render_media.go) shows as a plain chip naming the
// file: no image is fetched, no anchor is drawn, and nothing reaches the
// link opener, in a description and in a comment, written as Markdown or as
// embedded HTML.

import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CommentView, DetailView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import TicketComments from './TicketComments.svelte';
import TicketText from './TicketText.svelte';
import { externalURLForEventTarget, installExternalLinkDelegate } from '../../utils/externalLinks';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../../transport/backendKey';

const TEXT = [
  'Before ![diagram.png](media:abc) mid ![](media:def) and [report.pdf](media:ghi) after.',
  '',
  'Embedded <img src="media:jkl" alt="shot.png"> and <a href="media:mno">spec.pdf</a> inline.',
  '',
  // A wrapper is one HTML block, which the sanitizer renders.
  '<div><img src="media:pqr" alt="wrapped.png"> <a href="media:stu">wrapped.pdf</a></div>',
  '',
  'The end.',
].join('\n');

const CHIPS = ['diagram.png', 'image', 'report.pdf', 'shot.png', 'spec.pdf', 'wrapped.png'];

let uninstall: (() => void) | null = null;
afterEach(() => {
  uninstall?.();
  uninstall = null;
  vi.restoreAllMocks();
});

/** Every check that the text draws chips and leaves nothing to open. */
async function expectChips(root: HTMLElement): Promise<void> {
  await waitFor(() => expect(root.textContent).toContain('The end.'));
  const chips = await waitFor(() => {
    const found = Array.from(root.querySelectorAll<HTMLElement>('[data-tracker-media]'));
    expect(found.map((c) => c.textContent?.trim())).toEqual(CHIPS);
    return found;
  });
  expect(root.querySelector('img')).toBeNull();
  // An `<a>` inside an HTML block keeps its text and loses its href.
  expect(root.querySelector('a[href]')).toBeNull();
  expect(root.textContent).toContain('wrapped.pdf');
  expect(root.querySelector('[data-streamdown-image-blocked]')).toBeNull();
  // Text, not a control.
  for (const chip of chips) {
    expect(chip.tagName).toBe('SPAN');
    expect(chip.hasAttribute('tabindex')).toBe(false);
    expect(chip.getAttribute('role')).toBeNull();
    expect(chip.querySelector('a, button, [tabindex]')).toBeNull();
  }
  // Nothing a click hands to the opener.
  const open = vi.spyOn(window, 'open').mockReturnValue(null);
  const opener = setBindingMock('OpenExternalURL', async () => {});
  const wrapped = Array.from(root.querySelectorAll<HTMLElement>('a')).find((a) => a.textContent === 'wrapped.pdf');
  expect(wrapped).toBeDefined();
  for (const target of [...chips, wrapped!]) {
    expect(externalURLForEventTarget(target)).toBeNull();
    await fireEvent.click(target);
  }
  expect(open).not.toHaveBeenCalled();
  expect(opener).not.toHaveBeenCalled();
}

describe('Jira media in ticket text', () => {
  it('shows each image and attachment of a description as a chip naming it', async () => {
    uninstall = installExternalLinkDelegate();
    const { container } = render(TicketText, {
      props: {
        field: 'description', title: 'Description', markdown: TEXT, editable: false, open: null,
        onSave: async () => null, ticketKey: 'ticket', target: 'field:description', fieldName: (id: string) => id,
      },
    });
    await expectChips(container.querySelector<HTMLElement>('[data-testid="ticket-section-text"]')!);
  });

  it('shows each image and attachment of a comment as a chip naming it', async () => {
    uninstall = installExternalLinkDelegate();
    const comment = { id: '7', author: { id: 'u1', name: 'User 1' }, body: TEXT, created: 1_000 } as CommentView;
    const { container } = render(TicketComments, {
      props: {
        ctx: { key: 'ticket', ref: { backend: HOME_BACKEND, connection: 'c1', issue: '1' }, label: 'WEB-1' },
        detail: { comments: [comment] } as unknown as DetailView,
        earlier: { pages: [], next: null, loading: false, error: null },
        order: 'oldest',
        onOrder: () => {},
        fieldName: (id: string) => id,
      },
    });
    await expectChips(container.querySelector<HTMLElement>('[data-testid="ticket-comment-body"]')!);
  });
});
