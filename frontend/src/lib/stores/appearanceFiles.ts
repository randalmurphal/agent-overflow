// A desktop/controller has its own editable presentation directory. A phone
// or remote browser owns a durable copy, importing from a computer explicitly.
import { hasScope } from '../transport/scopes';
import { backendById, withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { GetSpinnerFiles, GetThemeFiles, ThemeFiles } from './bindings';
import {
  readFrontendAssets, saveFrontendAssets,
  type AssetKind, type AssetFiles,
} from './frontendAssets';

export function usesFrontendAssetLibrary(): boolean { return !hasScope('host'); }

const fetchers: { [K in AssetKind]: () => Promise<AssetFiles[K]> } = {
  themes: () => GetThemeFiles(),
  spinners: () => GetSpinnerFiles(),
};
const EMPTY: AssetFiles = { themes: { themes: [], warnings: [] }, spinners: { sprites: [], warnings: [] } };

function fetchFiles<K extends AssetKind>(kind: K, backend: BackendKey): Promise<AssetFiles[K]> {
  return withBackendTarget(backend, fetchers[kind]);
}

async function localFiles<K extends AssetKind>(kind: K): Promise<AssetFiles[K]> {
  const stored = await readFrontendAssets(kind);
  if (stored) return stored;
  // One-time migration of the directory older clients read from their first
  // host. Atomic insert-if-missing cannot overwrite a simultaneous explicit
  // import in another tab. New shells without a legacy home start empty.
  if (backendById(HOME_BACKEND) && hasScope('settings:read', HOME_BACKEND)) {
    return saveFrontendAssets(kind, await fetchFiles(kind, HOME_BACKEND), true);
  }
  return EMPTY[kind];
}

export async function readAppearanceFiles(): Promise<ThemeFiles> {
  return usesFrontendAssetLibrary()
    ? new ThemeFiles(await localFiles('themes'))
    : GetThemeFiles();
}

export async function readSpinnerFiles(): Promise<{ dir: string } & AssetFiles['spinners']> {
  return usesFrontendAssetLibrary()
    ? { ...await localFiles('spinners'), dir: '' }
    : GetSpinnerFiles();
}

/** Copies files only. It never adopts the source computer's preferences. */
export async function copyAppearanceFiles(kind: AssetKind, backend: BackendKey): Promise<void> {
  if (!usesFrontendAssetLibrary()) throw new Error('This frontend uses its own appearance directory.');
  await saveFrontendAssets(kind, await fetchFiles(kind, backend));
}
