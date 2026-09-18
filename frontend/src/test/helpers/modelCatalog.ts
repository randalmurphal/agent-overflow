import type { CatalogProvenance, ModelCatalog, ModelInfo } from '../../lib/types/settings';

/**
 * A GetModelsForProvider answer. Tests default to an authoritative catalog so
 * a composer under test sees the list the way a probed account would.
 */
export function modelCatalog(models: ModelInfo[], provenance: CatalogProvenance = 'probed'): ModelCatalog {
  return { models, provenance };
}
