import { iterPanes } from './panes.svelte';
import { threadBackend } from '../transport/entityIndex';
import { resyncDraftsForBackend } from './eventsDraftRows';
import { resyncKeybindings } from './keybindings.svelte';
// Refresh computer-owned snapshots on reconnect and first attachment. Each
// read is pinned; one offline or ungranted computer cannot block the others.
import { refreshSidebarProjections } from './eventsThreadRows';
import type { BackendKey } from '../transport/backendKey';
import { HOME_BACKEND } from '../transport/backendKey';
import { isFrontendOnly } from '../transport/runMode';
import { attachedBackends } from '../transport/backends';
import { hasScope } from '../transport/scopes';
import { getTransportHelloFor, getTransportStatusFor, onBackendHelloChange, onBackendStatusChange } from './transportStatus.svelte';
import { loadSettings, getSettings, mirrorFrontendPreferences } from './settings.svelte';
import { preloadProviderModelsForSettings } from './providerModels.svelte';
import { loadProviderAccounts, hydrateProviderLogins } from './providerAccounts.svelte';
import { hydrateRateLimitsSnapshots } from './eventsRateLimits';
import { isWorkflowOverlayLoaded, refreshWorkflowRunsSoon, resyncWorkflowEngineState } from './workflowRuns.svelte';

export function installComputerHydration(): () => void {
  let stopped = false;
  let scheduled = false;
  const pending = new Set<BackendKey>();
  function schedule(backend: BackendKey): void {
    if (isFrontendOnly() && backend === HOME_BACKEND) return;
    pending.add(backend);
    if (scheduled) return;
    scheduled = true;
    queueMicrotask(() => {
      scheduled = false;
      if (stopped) return;
      const targets = [...pending];
      pending.clear();
      let refreshRows = false;
      for (const backend of targets) {
        if (getTransportStatusFor(backend).status !== 'connected' || !getTransportHelloFor(backend)) continue;
        refreshRows = true;
        hydrate(backend);
      }
      if (refreshRows) { refreshSidebarProjections(); refreshWorkflowRunsSoon(); }
    });
  }
  function hydrate(backend: BackendKey): void {
    mirrorFrontendPreferences(backend);
    resyncDraftsForBackend(backend);
    for (const pane of iterPanes()) {
      if (pane.threadId && threadBackend(pane.threadId) === backend) void pane.retryHistoryLoad();
    }
    if (backend === HOME_BACKEND && hasScope('settings:read', backend)) void resyncKeybindings();
    if (isWorkflowOverlayLoaded()) void resyncWorkflowEngineState(backend);
    // Boot's initial read can fail while a computer is offline. Its first
    // successful connection must hydrate it just like a later reconnect.
    void (async () => {
      if (!hasScope('settings:read', backend)) return;
      if (await loadSettings(backend)) {
        await preloadProviderModelsForSettings(getSettings(backend), backend);
      }
    })();
    if (hasScope('access:admin', backend)) {
      void loadProviderAccounts(backend);
      void hydrateProviderLogins(backend);
    }
    if (hasScope('threads:read', backend)) {
      void hydrateRateLimitsSnapshots(backend).catch((error) => {
        console.warn('Failed to refresh computer quotas:', error);
      });
    }
  }
  // Hello publication is intentionally deduplicated by content. A reconnect
  // to the same boot may repeat identical metadata, so connection edges are
  // independently authoritative. An initial hello can also arrive after open.
  const cancelStatus = onBackendStatusChange((backend, status) => { if (status.status === 'connected') schedule(backend); });
  const cancelHello = onBackendHelloChange((backend, hello) => { if (hello) schedule(backend); });
  // A frontend-only boot can open its local catalog before mounting the app.
  // Include computers that already completed their handshake in that interval.
  for (const entry of attachedBackends()) if (!entry.home) schedule(entry.id);
  return () => { stopped = true; pending.clear(); cancelStatus(); cancelHello(); };
}
