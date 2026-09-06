import { loadSettingsFixture as loadSettings } from './settingsFixture';
import { resetKeybindingsStore } from '../../lib/stores/keybindings.svelte';
import { setBindingMock } from '../mocks/bindings-app';
import { makeSettings } from './settings';

export async function seedSettingsPages(): Promise<void> {
  const settings = makeSettings();
  setBindingMock('GetSettings', async () => settings);
  setBindingMock('UpdateSettings', async () => settings);
  setBindingMock('Version', async () => '0.0.1');
  setBindingMock('GetProviderStatuses', async () => []);
  setBindingMock('GetModelsForProvider', async () => []);
  setBindingMock('ListProviderAccounts', async () => []);
  setBindingMock('ListDiscussions', async () => []);
  setBindingMock('GetKeybindings', async () => ({ bindings: [] }));
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListArchivedThreads', async () => []);
  setBindingMock('GetThemeFiles', async () => ({
    dir: '/tmp/themes',
    themes: [],
    appearance: { mode: 'system', uiTheme: 'default', codeTheme: 'github' },
  }));
  setBindingMock('ListProjects', async () => [{ project: { id: 'settings-project', name: 'repo', path: '/repo', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false }, threadCount: 0 }]);
  setBindingMock('GetProjectWorktreeSetup', async () => ({ copy: [], run: [], timeout: '' }));
  setBindingMock('ListAvailableEditors', async () => []);
  setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
  setBindingMock('GetSpinnerFiles', async () => ({ dir: '/tmp/spinners', sprites: [], warnings: [] }));
  // The notifications page reads the phone-push status on mount.
  setBindingMock('GetPushSenderStatus', async () => ({
    configured: false,
    projectId: '',
    clientEmail: '',
    lastError: '',
    registeredDevices: 0,
  }));
  // Remote access: the whole persisted record plus the two derived
  // status blocks, because the page reads `tls.renewing` and
  // `tailnet.running` to decide whether to poll (network.Settings).
  setBindingMock('GetNetworkSettings', async () => ({
    bindAll: false,
    listenPort: 0,
    canonicalDomain: '',
    acmeDnsHook: [],
    externalCertFile: '',
    externalKeyFile: '',
    tailnetEnabled: false,
    tailnetControlUrl: '',
    tls: {
      serving: 'self-signed',
      notAfter: 0,
      renewing: false,
      lastError: '',
      selfSignedFingerprint: '',
    },
    tailnet: {
      running: false,
      state: '',
      authUrl: '',
      dnsName: '',
      ips: [],
      url: '',
      https: false,
      hasState: false,
      lastError: '',
    },
    url: 'http://127.0.0.1:1/?t=t',
    token: 't',
    insecure: false,
  }));
  setBindingMock('GetAccessOverview', async () => ({
    devices: [],
    pendingPairings: [],
    audit: [],
  }));
  setBindingMock('ListPasskeys', async () => []);
  setBindingMock('GetDevServers', async () => ({ previewHost: '', servers: [] }));
  setBindingMock('IsWSL', async () => false);
  setBindingMock('ListWSLDistros', async () => []);
  setBindingMock('GetWSLDistroPreference', async () => '');
  setBindingMock('ListBackends', async () => []);
  resetKeybindingsStore();
  await loadSettings();
}

