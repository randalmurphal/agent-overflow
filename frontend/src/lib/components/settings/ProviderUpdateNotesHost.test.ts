import { describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ProviderUpdateNotesHost from './ProviderUpdateNotesHost.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { REMOTE_BACKEND_UUID, stageBackend } from '../../../test/helpers/backends';
import { grantBackendScopes } from '../../../test/helpers/scopes';
import { SCOPES } from '../../transport/scopes';
import { takePinnedBackend } from '../../transport/backends';
import { HOME_BACKEND } from '../../transport/backendKey';
import { listenProviderUpdates } from '../../../test/helpers/providerUpdates';
import {
  openProviderUpdateNotes,
  providerUpdateNotes,
  type ProviderUpdateNotes,
} from '../../stores/providerUpdates.svelte';

function notes(overrides: Partial<ProviderUpdateNotes> = {}): ProviderUpdateNotes {
  return {
    provider: 'claude',
    fromVersion: '2.1.321',
    toVersion: '2.1.325',
    releases: [
      { version: '2.1.325', name: 'v2.1.325', publishedAt: '2026-11-13T10:00:00Z', body: '## Newest\n\n- fixed **the thing**', url: '' },
      { version: '2.1.324', name: 'v2.1.324', publishedAt: '', body: 'older body', url: '' },
      { version: '2.1.322', name: '', publishedAt: '', body: '', url: '' },
    ],
    omitted: 0,
    url: 'https://github.com/anthropics/claude-code/releases',
    ...overrides,
  } as ProviderUpdateNotes;
}

const sections = () => [...document.querySelectorAll<HTMLElement>('[data-testid="provider-update-notes-version"]')];

describe('<ProviderUpdateNotesHost>', () => {
  it('renders nothing until notes are opened', () => {
    const { queryByTestId } = render(ProviderUpdateNotesHost);
    expect(queryByTestId('provider-update-notes-modal')).toBeNull();
  });

  it('lists one section per version, newest first and expanded, rendering only expanded bodies', async () => {
    setBindingMock('GetProviderUpdateNotes', async () => notes());
    render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', HOME_BACKEND);
    await waitFor(() => expect(sections()).toHaveLength(3));
    expect(document.body.textContent).toContain("What's new in Claude");
    expect(document.querySelector('[data-testid="provider-update-notes-modal"]')?.textContent).toContain('2.1.321 to 2.1.325');
    expect(sections().map((s) => s.dataset.version)).toEqual(['2.1.325', '2.1.324', '2.1.322']);
    const bodies = () => document.querySelectorAll('[data-testid="provider-update-notes-body"]');
    expect(bodies()).toHaveLength(1);
    expect(sections()[0].querySelector('button')?.getAttribute('aria-expanded')).toBe('true');
    expect(sections()[0].querySelector('strong')?.textContent).toBe('the thing');
    expect(sections()[1].textContent).not.toContain('older body');

    await fireEvent.click(sections()[1].querySelector('button')!);
    expect(bodies()).toHaveLength(2);
    expect(sections()[1].textContent).toContain('older body');
    await fireEvent.click(sections()[0].querySelector('button')!);
    expect(bodies()).toHaveLength(1);

    await fireEvent.click(sections()[2].querySelector('button')!);
    expect(sections()[2].textContent).toContain('2.1.322');
    expect(sections()[2].textContent).toContain('No notes were published for this version.');
  });

  // Chat-mode Markdown (no embeddedHtml): raw HTML never becomes an element.
  it('creates no element from raw HTML in a release body', async () => {
    setBindingMock('GetProviderUpdateNotes', async () => notes({
      releases: [{ version: '2.1.325', name: '', publishedAt: '', body: 'before <img src="x" onerror="alert(1)"> <b>bold</b>\n\n<div class="x">block</div>\n\n<details><summary>more</summary>\n\nhidden\n\n</details>', url: '' }] as ProviderUpdateNotes['releases'],
    }));
    render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', HOME_BACKEND);
    const body = await waitFor(() => {
      const el = document.querySelector('[data-testid="provider-update-notes-body"]');
      expect(el).not.toBeNull();
      return el!;
    });
    await waitFor(() => expect(body.textContent).toContain('before'));
    expect(body.textContent).toContain('bold');
    expect(body.querySelector('img, b, strong, div.x, details, summary, [onerror]')).toBeNull();
  });

  it('says how many older versions were left out and links the releases page', async () => {
    setBindingMock('GetProviderUpdateNotes', async () => notes({ omitted: 3 }));
    render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', HOME_BACKEND);
    const omitted = await waitFor(() => {
      const el = document.querySelector<HTMLElement>('[data-testid="provider-update-notes-omitted"]');
      expect(el).not.toBeNull();
      return el!;
    });
    expect(omitted.textContent).toContain('3 older versions not shown.');
    expect(omitted.querySelector('a')?.getAttribute('href')).toBe('https://github.com/anthropics/claude-code/releases');
    expect(document.querySelector('[data-testid="provider-update-notes-releases-link"]')?.getAttribute('href'))
      .toBe('https://github.com/anthropics/claude-code/releases');
  });

  it('shows loading, an empty range, and an error with retry', async () => {
    let answer: () => Promise<ProviderUpdateNotes> = () => new Promise(() => {});
    setBindingMock('GetProviderUpdateNotes', () => answer());
    render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('codex', HOME_BACKEND);
    await waitFor(() => expect(document.body.textContent).toContain('Loading release notes…'));

    answer = async () => { throw new Error('github rate limited'); };
    openProviderUpdateNotes('claude', HOME_BACKEND);
    const error = await waitFor(() => {
      const el = document.querySelector<HTMLElement>('[data-testid="provider-update-notes-error"]');
      expect(el).not.toBeNull();
      return el!;
    });
    expect(error.textContent).toContain('Github rate limited.');

    answer = async () => notes({ releases: [] });
    await fireEvent.click([...error.querySelectorAll('button')].find((b) => b.textContent?.includes('Retry'))!);
    await waitFor(() => expect(document.querySelector('[data-testid="provider-update-notes-empty"]')).not.toBeNull());
  });

  it("offers the update of the computer that reported it, and closes", async () => {
    stageBackend();
    await grantBackendScopes('laptop', SCOPES);
    listenProviderUpdates();
    emitWailsEvent('provider:updates', [{
      provider: 'claude', installedVersion: '2.1.321', latestVersion: '2.1.325', available: true, checkedAt: 1, run: { phase: 'idle' },
    }], REMOTE_BACKEND_UUID);
    setBindingMock('GetProviderUpdateNotes', async () => notes());
    // The backend publishes the running frame before it answers the call.
    const run = setBindingMock('RunProviderUpdate', async () => {
      expect(takePinnedBackend()).toBe('laptop');
      emitWailsEvent('provider:updates', [{
        provider: 'claude', installedVersion: '2.1.321', latestVersion: '2.1.325', available: true, checkedAt: 1, run: { phase: 'running' },
      }], REMOTE_BACKEND_UUID);
    });
    const { findByTestId, getByRole } = render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', 'laptop');
    const update = await findByTestId('provider-update-notes-update-button');
    expect(update.textContent?.trim()).toBe('Update to 2.1.325');
    // Two computers are attached, so the modal says which one this is.
    expect(document.querySelector('[data-testid="provider-update-notes-range"]')?.textContent?.replace(/\s+/g, ' ').trim())
      .toBe('2.1.321 to 2.1.325 on Laptop');
    await fireEvent.click(update);
    expect(run).toHaveBeenCalledWith('claude');
    await waitFor(() => expect((update as HTMLButtonElement).disabled).toBe(true));
    expect(update.textContent?.trim()).toBe('Updating…');

    // A check that fails mid-run clears the offered version, not the run.
    emitWailsEvent('provider:updates', [{
      provider: 'claude', installedVersion: '2.1.321', latestVersion: '', available: false, checkedAt: 2, checkError: 'release host unreachable', run: { phase: 'running' },
    }], REMOTE_BACKEND_UUID);
    await Promise.resolve();
    const busy = await findByTestId('provider-update-notes-update-button');
    expect((busy as HTMLButtonElement).disabled).toBe(true);
    expect(busy.textContent?.trim()).toBe('Updating…');

    await fireEvent.click(getByRole('button', { name: 'Close' }));
    expect(providerUpdateNotes()).toBeNull();
  });

  it('keeps a refused start and a failed run, with its output, beside the update button', async () => {
    listenProviderUpdates();
    const offered = { provider: 'claude', installedVersion: '2.1.321', latestVersion: '2.1.325', available: true, checkedAt: 1 };
    emitWailsEvent('provider:updates', [{ ...offered, run: { phase: 'idle' } }]);
    setBindingMock('GetProviderUpdateNotes', async () => notes());
    setBindingMock('RunProviderUpdate', async () => { throw new Error('an update of this provider is already running'); });
    const { findByTestId, queryByTestId } = render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', HOME_BACKEND);
    expect(queryByTestId('provider-update-notes-update-error')).toBeNull();
    await fireEvent.click(await findByTestId('provider-update-notes-update-button'));
    const refused = await findByTestId('provider-update-notes-update-error');
    expect(refused.textContent).toContain('An update of this provider is already running.');

    setBindingMock('RunProviderUpdate', async () => {
      emitWailsEvent('provider:updates', [{ ...offered, run: { phase: 'running' } }]);
    });
    await fireEvent.click(await findByTestId('provider-update-notes-update-button'));
    await waitFor(() => expect(queryByTestId('provider-update-notes-update-error')).toBeNull());
    emitWailsEvent('provider:updates', [{
      ...offered, run: { phase: 'failed', error: 'the update command exited with status 1', output: 'npm ERR! EACCES' },
    }]);
    const failed = await findByTestId('provider-update-notes-update-error');
    expect(failed.textContent).toContain('the update command exited with status 1');
    expect(failed.querySelector('pre')?.textContent).toBe('npm ERR! EACCES');
    expect((await findByTestId('provider-update-notes-update-button')).textContent?.trim()).toBe('Update to 2.1.325');
  });

  it('offers no update for a computer with nothing newer', async () => {
    setBindingMock('GetProviderUpdateNotes', async () => notes());
    const { queryByTestId } = render(ProviderUpdateNotesHost);
    openProviderUpdateNotes('claude', HOME_BACKEND);
    await waitFor(() => expect(sections()).toHaveLength(3));
    expect(queryByTestId('provider-update-notes-update-button')).toBeNull();
    // One computer: nothing to tell apart.
    expect(document.querySelector('[data-testid="provider-update-notes-range"]')?.textContent?.replace(/\s+/g, ' ').trim())
      .toBe('2.1.321 to 2.1.325');
  });
});
