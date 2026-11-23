import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ProviderAccountResets from './ProviderAccountResets.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { account } from '../../../test/helpers/providerAccounts';
import { resetForTest as resetProviderAccounts } from '../../stores/providerAccounts.svelte';
import type { ManagedProviderAccount, ProviderAccountResetOption } from '../../stores/bindings';
import { HOME_BACKEND } from '../../transport/backendKey';
import { formatCalendarDate } from '../../utils/format';
import { answerAsV0019 } from '../../../test/helpers/olderComputer';

function option(overrides: Partial<ProviderAccountResetOption>): ProviderAccountResetOption {
  return { id: '', kind: 'full', label: 'Full reset', refills: 'Refills your current Codex usage limits.', expiresAt: 0, usable: true, ...overrides } as ProviderAccountResetOption;
}

function withResets(
  options: ProviderAccountResetOption[],
  extra: Partial<ManagedProviderAccount> = {},
  revision = 4,
  unlisted = 0,
): ManagedProviderAccount {
  return account({
    id: 'codex-a', provider: 'codex', email: 'a@example.com',
    usageResets: { revision, options, ...(unlisted ? { unlisted } : {}) },
    ...extra,
  } as Partial<ManagedProviderAccount>);
}

const UNSETTLED = { id: 'spend-1', label: 'Full reset', refills: 'Refills your current Codex usage limits.', startedAt: 1_800_000_000_000 };

function renderResets(card: ManagedProviderAccount, props: { admin?: boolean; credentialOpInFlight?: boolean } = {}) {
  return render(ProviderAccountResets, {
    props: {
      account: card,
      backend: HOME_BACKEND,
      admin: props.admin ?? true,
      credentialOpInFlight: props.credentialOpInFlight ?? false,
    },
  });
}

function rowText(el: Element): string {
  return el.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

beforeEach(() => {
  resetBindingMocks();
  resetProviderAccounts();
  setBindingMock('ListProviderAccounts', async () => []);
});
afterEach(() => resetProviderAccounts());

describe('<ProviderAccountResets>', () => {
  it('shows no button for an account with nothing to show, and keeps its box', () => {
    // A computer on an older release sends neither usageResets nor
    // pendingReset, so this is also what its cards show.
    const bare = renderResets(account({ id: 'codex-a', provider: 'codex' }));
    expect(bare.queryByTestId('provider-account-resets-codex-a')).toBeNull();
    expect(bare.queryByTestId('provider-account-resets-box-codex-a')).not.toBeNull();
    bare.unmount();
    const empty = renderResets(withResets([]));
    expect(empty.queryByTestId('provider-account-resets-codex-a')).toBeNull();
  });

  // v0.0.19 has neither reset method and its listing carries neither field:
  // its card offers nothing, so neither method is ever called there.
  it('offers nothing on a computer on an older release', async () => {
    answerAsV0019();
    const card = account({ id: 'codex-a', provider: 'codex', email: 'a@example.com' });
    expect('usageResets' in card || 'pendingReset' in card).toBe(false);
    const { queryByTestId, queryByRole } = renderResets(card);
    expect(queryByTestId('provider-account-resets-codex-a')).toBeNull();
    expect(queryByRole('button', { name: /Use a|Finish the/ })).toBeNull();
    expect(getBindingMock('SpendProviderAccountReset')).not.toHaveBeenCalled();
    expect(getBindingMock('FinishProviderAccountReset')).not.toHaveBeenCalled();
  });

  it('counts only the resets usable now, unlisted ones included', () => {
    const { getByTestId } = renderResets(withResets([
      option({ id: 'a' }),
      option({ id: 'b' }),
      option({ id: 'c', usable: false, reason: 'Codex is still applying this reset.' }),
    ], {}, 4, 3));
    expect(rowText(getByTestId('provider-account-resets-codex-a'))).toBe('Usage resets (5)');
  });

  it('says 99+ past 99, which is the label its box is sized for', () => {
    const { getByTestId } = renderResets(withResets([option({ id: 'a' })], {}, 4, 150));
    expect(rowText(getByTestId('provider-account-resets-codex-a'))).toBe('Usage resets (99+)');
    const sizer = getByTestId('provider-account-resets-box-codex-a').querySelector('[aria-hidden="true"]');
    expect(rowText(sizer!)).toBe('Usage resets (99+)');
  });

  it('opens a menu with one line per kind, counted and dated by the first to expire', async () => {
    const soon = 1_800_000_000;
    const { getByTestId, getAllByTestId, queryByTestId } = renderResets(withResets([
      option({ id: 'a', expiresAt: soon }),
      option({ id: 'b', expiresAt: soon + 86_400 }),
      option({ id: 'c', kind: 'weekly', label: 'Full reset (Weekly + 5 hr)' }),
      // Same kind as a and b; the title does not decide the line.
      option({ id: 'd', label: 'Full reset, renamed' }),
    ]));
    expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull();
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    expect(getAllByTestId('provider-account-reset-row').map(rowText)).toEqual([
      `Full reset 3 left · first expires ${formatCalendarDate(soon * 1000)} Use`,
      'Full reset (Weekly + 5 hr) 1 left Use',
    ]);
  });

  it('shows resets Codex counted but did not list as one plain line with no Use', async () => {
    const { getByTestId, getAllByTestId } = renderResets(withResets([option({ id: 'a' })], {}, 4, 2));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    const unlisted = getByTestId('provider-account-reset-unlisted');
    expect(rowText(unlisted)).toBe("2 more Codex counted these but didn't list them, so they can't be picked here.");
    expect(unlisted.querySelector('[role="menuitem"]')?.getAttribute('aria-disabled')).toBe('true');
    expect(unlisted.querySelector('button[aria-label^="Use"]')).toBeNull();
    expect(getAllByTestId('provider-account-reset-use')).toHaveLength(1);
  });

  // A count with no list: Codex said how many, and named none to spend.
  it('shows a count with no list as one plain line with its count and no Use', async () => {
    const { getByTestId, queryByTestId } = renderResets(withResets([], {}, 4, 2));
    expect(rowText(getByTestId('provider-account-resets-codex-a'))).toBe('Usage resets (2)');
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    expect(rowText(getByTestId('provider-account-reset-unlisted')))
      .toBe("2 available Codex counted these but didn't list them, so they can't be picked here.");
    expect(queryByTestId('provider-account-reset-use')).toBeNull();
  });

  // The record is made just before the write, so an unconfirmed reset may
  // not have gone out at all; its line is true either way.
  it('words an unconfirmed reset so it holds whether or not it was sent', async () => {
    const { getByTestId } = renderResets(withResets([], { pendingReset: UNSETTLED } as Partial<ManagedProviderAccount>));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    const line = rowText(getByTestId('provider-account-reset-unsettled'));
    expect(line).toContain('Started at ');
    expect(line).toContain('Finish completes it without using a second reset.');
    expect(line).not.toContain('Sent');
  });

  it("shows a reset that can't be used now as a line with Codex's reason and no Use", async () => {
    const { getByTestId, getAllByTestId, queryByRole } = renderResets(withResets([
      option({ id: 'a', usable: false, reason: 'Codex is still applying this reset.' }),
    ]));
    const trigger = getByTestId('provider-account-resets-codex-a');
    expect(rowText(trigger)).toBe('Usage resets (0)');
    await fireEvent.click(trigger);
    const [line] = getAllByTestId('provider-account-reset-row');
    expect(line.dataset.usable).toBe('false');
    expect(rowText(line)).toBe('Full reset Codex is still applying this reset.');
    expect(queryByRole('button', { name: /Use a Full reset/ })).toBeNull();
  });

  it('puts an unconfirmed reset first with Finish for that spend, marks the button and offers no Use', async () => {
    setBindingMock('FinishProviderAccountReset', async () => ({ message: 'This reset was already used.', spent: true, readingLanded: true }));
    const { getByTestId, getAllByTestId, queryByRole, getByRole } = renderResets(
      withResets([option({ id: 'b' })], { pendingReset: UNSETTLED } as Partial<ManagedProviderAccount>),
    );
    const trigger = getByTestId('provider-account-resets-codex-a');
    expect(trigger.dataset.unsettled).toBe('true');
    expect(trigger.className).toContain('text-warning');
    await fireEvent.click(trigger);
    const list = getByTestId('provider-account-resets-list-codex-a');
    expect(list.querySelector('[data-testid]')?.getAttribute('data-testid')).toBe('provider-account-reset-unsettled');
    expect(getAllByTestId('provider-account-reset-row')).toHaveLength(1);
    expect(queryByRole('button', { name: /Use a Full reset/ })).toBeNull();

    await fireEvent.click(getByRole('button', { name: 'Finish the Full reset on a@example.com' }));
    await waitFor(() => expect(getBindingMock('FinishProviderAccountReset')).toHaveBeenCalledWith('codex', 'codex-a', 'spend-1'));
    // Finish has no confirm: it can only settle what was confirmed.
    expect(queryByRole('dialog', { name: /Use a/ })).toBeNull();
  });

  it('shows the button for an unconfirmed reset alone', () => {
    const { getByTestId } = renderResets(withResets([], { pendingReset: UNSETTLED } as Partial<ManagedProviderAccount>));
    expect(rowText(getByTestId('provider-account-resets-codex-a'))).toBe('Usage resets (0)');
  });

  it('opens the confirm from Use, closes the menu, and spends only after the confirm', async () => {
    setBindingMock('SpendProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
    const { getByTestId, getByRole, findByRole, queryByTestId } = renderResets(withResets([
      option({ id: 'a', expiresAt: 1_800_000_000 }),
      option({ id: 'b', expiresAt: 1_900_000_000 }),
    ]));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    await fireEvent.click(getByRole('button', { name: 'Use a Full reset on a@example.com' }));
    const dialog = await findByRole('dialog', { name: 'Use a Full reset?' });
    expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull();
    expect(dialog.textContent).toContain('Use one Full reset on a@example.com? Refills your current Codex usage limits.');
    expect(getBindingMock('SpendProviderAccountReset')).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: 'Use reset' }));
    await waitFor(() => expect(getBindingMock('SpendProviderAccountReset')).toHaveBeenCalledWith('codex', 'codex-a', 'a', 4));
  });

  it('opens the confirm when the Use line itself is chosen', async () => {
    const { getByTestId, findByRole } = renderResets(withResets([option({ id: 'a' })]));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    await fireEvent.click(getByTestId('provider-account-reset-use'));
    await findByRole('dialog', { name: 'Use a Full reset?' });
  });

  it('spends nothing when the confirm is cancelled', async () => {
    setBindingMock('SpendProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
    const { getByTestId, getByRole, findByRole, queryByRole } = renderResets(withResets([option({ id: 'a' })]));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    await fireEvent.click(getByRole('button', { name: /Use a Full reset/ }));
    await findByRole('dialog', { name: 'Use a Full reset?' });
    await fireEvent.click(getByRole('button', { name: 'Cancel' }));
    await waitFor(() => expect(queryByRole('dialog', { name: 'Use a Full reset?' })).toBeNull());
    expect(getBindingMock('SpendProviderAccountReset')).not.toHaveBeenCalled();
  });

  it('closes the menu on an outside click and on Escape', async () => {
    const { getByTestId, queryByTestId } = renderResets(withResets([option({ id: 'a' })]));
    const trigger = getByTestId('provider-account-resets-codex-a');
    await fireEvent.click(trigger);
    expect(queryByTestId('provider-account-resets-list-codex-a')).not.toBeNull();
    await fireEvent.mouseDown(document.body);
    await waitFor(() => expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull());

    await fireEvent.click(trigger);
    expect(queryByTestId('provider-account-resets-list-codex-a')).not.toBeNull();
    await fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull());
  });

  it('follows the list while it is open, and closes with the button when nothing is left', async () => {
    const { getByTestId, getAllByTestId, queryByTestId, rerender } = renderResets(withResets([option({ id: 'a' }), option({ id: 'b' })]));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    expect(getAllByTestId('provider-account-reset-row').map(rowText)).toEqual(['Full reset 2 left Use']);

    await rerender({ account: withResets([option({ id: 'b' })], {}, 5) });
    expect(getAllByTestId('provider-account-reset-row').map(rowText)).toEqual(['Full reset 1 left Use']);
    expect(rowText(getByTestId('provider-account-resets-codex-a'))).toBe('Usage resets (1)');

    await rerender({ account: withResets([], {}, 6) });
    expect(queryByTestId('provider-account-resets-codex-a')).toBeNull();
    expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull();
    // A reset earned later brings the button back closed.
    await rerender({ account: withResets([option({ id: 'c' })], {}, 7) });
    expect(queryByTestId('provider-account-resets-codex-a')).not.toBeNull();
    expect(queryByTestId('provider-account-resets-list-codex-a')).toBeNull();
  });

  it('spends what the confirm showed when the list changes while it is open', async () => {
    setBindingMock('SpendProviderAccountReset', async () => ({ message: 'Reset used.', spent: true, readingLanded: true }));
    const { getByTestId, getByRole, findByRole, rerender } = renderResets(withResets([
      option({ id: 'a' }),
      option({ id: 'b', kind: 'weekly', label: 'Full reset (Weekly + 5 hr)' }),
    ]));
    await fireEvent.click(getByTestId('provider-account-resets-codex-a'));
    await fireEvent.click(getByRole('button', { name: 'Use a Full reset on a@example.com' }));
    await findByRole('dialog', { name: 'Use a Full reset?' });

    // Another client spent a; the new list has only b, at a new revision.
    await rerender({ account: withResets([option({ id: 'b', kind: 'weekly', label: 'Full reset (Weekly + 5 hr)' })], {}, 5) });
    expect(getByRole('dialog', { name: 'Use a Full reset?' }).textContent).toContain('Use one Full reset on a@example.com?');
    await fireEvent.click(getByRole('button', { name: 'Use reset' }));
    // The backend refuses revision 4, so nothing the person did not see is spent.
    await waitFor(() => expect(getBindingMock('SpendProviderAccountReset')).toHaveBeenCalledWith('codex', 'codex-a', 'a', 4));
  });

  it('keeps the menu openable and Use and Finish inert, with the reason, while the card is', async () => {
    setBindingMock('FinishProviderAccountReset', async () => ({ message: 'x', spent: true, readingLanded: true }));
    const cases: Array<[string, Parameters<typeof renderResets>[1], Partial<ManagedProviderAccount>, RegExp]> = [
      ['no grant', { admin: false }, {}, /cannot manage accounts/],
      ['credential operation', { credentialOpInFlight: true }, {}, /account action that is running/],
      ['needs login', {}, { needsLogin: true }, /Sign in again/],
    ];
    for (const [name, props, extra, reason] of cases) {
      for (const unsettled of [false, true]) {
        const card = withResets([option({ id: 'a' })], { ...extra, ...(unsettled ? { pendingReset: UNSETTLED } : {}) } as Partial<ManagedProviderAccount>);
        const view = renderResets(card, props);
        await fireEvent.click(view.getByTestId('provider-account-resets-codex-a'));
        const button = view.getByRole('button', { name: unsettled ? /Finish the Full reset/ : /Use a Full reset/ }) as HTMLButtonElement;
        expect(button.disabled, `${name}, unsettled=${unsettled}`).toBe(true);
        expect(button.title, `${name}, unsettled=${unsettled}`).toMatch(reason);
        // Choosing the line itself does nothing either.
        await fireEvent.click(view.getByTestId(unsettled ? 'provider-account-reset-finish' : 'provider-account-reset-use'));
        expect(view.queryByRole('dialog'), `${name}, unsettled=${unsettled}`).toBeNull();
        expect(getBindingMock('FinishProviderAccountReset')).not.toHaveBeenCalled();
        view.unmount();
      }
    }
  });
});
