import { describe, expect, it, beforeEach } from 'vitest';
import { render, fireEvent, waitFor, within } from '@testing-library/svelte';
import ProviderCustomEnvSection from './ProviderCustomEnvSection.svelte';
import { loadSettings } from '../../stores/settings.svelte';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';
import { getProviderDefinition } from '../../providers/catalog';
import type { ProviderEnvVar, Settings } from '../../types/settings';
import { makeSettings } from '../../../test/helpers/settings';

const CLAUDE = getProviderDefinition('claude');
const CODEX = getProviderDefinition('codex');

// The backend redacts sensitive values on every read path, so the seed
// mirrors what the UI actually receives: name + flag, no value.
async function seed(overrides: Partial<Settings> = {}): Promise<Settings> {
  const merged = makeSettings(overrides);
  setBindingMock('GetSettings', async () => merged);
  setBindingMock('SetProviderCustomEnvVar', async () => merged);
  setBindingMock('DeleteProviderCustomEnvVar', async () => merged);
  await loadSettings();
  return merged;
}

const OPEN_VAR: ProviderEnvVar = {
  name: 'ANTHROPIC_BASE_URL',
  value: 'https://gw.example.test',
};
const SECRET_VAR: ProviderEnvVar = {
  name: 'PROXY_TOKEN',
  value: '',
  sensitive: true,
};

describe('<ProviderCustomEnvSection>', () => {
  beforeEach(async () => {
    await seed();
  });

  it('renders the empty state when the provider has no variables', async () => {
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    expect(getByTestId('settings-provider-env-empty')).toBeTruthy();
  });

  it('renders each configured variable for its own provider only', async () => {
    await seed({
      claudeCustomEnv: [OPEN_VAR],
      codexCustomEnv: [{ name: 'HTTPS_PROXY', value: 'http://proxy.test:8080' }],
    });
    // Scoped to each component's own container: both mount into one document.
    const claude = within(
      render(ProviderCustomEnvSection, { props: { provider: CLAUDE } }).container,
    );
    expect(claude.getByTestId('settings-provider-env-row-ANTHROPIC_BASE_URL')).toBeTruthy();
    expect(claude.queryByTestId('settings-provider-env-row-HTTPS_PROXY')).toBeNull();

    const codex = within(
      render(ProviderCustomEnvSection, { props: { provider: CODEX } }).container,
    );
    expect(codex.getByTestId('settings-provider-env-row-HTTPS_PROXY')).toBeTruthy();
    expect(codex.queryByTestId('settings-provider-env-row-ANTHROPIC_BASE_URL')).toBeNull();
  });

  // A sensitive value never arrives in the browser, so the row must not
  // pretend to show one — it renders as an empty masked field the user
  // overwrites by typing a replacement.
  it('masks a sensitive value and shows a plain one', async () => {
    await seed({ claudeCustomEnv: [OPEN_VAR, SECRET_VAR] });
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });

    const open = getByTestId('settings-provider-env-value-ANTHROPIC_BASE_URL') as HTMLInputElement;
    expect(open.type).toBe('text');
    expect(open.value).toBe('https://gw.example.test');

    const secret = getByTestId('settings-provider-env-value-PROXY_TOKEN') as HTMLInputElement;
    expect(secret.type).toBe('password');
    expect(secret.value).toBe('');
    expect(secret.placeholder).toBe('••••••••');
    // No affordance offers to reveal or re-mask a value the client cannot see.
    expect(secret.textContent).not.toContain('hunter2');
    expect(
      (getByTestId('settings-provider-env-row-PROXY_TOKEN') as HTMLElement).querySelector(
        '[data-testid="settings-provider-env-mask-PROXY_TOKEN"]',
      ),
    ).toBeNull();
  });

  it('adds a variable through the dedicated binding and clears the draft', async () => {
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    const name = getByTestId('settings-provider-env-name-input') as HTMLInputElement;
    const value = getByTestId('settings-provider-env-value-input') as HTMLInputElement;
    const sensitive = getByTestId('settings-provider-env-sensitive-input') as HTMLInputElement;

    await fireEvent.input(name, { target: { value: 'PROXY_TOKEN' } });
    await fireEvent.input(value, { target: { value: 'hunter2' } });
    await fireEvent.click(sensitive);
    await fireEvent.click(getByTestId('settings-provider-env-add'));

    await waitFor(() => {
      expect(getBindingMock('SetProviderCustomEnvVar')?.mock.calls[0]).toEqual([
        'claude',
        'PROXY_TOKEN',
        'hunter2',
        true,
      ]);
    });
    await waitFor(() => expect(name.value).toBe(''));
    expect(value.value).toBe('');
    expect(sensitive.checked).toBe(false);
  });

  it('refuses a malformed or reserved name before calling the backend', async () => {
    const { getByTestId, queryByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    const name = getByTestId('settings-provider-env-name-input') as HTMLInputElement;

    for (const [input, fragment] of [
      ['MY-VAR', 'letters, digits, and underscore'],
      ['FOO=BAR', 'cannot contain'],
      ['AO_TOKEN', 'reserved'],
    ] as const) {
      await fireEvent.input(name, { target: { value: input } });
      await waitFor(() => {
        expect(getByTestId('settings-provider-env-error').textContent).toContain(fragment);
      });
      expect((getByTestId('settings-provider-env-add') as HTMLButtonElement).disabled).toBe(true);
    }

    await fireEvent.input(name, { target: { value: 'GOOD_NAME' } });
    await waitFor(() => expect(queryByTestId('settings-provider-env-error')).toBeNull());
    expect((getByTestId('settings-provider-env-add') as HTMLButtonElement).disabled).toBe(false);
    expect(getBindingMock('SetProviderCustomEnvVar')?.mock.calls.length ?? 0).toBe(0);
  });

  it('refuses a duplicate name against the configured list', async () => {
    await seed({ claudeCustomEnv: [OPEN_VAR] });
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    await fireEvent.input(getByTestId('settings-provider-env-name-input'), {
      target: { value: 'anthropic_base_url' },
    });
    await waitFor(() => {
      expect(getByTestId('settings-provider-env-error').textContent).toContain('already set');
    });
  });

  it('overwrites a sensitive value by re-entry and ignores an untouched field', async () => {
    await seed({ claudeCustomEnv: [SECRET_VAR] });
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    const field = getByTestId('settings-provider-env-value-PROXY_TOKEN') as HTMLInputElement;

    // Blurring an untouched masked field must not overwrite the stored secret
    // with the empty string the input necessarily renders.
    await fireEvent.change(field, { target: { value: '' } });
    expect(getBindingMock('SetProviderCustomEnvVar')?.mock.calls.length ?? 0).toBe(0);

    await fireEvent.change(field, { target: { value: 'new-secret' } });
    await waitFor(() => {
      expect(getBindingMock('SetProviderCustomEnvVar')?.mock.calls[0]).toEqual([
        'claude',
        'PROXY_TOKEN',
        'new-secret',
        true,
      ]);
    });
  });

  it('masks a visible value in place and removes a variable', async () => {
    await seed({ claudeCustomEnv: [OPEN_VAR] });
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });

    await fireEvent.click(getByTestId('settings-provider-env-mask-ANTHROPIC_BASE_URL'));
    await waitFor(() => {
      expect(getBindingMock('SetProviderCustomEnvVar')?.mock.calls[0]).toEqual([
        'claude',
        'ANTHROPIC_BASE_URL',
        'https://gw.example.test',
        true,
      ]);
    });

    await fireEvent.click(getByTestId('settings-provider-env-remove-ANTHROPIC_BASE_URL'));
    await waitFor(() => {
      expect(getBindingMock('DeleteProviderCustomEnvVar')?.mock.calls[0]).toEqual([
        'claude',
        'ANTHROPIC_BASE_URL',
      ]);
    });
  });

  it('surfaces a backend rejection instead of leaving the row looking saved', async () => {
    await seed();
    setBindingMock('SetProviderCustomEnvVar', async () => {
      throw new Error('"CLAUDE_CONFIG_DIR" is reserved: Agent Overflow owns Claude\'s config home');
    });
    const { getByTestId } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    const name = getByTestId('settings-provider-env-name-input') as HTMLInputElement;
    await fireEvent.input(name, { target: { value: 'CLAUDE_CONFIG_DIR' } });
    await fireEvent.click(getByTestId('settings-provider-env-add'));

    // The draft survives a failure so the user can correct it.
    await waitFor(() => expect(name.value).toBe('CLAUDE_CONFIG_DIR'));
  });

  it('states that changes apply to new sessions', async () => {
    const { getByText } = render(ProviderCustomEnvSection, {
      props: { provider: CLAUDE },
    });
    expect(
      getByText(/Takes effect on the next session or account check\./),
    ).toBeTruthy();
  });
});
