import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import SettingsRail from './SettingsRail.svelte';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { REMOTE_BACKEND_UUID, stageBackend } from '../../../test/helpers/backends';
import { grantBackendScopes, pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';
import { SCOPES } from '../../transport/scopes';
import { HOME_BACKEND } from '../../transport/backendKey';
import { listenProviderUpdates } from '../../../test/helpers/providerUpdates';

function update(provider: string, overrides: Record<string, unknown> = {}) {
  return { provider, installedVersion: '1.0.0', latestVersion: '1.1.0', available: true, checkedAt: 1, run: { phase: 'idle' }, ...overrides };
}

const idle = (provider: string) => update(provider, { latestVersion: '1.0.0', available: false });

function dots(): string[] {
  return [...document.querySelectorAll<HTMLElement>('[data-testid="settings-rail-provider-update-dot"]')]
    .map((dot) => `${dot.closest('[role="tab"]')?.id}:${dot.dataset.provider}`);
}

describe('<SettingsRail> provider update dots', () => {
  it('marks each provider with an update on the selected computer only', async () => {
    stageBackend();
    await grantBackendScopes('laptop', SCOPES);
    listenProviderUpdates();
    emitWailsEvent('provider:updates', [update('claude'), idle('codex')]);
    emitWailsEvent('provider:updates', [idle('claude'), update('codex')], REMOTE_BACKEND_UUID);

    const props = { activeSection: 'claude' as const, onSelectSection() {}, onSelectHit() {} };
    const view = render(SettingsRail, { props: { ...props, computer: HOME_BACKEND } });
    expect(dots()).toEqual(['settings-tab-claude:claude']);
    expect(document.querySelector('[data-testid="settings-rail-provider-update-dot"]')?.getAttribute('aria-label')).toBe('Update available');

    await view.rerender({ ...props, computer: 'laptop' });
    expect(dots()).toEqual(['settings-tab-codex:codex']);

    // A running update is no longer waiting; the dot goes.
    emitWailsEvent('provider:updates', [idle('claude'), update('codex', { run: { phase: 'running' } })], REMOTE_BACKEND_UUID);
    await Promise.resolve();
    expect(dots()).toEqual([]);
  });

  it('shows no dot without access:admin', async () => {
    listenProviderUpdates();
    emitWailsEvent('provider:updates', [update('claude'), update('codex')]);
    render(SettingsRail, { props: { activeSection: 'theme', computer: HOME_BACKEND, onSelectSection() {}, onSelectHit() {} } });
    expect(dots()).toHaveLength(2);
    await pairViewOnly();
    try {
      await Promise.resolve();
      expect(dots()).toEqual([]);
    } finally {
      resetToLocalPage();
    }
  });
});
