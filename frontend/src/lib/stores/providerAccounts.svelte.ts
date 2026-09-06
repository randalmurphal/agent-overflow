// Account surfaces share one owner per attached computer.
import { ComputerAccounts } from './computerAccounts.svelte';
import type { ProviderAccountActions, ProviderAccountGroup } from './computerAccounts.svelte';
export type { ProviderAccountActions, ProviderAccountGroup } from './computerAccounts.svelte';
import type { ManagedProviderAccount, ProviderLoginState } from './bindings';
import type { ProviderID } from '../types/providers';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { attachedBackends, onBackendsChanged } from '../transport/backends';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
export { providerLabel, providerAccountName, providerAccountOrgLabel, providerAccountActionLabel } from './providerAccountLabels';



const computers = createKeyedSignalRegistry<ComputerAccounts | null>(null);
const retained = new Map<BackendKey, ComputerAccounts>();
let EMPTY = new ComputerAccounts(HOME_BACKEND);
function syncComputers(): void {
  const live = new Set(attachedBackends().map((entry) => entry.id));
  live.add(HOME_BACKEND);
  for (const backend of live) {
    if (retained.has(backend)) continue;
    const value = new ComputerAccounts(backend);
    retained.set(backend, value);
    computers.set(backend, value);
  }
  for (const [backend, value] of retained) {
    if (live.has(backend)) continue;
    value.dispose();
    retained.delete(backend);
    computers.drop(backend);
  }
}
onBackendsChanged(syncComputers);
syncComputers();
function readComputer(backend: BackendKey): ComputerAccounts { return computers.get(backend) ?? EMPTY; }
function editComputer(backend: BackendKey): ComputerAccounts {
  const value = computers.get(backend);
  if (!value) throw new Error('This computer is no longer connected.');
  return value;
}
export function resetForTest(): void {
  for (const value of retained.values()) value.dispose();
  EMPTY.dispose();
  EMPTY = new ComputerAccounts(HOME_BACKEND);
  retained.clear();
  computers.reset();
  syncComputers();
}

export function getProviderAccountsFor(provider: ProviderID, backend: BackendKey = HOME_BACKEND): ManagedProviderAccount[] {
  return readComputer(backend).getProviderAccountsFor(provider);
}

export function getProviderAccountGroups(backend: BackendKey = HOME_BACKEND): ProviderAccountGroup[] {
  return readComputer(backend).getProviderAccountGroups();
}

export function isProviderAccountsLoading(backend: BackendKey = HOME_BACKEND): boolean {
  return readComputer(backend).isProviderAccountsLoading();
}

export function getProviderAccountActions(provider: ProviderID, backend: BackendKey = HOME_BACKEND): ProviderAccountActions {
  return readComputer(backend).getProviderAccountActions(provider);
}

export function isProviderCredentialOpInFlight(provider: ProviderID, backend: BackendKey = HOME_BACKEND): boolean {
  return readComputer(backend).isProviderCredentialOpInFlight(provider);
}

export function loadProviderAccounts(backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).loadProviderAccounts();
}

export function applyProviderAccountsChanged(backend: BackendKey = HOME_BACKEND): void {
  return editComputer(backend).applyProviderAccountsChanged();
}

export function switchProviderAccount(provider: ProviderID, account: ManagedProviderAccount, backend: BackendKey = HOME_BACKEND): Promise<boolean> {
  return editComputer(backend).switchProviderAccount(provider, account);
}

export function refreshProviderAccountUsage(provider: ProviderID, account: ManagedProviderAccount, backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).refreshProviderAccountUsage(provider, account);
}

export function removeProviderAccount(provider: ProviderID, account: ManagedProviderAccount, backend: BackendKey = HOME_BACKEND): Promise<boolean> {
  return editComputer(backend).removeProviderAccount(provider, account);
}

export function getProviderLogin(provider: ProviderID, backend: BackendKey = HOME_BACKEND): ProviderLoginState {
  return readComputer(backend).getProviderLogin(provider);
}

export function isProviderLoginActive(provider: ProviderID, backend: BackendKey = HOME_BACKEND): boolean {
  return readComputer(backend).isProviderLoginActive(provider);
}

export function applyProviderLogin(state: ProviderLoginState | null | undefined, backend: BackendKey = HOME_BACKEND): void {
  return editComputer(backend).applyProviderLogin(state);
}

export function hydrateProviderLogins(backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).hydrateProviderLogins();
}

export function startProviderLogin(provider: ProviderID, backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).startProviderLogin(provider);
}

export function submitProviderLoginCode(provider: ProviderID, code: string, backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).submitProviderLoginCode(provider, code);
}

export function cancelProviderLogin(provider: ProviderID, backend: BackendKey = HOME_BACKEND): Promise<void> {
  return editComputer(backend).cancelProviderLogin(provider);
}
